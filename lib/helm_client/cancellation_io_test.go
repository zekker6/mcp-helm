package helm_client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func stallResponse(w http.ResponseWriter, r *http.Request, body bool) {
	if body {
		prefix := "x"
		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			prefix = "{"
		}
		_, _ = w.Write([]byte(prefix))
		w.(http.Flusher).Flush()
	}
	select {
	case <-r.Context().Done():
	case <-time.After(500 * time.Millisecond):
	}
	_, _ = w.Write([]byte("finished"))
}

func assertCanceledPromptly(t *testing.T, started time.Time, err error) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("operation error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("operation took %s after a 60ms deadline", elapsed)
	}
}

func TestHTTPDownloadStopsDuringHeadersAndBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		inBody bool
	}{
		{name: "index headers", path: "/index.yaml"},
		{name: "index body", path: "/index.yaml", inBody: true},
		{name: "archive headers", path: "/charts/test-chart-1.0.0.tgz"},
		{name: "archive body", path: "/charts/test-chart-1.0.0.tgz", inBody: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var repoURL string
			archive := testChartArchive(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					stallResponse(w, r, tc.inBody)
					return
				}
				if r.URL.Path == "/index.yaml" {
					_, _ = w.Write(createTestIndex(repoURL))
				} else {
					_, _ = w.Write(archive)
				}
			}))
			defer server.Close()
			repoURL = server.URL
			client := newTestClient(t)
			if strings.HasPrefix(tc.name, "archive") {
				if _, err := client.ListChartVersions(context.Background(), repoURL, "test-chart"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			start := time.Now()
			if strings.HasPrefix(tc.name, "archive") {
				_, err := client.GetChartValues(ctx, repoURL, "test-chart", "1.0.0")
				assertCanceledPromptly(t, start, err)
			} else {
				_, err := client.ListChartVersions(ctx, repoURL, "test-chart")
				assertCanceledPromptly(t, start, err)
			}
		})
	}
}

func TestOCIStopsDuringTagsAndPull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		inBody bool
	}{
		{name: "tags headers", path: "/tags/list"},
		{name: "tags body", path: "/tags/list", inBody: true},
		{name: "pull headers", path: "/blobs/"},
		{name: "pull body", path: "/blobs/", inBody: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := buildOCIArtifact(t, "charts/"+matrixChart, matrixVersion, buildMatrixChartTGZ(t))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, tc.path) && r.Method == http.MethodGet {
					stallResponse(w, r, tc.inBody)
					return
				}
				artifact.ServeHTTP(w, r)
			}))
			defer server.Close()
			client, err := NewClient(WithPlainHTTP(true))
			if err != nil {
				t.Fatal(err)
			}
			url := "oci://" + strings.TrimPrefix(server.URL, "http://") + "/charts/" + matrixChart
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			start := time.Now()
			if strings.HasPrefix(tc.name, "pull") {
				_, err = client.GetChartValues(ctx, url, "", matrixVersion)
			} else {
				_, err = client.ListChartVersions(ctx, url, "")
			}
			assertCanceledPromptly(t, start, err)
		})
	}
}
