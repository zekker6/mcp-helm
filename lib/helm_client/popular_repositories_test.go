package helm_client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/repo/v1"
)

type popularRepoSnapshot struct {
	name  string
	url   string
	chart string
	index []byte
	hits  atomic.Int64
}

// TestPopularRepositories fetches each public index once, then replays those
// exact bytes locally for concurrent cache tests. Only one representative chart
// per repository is pulled upstream; warm-load loops never hit public hosts.
func TestPopularRepositories(t *testing.T) {
	if testing.Short() {
		t.Skip("public repository validation requires task test:repos")
	}
	fixtures := []*popularRepoSnapshot{
		{name: "prometheus", url: "https://prometheus-community.github.io/helm-charts", chart: "prometheus"},
		{name: "grafana", url: "https://grafana.github.io/helm-charts", chart: "grafana"},
		{name: "bitnami", url: "https://charts.bitnami.com/bitnami", chart: "nginx"},
		{name: "jetstack", url: "https://charts.jetstack.io", chart: "cert-manager"},
		{name: "argo", url: "https://argoproj.github.io/argo-helm", chart: "argo-cd"},
		{name: "ingress-nginx", url: "https://kubernetes.github.io/ingress-nginx", chart: "ingress-nginx"},
	}
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var totalBytes int64
	allDownloaded := true
	for _, fixture := range fixtures {
		ok := t.Run(fixture.name+"/index", func(t *testing.T) {
			g, err := client.newHTTPGetter(ctx, fixture.url, client.options.indexMaxBytes)
			if err != nil {
				t.Fatal(err)
			}
			indexURL, err := repo.ResolveReferenceURL(fixture.url, "index.yaml")
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			data, err := g.Get(indexURL)
			if err != nil {
				t.Fatalf("public index %s: %v", indexURL, err)
			}
			fixture.index = data.Bytes()
			totalBytes += int64(data.Len())
			t.Logf("index_bytes=%d download=%s", data.Len(), time.Since(start).Round(time.Millisecond))
		})
		allDownloaded = allDownloaded && ok
	}
	if !allDownloaded {
		t.Log("concurrent replay not run because the complete public working set could not be fetched")
		return
	}
	t.Logf("working_set: repos=%d source_bytes=%d cache_entries=%d cache_bytes=%d", len(fixtures), totalBytes,
		client.options.repoCacheEntries, client.options.repoCacheBytes)
	if len(fixtures) > client.options.repoCacheEntries || totalBytes > client.options.repoCacheBytes {
		t.Fatalf("popular repository working set does not fit cache; increase MCP_HELM_REPO_CACHE_MAX_ENTRIES or MCP_HELM_REPO_CACHE_MAX_BYTES")
	}

	byName := make(map[string]*popularRepoSnapshot, len(fixtures))
	for _, fixture := range fixtures {
		byName[fixture.name] = fixture
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, suffix, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		fixture := byName[name]
		if fixture == nil {
			http.NotFound(w, r)
			return
		}
		if suffix == "index.yaml" {
			fixture.hits.Add(1)
			if _, err := w.Write(fixture.index); err != nil {
				t.Errorf("serve snapshot: %v", err)
			}
			return
		}
		// Relative archive references still fetch the real upstream chart.
		if r.URL.RawQuery != "" {
			suffix += "?" + r.URL.RawQuery
		}
		target, err := repo.ResolveReferenceURL(fixture.url, suffix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	readVersions := func(worker, round int) error {
		fixture := fixtures[(worker+round)%len(fixtures)]
		versions, err := client.ListChartVersions(ctx, server.URL+"/"+fixture.name, fixture.chart)
		if err != nil {
			return fmt.Errorf("%s: %w", fixture.name, err)
		}
		if len(versions) == 0 {
			return fmt.Errorf("%s: no versions for %s", fixture.name, fixture.chart)
		}
		return nil
	}
	t.Run("cold_readers", func(t *testing.T) { runPopularReaders(t, 8, 1, readVersions) })
	for _, fixture := range fixtures {
		var ociRef string
		t.Run(fixture.name+"/chart", func(t *testing.T) {
			url := server.URL + "/" + fixture.name
			version, err := client.GetChartLatestVersion(ctx, url, fixture.chart)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := client.getRepo(ctx, url, url)
			if err != nil {
				t.Fatal(err)
			}
			chart, err := snapshot.IndexFile.Get(fixture.chart, version)
			if err != nil {
				t.Fatal(err)
			}
			if len(chart.URLs) > 0 && IsOCI(chart.URLs[0]) {
				ociRef = chart.URLs[0]
			}
			start := time.Now()
			values, err := client.GetChartValues(ctx, url, fixture.chart, version)
			if err != nil {
				t.Fatalf("public chart %s/%s %s: %v", fixture.name, fixture.chart, version, err)
			}
			if strings.TrimSpace(values) == "" {
				t.Fatal("empty chart values")
			}
			t.Logf("version=%s values_bytes=%d pull=%s", version, len(values), time.Since(start).Round(time.Millisecond))
		})
		if ociRef != "" {
			t.Run(fixture.name+"/direct_oci", func(t *testing.T) {
				start := time.Now()
				values, err := client.GetChartValues(ctx, ociRef, "", "")
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(values) == "" {
					t.Fatal("empty OCI chart values")
				}
				t.Logf("ref=%s values_bytes=%d pull=%s", ociRef, len(values), time.Since(start).Round(time.Millisecond))
			})
		}
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	t.Run("warm_readers", func(t *testing.T) { runPopularReaders(t, 8, 25, readVersions) })
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("quiescent_go_heap_bytes: before_warm=%d after_warm=%d; includes captured index fixtures, not process RSS", before.HeapAlloc, after.HeapAlloc)
	for _, fixture := range fixtures {
		if got := fixture.hits.Load(); got != 1 {
			t.Errorf("%s: index fetches=%d, want 1 across cold and warm rounds", fixture.name, got)
		}
	}
	client.reposMu.Lock()
	defer client.reposMu.Unlock()
	var retained int64
	for _, entry := range client.repos {
		retained += entry.size
	}
	t.Logf("retained_indexes=%d retained_source_bytes=%d", len(client.repos), retained)
	if len(client.repos) != len(fixtures) || retained != totalBytes {
		t.Errorf("cache did not retain the complete working set: indexes=%d bytes=%d", len(client.repos), retained)
	}
}

func runPopularReaders(t *testing.T, workers, rounds int, request func(int, int) error) {
	t.Helper()
	type result struct {
		elapsed time.Duration
		err     error
	}
	results := make(chan result, workers*rounds)
	start := make(chan struct{})
	for worker := range workers {
		go func() {
			<-start
			for round := range rounds {
				before := time.Now()
				err := request(worker, round)
				results <- result{elapsed: time.Since(before), err: err}
			}
		}()
	}
	before := time.Now()
	close(start)
	latencies := make([]time.Duration, 0, workers*rounds)
	failures := 0
	for range workers * rounds {
		result := <-results
		latencies = append(latencies, result.elapsed)
		if result.err != nil {
			failures++
			t.Errorf("concurrent request: %v", result.err)
		}
	}
	slices.Sort(latencies)
	t.Logf("workers=%d requests=%d failures=%d elapsed=%s p50=%s p95=%s max=%s", workers, len(latencies), failures,
		time.Since(before).Round(time.Millisecond), latencies[len(latencies)/2].Round(time.Microsecond),
		latencies[(len(latencies)-1)*95/100].Round(time.Microsecond), latencies[len(latencies)-1].Round(time.Microsecond))
}
