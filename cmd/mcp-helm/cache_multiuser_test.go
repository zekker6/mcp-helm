package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"

	"github.com/zekker6/mcp-helm/lib/helm_client"
)

// Each repository uses the same chart name but distinct versions and values,
// so a cache key collision cannot pass as a successful tool call.
func TestSharedRepositoryCacheMultiuser(t *testing.T) {
	for _, entries := range []int{16, 2} {
		t.Run(fmt.Sprintf("entries_%d", entries), func(t *testing.T) {
			for name, value := range map[string]int{
				"MCP_HELM_REPO_CACHE_MAX_ENTRIES": entries,
				"MCP_HELM_REPO_CACHE_MAX_BYTES":   64 << 20,
				"MCP_HELM_INDEX_MAX_BYTES":        32 << 20,
				"MCP_HELM_CHART_MAX_BYTES":        100 << 20,
				"MCP_HELM_OCI_MAX_BYTES":          128 << 20,
			} {
				t.Setenv(name, strconv.Itoa(value))
			}

			repos := serveMultiuserRepositories(t)
			clients := newMultiuserClients(t, 8)
			started := time.Now()
			runMultiuserRounds(t, clients, repos, 1)
			var coldFetches int64
			for _, repo := range repos {
				coldFetches += repo.indexFetches.Load()
				if entries >= len(repos) && repo.indexFetches.Load() != 1 {
					t.Errorf("cold %s index fetches = %d, want 1", repo.url, repo.indexFetches.Load())
				}
			}
			runMultiuserRounds(t, clients, repos, 9)

			var indexFetches, archiveFetches int64
			for _, repo := range repos {
				indexes, archives := repo.indexFetches.Load(), repo.archiveFetches.Load()
				indexFetches += indexes
				archiveFetches += archives
				if entries >= len(repos) && indexes != 1 {
					t.Errorf("warm %s index fetches = %d, want 1", repo.url, indexes)
				}
				// Archives are not cached: every values request downloads one.
				if want := int64(len(clients) * 10); archives != want {
					t.Errorf("%s archive fetches = %d, want %d", repo.url, archives, want)
				}
			}
			if entries < len(repos) && indexFetches <= coldFetches {
				t.Errorf("eviction caused no refetches: cold = %d, final = %d", coldFetches, indexFetches)
			}
			t.Logf("%d clients, 10 rounds, %d repositories: %s, index fetches %d, archive fetches %d",
				len(clients), len(repos), time.Since(started), indexFetches, archiveFetches)
		})
	}
}

type multiuserRepository struct {
	url            string
	version        string
	values         string
	indexFetches   atomic.Int64
	archiveFetches atomic.Int64
}

func serveMultiuserRepositories(t *testing.T) []*multiuserRepository {
	t.Helper()

	mux := http.NewServeMux()
	repos := make([]*multiuserRepository, 6)
	for i := range repos {
		repo := &multiuserRepository{
			version: fmt.Sprintf("1.0.%d", i),
			values:  fmt.Sprintf("repository: repo-%d\n", i),
		}
		repos[i] = repo
		prefix := fmt.Sprintf("/repo-%d", i)
		archivePath, err := chartutil.Save(&chartv2.Chart{
			Metadata: &chartv2.Metadata{APIVersion: "v2", Name: "shared-chart", Version: repo.version},
			Raw:      []*common.File{{Name: "values.yaml", Data: []byte(repo.values)}},
		}, t.TempDir())
		if err != nil {
			t.Fatalf("save chart: %v", err)
		}
		archive, err := os.ReadFile(archivePath)
		if err != nil {
			t.Fatalf("read chart archive: %v", err)
		}
		index := fmt.Sprintf("apiVersion: v1\nentries:\n  shared-chart:\n    - apiVersion: v2\n      name: shared-chart\n      version: %s\n      urls:\n        - shared-chart.tgz\n", repo.version)
		mux.HandleFunc(prefix+"/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
			repo.indexFetches.Add(1)
			if _, err := fmt.Fprint(w, index); err != nil {
				t.Errorf("serve index: %v", err)
			}
		})
		mux.HandleFunc(prefix+"/shared-chart.tgz", func(w http.ResponseWriter, _ *http.Request) {
			repo.archiveFetches.Add(1)
			if _, err := w.Write(archive); err != nil {
				t.Errorf("serve archive: %v", err)
			}
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for i, repo := range repos {
		repo.url = fmt.Sprintf("%s/repo-%d", srv.URL, i)
	}
	return repos
}

func newMultiuserClients(t *testing.T, count int) []*client.Client {
	t.Helper()

	helmClient, err := helm_client.NewClient()
	if err != nil {
		t.Fatalf("create shared Helm client: %v", err)
	}
	srv := &http.Server{} //nolint:gosec // Matches the production transport setup.
	transport := newStreamableHTTPTransport(srv, buildServer(helmClient), false)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := transport.Shutdown(ctx); err != nil {
			t.Errorf("shut down streamable transport: %v", err)
		}
	})
	httpSrv := httptest.NewServer(srv.Handler)
	t.Cleanup(httpSrv.Close)

	clients := make([]*client.Client, count)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range clients {
		c, err := client.NewStreamableHttpClient(httpSrv.URL + streamableEndpointPath)
		if err != nil {
			t.Fatalf("create MCP client %d: %v", i, err)
		}
		t.Cleanup(func() {
			if err := c.Close(); err != nil {
				t.Errorf("close MCP client: %v", err)
			}
		})
		if err := c.Start(ctx); err != nil {
			t.Fatalf("start MCP client %d: %v", i, err)
		}
		if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: fmt.Sprintf("cache-user-%d", i), Version: "1.0.0"},
		}}); err != nil {
			t.Fatalf("initialize MCP client %d: %v", i, err)
		}
		clients[i] = c
	}
	return clients
}

func runMultiuserRounds(t *testing.T, clients []*client.Client, repos []*multiuserRepository, rounds int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for user, c := range clients {
		wg.Go(func() {
			<-start
			for range rounds {
				for i := range repos {
					repo := repos[(i+user)%len(repos)]
					for _, tool := range []string{"list_chart_versions", "get_chart_values"} {
						result, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
							Name: tool,
							Arguments: map[string]any{
								"repository_url": repo.url,
								"chart_name":     "shared-chart",
							},
						}})
						if err != nil {
							t.Errorf("user %d %s %s: %v", user, tool, repo.url, err)
							return
						}
						if result.IsError || len(result.Content) != 1 {
							t.Errorf("user %d %s %s: unexpected result %+v", user, tool, repo.url, result)
							return
						}
						text, ok := result.Content[0].(mcp.TextContent)
						if !ok {
							t.Errorf("%s returned %T, want text", tool, result.Content[0])
							return
						}
						got, want := text.Text, repo.version
						if tool == "get_chart_values" {
							want = repo.values
							if err := json.Unmarshal([]byte(text.Text), &got); err != nil {
								t.Errorf("decode values: %v", err)
								return
							}
						}
						if got != want {
							t.Errorf("user %d %s %s = %q, want %q", user, tool, repo.url, got, want)
							return
						}
					}
				}
			}
		})
	}
	close(start)
	wg.Wait()
}
