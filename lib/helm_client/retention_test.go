package helm_client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/repo/v1"
)

func TestRetentionDefaults(t *testing.T) {
	options := defaultClientOptions()
	for _, tt := range []struct {
		name string
		got  int64
		want int64
	}{
		{"repository entries", int64(options.repoCacheEntries), 16},
		{"repository bytes fallback", options.repoCacheBytes, 64 << 20},
		{"index bytes", options.indexMaxBytes, 32 << 20},
		{"chart bytes", options.chartMaxBytes, 100 << 20},
		{"OCI bytes", options.ociMaxBytes, 128 << 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("default = %d, want %d", tt.got, tt.want)
			}
		})
	}
}

func TestRetentionLRU(t *testing.T) {
	// Whitespace counts toward source bytes even though the parsed index is unchanged.
	index := append(createTestIndex(""), bytes.Repeat([]byte(" "), 1024)...)
	for _, limit := range []string{"entries", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			bodies := make(map[string][]byte)
			for _, name := range []string{"a", "b", "c", "d"} {
				bodies["/"+name+"/index.yaml"] = index
			}
			server, hits := newRetentionServer(t, bodies)
			client := newTestClient(t)
			client.options.repoIndexMaxAge = time.Hour
			if limit == "entries" {
				client.options.repoCacheEntries = 2
			} else {
				client.options.repoCacheEntries = 16
				client.options.repoCacheBytes = 2 * int64(len(index))
			}

			for _, step := range []struct {
				name string
				want []string
			}{
				{"a", []string{"a"}},
				{"b", []string{"a", "b"}},
				{"a", []string{"a", "b"}},
				{"c", []string{"a", "c"}},
				{"d", []string{"c", "d"}},
				{"b", []string{"b", "d"}},
			} {
				getRetentionRepo(t, client, server.URL+"/"+step.name)
				assertRetentionCache(t, client, server.URL, step.want, int64(len(index)))
			}
			for name, want := range map[string]int{"a": 1, "b": 2, "c": 1, "d": 1} {
				if got := hits("/" + name + "/index.yaml"); got != want {
					t.Errorf("%s downloads = %d, want %d", name, got, want)
				}
			}
		})
	}
}

func TestRetentionBytesEvictsMultipleEntries(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	index := createTestIndex("")
	large := append(bytes.Clone(index), bytes.Repeat([]byte(" "), 2*len(index))...)
	server, _ := newRetentionServer(t, map[string][]byte{
		"/a/index.yaml": index,
		"/b/index.yaml": index,
		"/c/index.yaml": large,
	})
	client := newTestClient(t)
	client.options.repoCacheEntries = 16
	client.options.repoCacheBytes = int64(len(large))
	for _, name := range []string{"a", "b"} {
		getRetentionRepo(t, client, server.URL+"/"+name)
	}
	assertRetentionCache(t, client, server.URL, []string{"a", "b"}, int64(len(index)))
	getRetentionRepo(t, client, server.URL+"/c")
	assertRetentionCache(t, client, server.URL, []string{"c"}, int64(len(large)))
}

func TestRetentionOversizedIndexNotCached(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	index := createTestIndex("")
	server, hits := newRetentionServer(t, map[string][]byte{"/index.yaml": index})
	client := newTestClient(t)
	client.options.repoCacheBytes = int64(len(index) - 1)
	client.options.indexMaxBytes = int64(len(index) + 1)
	for range 2 {
		snapshot := getRetentionRepo(t, client, server.URL)
		assertRetentionVersion(t, snapshot, "1.0.0")
		client.reposMu.Lock()
		count := len(client.repos)
		client.reposMu.Unlock()
		if count != 0 {
			t.Fatalf("oversized index retained: %d cached repositories", count)
		}
	}
	if got := hits("/index.yaml"); got != 2 {
		t.Errorf("index downloads = %d, want 2", got)
	}
}

func TestRetentionSnapshotsSurviveEvictionAndRefresh(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	first := createTestIndex("")
	second := bytes.ReplaceAll(first, []byte("1.0.0"), []byte("2.0.0"))
	third := bytes.ReplaceAll(first, []byte("1.0.0"), []byte("3.0.0"))
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := first
		if r.URL.Path == "/a/index.yaml" {
			switch hits.Add(1) {
			case 1:
			case 2:
				body = second
			default:
				body = third
			}
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write index: %v", err)
		}
	}))
	defer server.Close()
	client := newTestClient(t)
	client.options.repoCacheEntries = 1
	url := server.URL + "/a"
	original := getRetentionRepo(t, client, url)
	getRetentionRepo(t, client, server.URL+"/b")
	assertRetentionCache(t, client, server.URL, []string{"b"}, int64(len(first)))
	assertRetentionVersion(t, original, "1.0.0")

	reloaded := getRetentionRepo(t, client, url)
	assertRetentionVersion(t, reloaded, "2.0.0")
	assertRetentionVersion(t, original, "1.0.0")

	client.reposMu.Lock()
	client.repos[url].fetched = time.Now().Add(-2 * client.options.repoIndexMaxAge)
	client.reposMu.Unlock()
	refreshed := getRetentionRepo(t, client, url)
	if refreshed == reloaded || refreshed.IndexFile == reloaded.IndexFile {
		t.Fatal("refresh reused the held repository snapshot")
	}
	assertRetentionVersion(t, original, "1.0.0")
	assertRetentionVersion(t, reloaded, "2.0.0")
	assertRetentionVersion(t, refreshed, "3.0.0")
}

func TestRetentionTemporaryIndexCleanup(t *testing.T) {
	index := createTestIndex("")
	for _, tt := range []struct {
		name    string
		body    []byte
		limit   int64
		wantErr bool
	}{
		{name: "valid index", body: index},
		{name: "invalid index", body: []byte("apiVersion: ["), wantErr: true},
		{name: "download failure", wantErr: true},
		{name: "index exceeds download limit", body: index, limit: int64(len(index) - 1), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			sentinel := filepath.Join(root, "unrelated-index.yaml")
			if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			bodies := make(map[string][]byte)
			if tt.body != nil {
				bodies["/index.yaml"] = tt.body
			}
			server, _ := newRetentionServer(t, bodies)
			seenPaths := make(map[string]bool)
			for range 2 {
				client := newTestClient(t)
				if tt.limit != 0 {
					client.options.indexMaxBytes = tt.limit
				}
				for attempt := range 2 {
					if attempt == 1 {
						client.options.repoIndexMaxAge = 0
					}
					snapshot, err := client.getRepo(context.Background(), server.URL, server.URL)
					if (err != nil) != tt.wantErr {
						t.Fatalf("getRepo() error = %v, want error %v", err, tt.wantErr)
					}
					if err == nil {
						rel, err := filepath.Rel(root, snapshot.CachePath)
						if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
							t.Fatalf("CachePath = %q, want owned directory under %q", snapshot.CachePath, root)
						}
						if seenPaths[snapshot.CachePath] {
							t.Errorf("temporary directory reused: %q", snapshot.CachePath)
						}
						seenPaths[snapshot.CachePath] = true
						if _, err := os.Stat(snapshot.CachePath); !os.IsNotExist(err) {
							t.Errorf("temporary cache still exists: %q, stat error = %v", snapshot.CachePath, err)
						}
						assertRetentionVersion(t, snapshot, "1.0.0")
					}
					entries, err := os.ReadDir(root)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 1 || entries[0].Name() != filepath.Base(sentinel) {
						t.Fatalf("remaining temporary directory entries = %v, want only sentinel", entries)
					}
					data, err := os.ReadFile(sentinel)
					if err != nil || string(data) != "keep me" {
						t.Fatalf("sentinel changed: data = %q, error = %v", data, err)
					}
				}
			}
		})
	}
}

func newRetentionServer(t *testing.T, bodies map[string][]byte) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write index: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[path]
	}
}

func getRetentionRepo(t *testing.T, client *HelmClient, url string) *repo.ChartRepository {
	t.Helper()
	snapshot, err := client.getRepo(context.Background(), url, url)
	if err != nil {
		t.Fatalf("getRepo(%q): %v", url, err)
	}
	return snapshot
}

func assertRetentionVersion(t *testing.T, snapshot *repo.ChartRepository, want string) {
	t.Helper()
	if snapshot == nil || snapshot.IndexFile == nil {
		t.Fatal("repository snapshot has no index")
	}
	versions := snapshot.IndexFile.Entries["test-chart"]
	if len(versions) != 1 || versions[0].Version != want {
		t.Fatalf("snapshot versions = %v, want %s", versions, want)
	}
}

func assertRetentionCache(t *testing.T, client *HelmClient, baseURL string, want []string, size int64) {
	t.Helper()
	client.reposMu.Lock()
	defer client.reposMu.Unlock()
	var names []string
	var total int64
	for url, entry := range client.repos {
		names = append(names, strings.TrimPrefix(url, baseURL+"/"))
		total += entry.size
		if entry.size != size {
			t.Errorf("%s source size = %d, want %d", url, entry.size, size)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("cached repositories = %v, want %v", names, want)
	}
	if total > client.options.repoCacheBytes || len(names) > client.options.repoCacheEntries {
		t.Fatalf("cache exceeds limits: %d bytes in %d entries", total, len(names))
	}
}
