package helm_client

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

var limitEnvNames = []string{
	"MCP_HELM_REPO_CACHE_MAX_ENTRIES",
	"MCP_HELM_REPO_CACHE_MAX_BYTES",
	"MCP_HELM_INDEX_MAX_BYTES",
	"MCP_HELM_CHART_MAX_BYTES",
	"MCP_HELM_OCI_MAX_BYTES",
}

func clearLimitEnv(t *testing.T) {
	t.Helper()
	for _, name := range limitEnvNames {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResourceLimitEnv(t *testing.T) {
	clearLimitEnv(t)
	for _, tt := range []struct {
		name string
		get  func(*clientOptions) int64
	}{
		{"MCP_HELM_REPO_CACHE_MAX_ENTRIES", func(o *clientOptions) int64 { return int64(o.repoCacheEntries) }},
		{"MCP_HELM_REPO_CACHE_MAX_BYTES", func(o *clientOptions) int64 { return o.repoCacheBytes }},
		{"MCP_HELM_INDEX_MAX_BYTES", func(o *clientOptions) int64 { return o.indexMaxBytes }},
		{"MCP_HELM_CHART_MAX_BYTES", func(o *clientOptions) int64 { return o.chartMaxBytes }},
		{"MCP_HELM_OCI_MAX_BYTES", func(o *clientOptions) int64 { return o.ociMaxBytes }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, value := range []int64{1, 4096} {
				t.Run(fmt.Sprint(value), func(t *testing.T) {
					t.Setenv(tt.name, fmt.Sprint(value))
					client, err := NewClient(WithRepoIndexMaxAge(2 * time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					if got := tt.get(client.options); got != value {
						t.Fatalf("configured limit = %d, want %d", got, value)
					}
					if client.options.repoIndexMaxAge != 2*time.Hour {
						t.Fatal("environment overrides changed the explicit TTL option")
					}
				})
			}
			for _, invalid := range []string{"", "0", "-1", "1.5", "64MiB", " 16", "16 ", "9223372036854775808"} {
				t.Run("invalid="+invalid, func(t *testing.T) {
					t.Setenv(tt.name, invalid)
					client, err := NewClient()
					if client != nil || err == nil || !strings.Contains(err.Error(), tt.name) {
						t.Fatalf("expected named configuration error, client=%v err=%v", client, err)
					}
				})
			}
		})
	}
}

func TestResourceLimitEnvReadPerClient(t *testing.T) {
	clearLimitEnv(t)
	first := newTestClient(t)
	t.Setenv("MCP_HELM_REPO_CACHE_MAX_ENTRIES", "42")
	second := newTestClient(t)
	if first.options.repoCacheEntries != DefaultRepoCacheMaxEntries || second.options.repoCacheEntries != 42 {
		t.Fatalf("limits are not per-client: first=%d second=%d", first.options.repoCacheEntries, second.options.repoCacheEntries)
	}
}
