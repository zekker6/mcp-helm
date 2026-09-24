package helm_client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepositoryCacheDropsRequestGetter(t *testing.T) {
	var indexHits atomic.Int32
	archive := testChartArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			indexHits.Add(1)
			_, _ = w.Write(createTestIndex(""))
		} else {
			_, _ = w.Write(archive)
		}
	}))
	defer server.Close()
	client := newTestClient(t)
	client.options.repoIndexMaxAge = time.Hour
	type payloadKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), payloadKey{}, make([]byte, 1<<20)))
	defer cancel()
	if _, err := client.ListCharts(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	cancel()
	client.reposMu.Lock()
	snapshot := client.repos[server.URL].repo
	client.reposMu.Unlock()
	if snapshot.Client != nil {
		t.Error("cached repository retains its request-bound downloader")
	}
	values, err := client.GetChartValues(t.Context(), server.URL, "test-chart", "1.0.0")
	if err != nil || values != "replicas: 1\n" {
		t.Fatalf("warm-cache archive download: values=%q error=%v", values, err)
	}
	if got := indexHits.Load(); got != 1 {
		t.Errorf("index downloads = %d, want 1", got)
	}
	client.reposMu.Lock()
	client.repos[server.URL].fetched = time.Now().Add(-2 * client.options.repoIndexMaxAge)
	client.reposMu.Unlock()
	if _, err := client.ListCharts(t.Context(), server.URL); err != nil {
		t.Fatal(err)
	}
	client.reposMu.Lock()
	refreshed := client.repos[server.URL].repo
	client.reposMu.Unlock()
	if refreshed.Client != nil || refreshed == snapshot {
		t.Error("refresh did not publish a new downloader-free snapshot")
	}
	if got := indexHits.Load(); got != 2 {
		t.Errorf("index downloads after refresh = %d, want 2", got)
	}
}
