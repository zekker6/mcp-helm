package helm_client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestHTTPDownloadLimits(t *testing.T) {
	for _, target := range []string{"index", "archive"} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chunked=%t", target, chunked), func(t *testing.T) {
				var hits atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if target == "archive" && r.URL.Path == "/index.yaml" {
						_, _ = w.Write(createTestIndex(""))
						return
					}
					hits.Add(1)
					if chunked {
						w.(http.Flusher).Flush()
					}
					_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
				}))
				defer server.Close()
				client := newTestClient(t)
				client.options.indexMaxBytes = 128
				client.options.chartMaxBytes = 128
				var err error
				if target == "index" {
					_, err = client.ListCharts(context.Background(), server.URL)
				} else {
					client.options.indexMaxBytes = 4096
					_, err = client.GetChartValues(context.Background(), server.URL, "test-chart", "1.0.0")
				}
				if err == nil || !strings.Contains(err.Error(), "byte limit") {
					t.Fatalf("expected byte limit error, got %v", err)
				}
				if hits.Load() != 1 {
					t.Fatalf("oversized download retried: %d requests", hits.Load())
				}
			})
		}
	}
}

func TestOCIPullOversizedManifest(t *testing.T) {
	host := startOCIRegistry(t, "user", "pass", buildMatrixChartTGZ(t))
	client, err := NewClient(WithPlainHTTP(true), WithBasicAuth("user", "pass"))
	if err != nil {
		t.Fatal(err)
	}
	client.options.ociMaxBytes = 128
	_, err = client.GetChartValues(context.Background(), "oci://"+host+"/charts/"+matrixChart, "", matrixVersion)
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("expected OCI byte limit error, got %v", err)
	}
}

func TestOCIPullAggregateLimit(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%t", chunked), func(t *testing.T) {
			artifact := buildOCIArtifact(t, "charts/"+matrixChart, matrixVersion, buildMatrixChartTGZ(t))
			var mu sync.Mutex
			var total, largest int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec := httptest.NewRecorder()
				artifact.ServeHTTP(rec, r)
				if r.Method == http.MethodGet {
					n := int64(rec.Body.Len())
					mu.Lock()
					total += n
					largest = max(largest, n)
					mu.Unlock()
				}
				for key, values := range rec.Header() {
					w.Header()[key] = values
				}
				if chunked && r.Method != http.MethodHead {
					w.Header().Del("Content-Length")
					w.WriteHeader(rec.Code)
					w.(http.Flusher).Flush()
				} else {
					w.WriteHeader(rec.Code)
				}
				_, _ = w.Write(rec.Body.Bytes())
			}))
			defer server.Close()
			client, err := NewClient(WithPlainHTTP(true))
			if err != nil {
				t.Fatal(err)
			}
			repoURL := "oci://" + strings.TrimPrefix(server.URL, "http://") + "/charts/" + matrixChart
			pull := func() error {
				_, err := client.GetChartValues(context.Background(), repoURL, "", matrixVersion)
				return err
			}
			if err := pull(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			totalBytes, largestBody := total, largest
			mu.Unlock()
			if totalBytes <= largestBody {
				t.Fatalf("fixture must contain multiple responses: total=%d largest=%d", totalBytes, largestBody)
			}
			client.options.ociMaxBytes = largestBody
			if err := pull(); err == nil || !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("each response fits, but total must fail: %v", err)
			}
			// Each pull gets its own budget, including when they run together.
			client.options.ociMaxBytes = totalBytes
			results := make(chan error, 2)
			for range 2 {
				go func() { results <- pull() }()
			}
			for range 2 {
				if err := <-results; err != nil {
					t.Fatalf("independent exact-size pull failed: %v", err)
				}
			}
		})
	}
}

type measuredDownloadBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *measuredDownloadBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *measuredDownloadBody) Close() error {
	b.closed = true
	return nil
}

func TestBudgetBody(t *testing.T) {
	for _, size := range []int{7, 8, 9, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			source := &measuredDownloadBody{Reader: strings.NewReader(strings.Repeat("x", size))}
			body := &budgetBody{ReadCloser: source, budget: newByteBudget(8)}
			data, err := io.ReadAll(body)
			if size > 8 {
				if err == nil || !strings.Contains(err.Error(), "byte limit") {
					t.Fatalf("expected limit error, got %v", err)
				}
				if source.read > 9 || len(data) > 8 {
					t.Fatalf("read=%d buffered=%d beyond budget", source.read, len(data))
				}
				_, _ = body.Read(make([]byte, 10))
				if source.read > 9 {
					t.Fatal("continued consuming an oversized body")
				}
			} else if err != nil || len(data) != size {
				t.Fatalf("bounded input: bytes=%d err=%v", len(data), err)
			}
			if err := body.Close(); err != nil || !source.closed {
				t.Fatalf("body not closed: %v", err)
			}
		})
	}
}

func TestBudgetBodyPreservesReadError(t *testing.T) {
	source := &measuredDownloadBody{Reader: failingDownloadReader{}}
	_, err := io.ReadAll(&budgetBody{ReadCloser: source, budget: newByteBudget(8)})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body error lost: %v", err)
	}
}

type failingDownloadReader struct{}

func (failingDownloadReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}
