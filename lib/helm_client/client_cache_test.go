package helm_client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestRepoIndexRefresh(t *testing.T) {
	tests := []struct {
		name         string
		maxAge       time.Duration
		expire       bool
		wantHits     int
		wantVersions []string
	}{
		{
			name:         "zero max age downloads the index on every request",
			maxAge:       0,
			wantHits:     2,
			wantVersions: []string{"2.0.0", "1.0.0"},
		},
		{
			name:         "fresh index is reused",
			maxAge:       time.Hour,
			wantHits:     1,
			wantVersions: []string{"1.0.0"},
		},
		{
			name:         "expired index is downloaded again",
			maxAge:       time.Hour,
			expire:       true,
			wantHits:     2,
			wantVersions: []string{"2.0.0", "1.0.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				hits     int
				versions = []string{"1.0.0"}
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/index.yaml" {
					http.NotFound(w, r)
					return
				}

				mu.Lock()
				defer mu.Unlock()
				hits++

				index := testIndexFile{APIVersion: "v1", Generated: time.Now(), Entries: map[string][]testChartEntry{}}
				for _, v := range versions {
					index.Entries["test-chart"] = append(index.Entries["test-chart"], testChartEntry{
						Name:    "test-chart",
						Version: v,
						URLs:    []string{"charts/test-chart-" + v + ".tgz"},
					})
				}
				data, err := json.Marshal(index)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()

			client, err := NewClient(WithRepoIndexMaxAge(tt.maxAge))
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			if _, err := client.ListChartVersions(context.Background(), server.URL, "test-chart"); err != nil {
				t.Fatalf("first ListChartVersions() error = %v", err)
			}

			mu.Lock()
			versions = append(versions, "2.0.0")
			mu.Unlock()

			if tt.expire {
				client.reposMu.Lock()
				client.repos[server.URL].fetched = time.Now().Add(-2 * tt.maxAge)
				client.reposMu.Unlock()
			}

			got, err := client.ListChartVersions(context.Background(), server.URL, "test-chart")
			if err != nil {
				t.Fatalf("second ListChartVersions() error = %v", err)
			}
			if !slices.Equal(got, tt.wantVersions) {
				t.Errorf("ListChartVersions() = %v, want %v", got, tt.wantVersions)
			}

			mu.Lock()
			defer mu.Unlock()
			if hits != tt.wantHits {
				t.Errorf("index downloads = %d, want %d", hits, tt.wantHits)
			}
		})
	}
}

func TestNewClientDefaultRepoIndexMaxAge(t *testing.T) {
	client := newTestClient(t)
	if client.options.repoIndexMaxAge != DefaultRepoIndexMaxAge {
		t.Fatalf("repoIndexMaxAge = %v, want %v", client.options.repoIndexMaxAge, DefaultRepoIndexMaxAge)
	}
}
