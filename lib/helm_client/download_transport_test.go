package helm_client

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPDownloadWrappedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = struct{ http.RoundTripper }{original}
	t.Cleanup(func() { http.DefaultTransport = original })

	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "custom CA"
		}
		t.Run(name, func(t *testing.T) {
			archive := testChartArchive(t)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/index.yaml" {
					_, _ = w.Write(createTestIndex(""))
				} else {
					_, _ = w.Write(archive)
				}
			})
			var server *httptest.Server
			var options []ClientOption
			if secure {
				server = httptest.NewTLSServer(handler)
				certificate := server.Certificate()
				path := filepath.Join(t.TempDir(), "ca.pem")
				if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
					t.Fatal(err)
				}
				options = append(options, WithCAFile(path))
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			client, err := NewClient(options...)
			if err != nil {
				t.Fatal(err)
			}
			values, err := client.GetChartValues(context.Background(), server.URL, "test-chart", "1.0.0")
			if err != nil || values != "replicas: 1\n" {
				t.Fatalf("values = %q, error = %v", values, err)
			}
		})
	}
}
