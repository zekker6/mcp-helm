package helm_client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestIsRetryable(t *testing.T) {
	urlErr := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://charts.example.com/index.yaml", Err: err}
	}

	tests := []struct {
		name   string
		status int
		err    error
		want   bool
	}{
		{name: "503", status: http.StatusServiceUnavailable, want: true},
		{name: "429", status: http.StatusTooManyRequests, want: true},
		{name: "408", status: http.StatusRequestTimeout, want: true},
		{name: "404", status: http.StatusNotFound, want: false},
		{name: "401", status: http.StatusUnauthorized, want: false},
		{
			name: "connection reset",
			err:  urlErr(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}),
			want: true,
		},
		{
			name: "connection refused",
			err:  urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}),
			want: true,
		},
		{name: "unexpected EOF", err: urlErr(io.ErrUnexpectedEOF), want: true},
		{name: "dial timeout", err: urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}), want: true},
		{
			name: "temporary DNS failure",
			err:  urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "server misbehaving", Name: "charts.example.com", IsTemporary: true}}),
			want: true,
		},
		{
			name: "unknown host",
			err:  urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "charts.example.invalid", IsNotFound: true}}),
			want: false,
		},
		{name: "TLS alert", err: urlErr(&net.OpError{Op: "remote error", Err: tls.AlertError(42)}), want: false},
		{name: "certificate verification failure", err: urlErr(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), want: false},
		{name: "client timeout", err: urlErr(context.DeadlineExceeded), want: false},
		{name: "canceled dial", err: urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}), want: false},
		{name: "non-network error", err: errors.New("scheme \"ftp\" not supported"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp *http.Response
			if tt.status != 0 {
				resp = &http.Response{StatusCode: tt.status}
			}
			got, err := isRetryable(resp, tt.err)
			if err != nil {
				t.Fatalf("isRetryable() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("isRetryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// dropConnection closes the connection without writing a response, simulating
// a one-off network glitch.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack connection: %v", err)
		return
	}
	_ = conn.Close()
}

func respondStatus(code int) func(*testing.T, http.ResponseWriter) {
	return func(_ *testing.T, w http.ResponseWriter) {
		w.WriteHeader(code)
	}
}

func testChartArchive(t *testing.T) []byte {
	t.Helper()

	files := []struct{ name, content string }{
		{"test-chart/Chart.yaml", "apiVersion: v2\nname: test-chart\nversion: 1.0.0\n"},
		{"test-chart/values.yaml", "replicas: 1\n"},
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.content))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(f.content)); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func TestHTTPRepoRetry(t *testing.T) {
	tests := []struct {
		name     string
		failPath string
		fail     func(*testing.T, http.ResponseWriter)
		wantErr  bool
		wantHits int32
	}{
		{
			name:     "dropped index connection is retried",
			failPath: "/index.yaml",
			fail:     dropConnection,
			wantHits: 2,
		},
		{
			name:     "index 503 is retried",
			failPath: "/index.yaml",
			fail:     respondStatus(http.StatusServiceUnavailable),
			wantHits: 2,
		},
		{
			name:     "dropped chart download connection is retried",
			failPath: "/charts/test-chart-1.0.0.tgz",
			fail:     dropConnection,
			wantHits: 2,
		},
		{
			name:     "index 404 is not retried",
			failPath: "/index.yaml",
			fail:     respondStatus(http.StatusNotFound),
			wantErr:  true,
			wantHits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := testChartArchive(t)

			var (
				hits      atomic.Int32
				serverURL string
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tt.failPath && hits.Add(1) == 1 {
					tt.fail(t, w)
					return
				}
				switch r.URL.Path {
				case "/index.yaml":
					_, _ = w.Write(createTestIndex(serverURL))
				case "/charts/test-chart-1.0.0.tgz":
					_, _ = w.Write(archive)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			serverURL = server.URL

			client := newTestClient(t)
			values, err := client.GetChartValues(server.URL, "test-chart", "1.0.0")
			if tt.wantErr {
				if err == nil {
					t.Fatal("GetChartValues() expected error")
				}
			} else {
				if err != nil {
					t.Fatalf("GetChartValues() error = %v", err)
				}
				if values != "replicas: 1\n" {
					t.Errorf("GetChartValues() = %q, want %q", values, "replicas: 1\n")
				}
			}

			if got := hits.Load(); got != tt.wantHits {
				t.Errorf("requests to %s = %d, want %d", tt.failPath, got, tt.wantHits)
			}
		})
	}
}

func TestOCIRegistryRetry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/org/test-chart/tags/list" {
			http.NotFound(w, r)
			return
		}
		if hits.Add(1) == 1 {
			dropConnection(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "org/test-chart", "tags": []string{"1.0.0"}})
	}))
	defer server.Close()

	client, err := NewClient(WithPlainHTTP(true))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	repoURL := "oci://" + strings.TrimPrefix(server.URL, "http://") + "/org/test-chart"
	versions, err := client.ListChartVersions(repoURL, "")
	if err != nil {
		t.Fatalf("ListChartVersions() error = %v", err)
	}
	if !slices.Equal(versions, []string{"1.0.0"}) {
		t.Errorf("ListChartVersions() = %v, want [1.0.0]", versions)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("tags requests = %d, want 2", got)
	}
}
