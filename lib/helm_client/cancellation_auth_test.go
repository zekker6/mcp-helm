package helm_client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
)

type retryStatusGetter struct {
	first chan struct{}
	hits  atomic.Int32
}

func (g *retryStatusGetter) Get(_ string, _ ...getter.Option) (*bytes.Buffer, error) {
	if g.hits.Add(1) == 1 {
		close(g.first)
	}
	return nil, errors.New("failed to fetch http://repo.test/index.yaml : 503 Service Unavailable")
}

func TestHTTPBackoffStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := &retryStatusGetter{first: make(chan struct{})}
	client := &retryGetter{Getter: status, ctx: ctx}
	done := make(chan error, 1)
	go func() { _, err := client.Get("http://repo.test/index.yaml"); done <- err }()
	<-status.first
	// Let the first 503 reach the retry wait, then cancel within its budget.
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("backoff error = %v, want context.Canceled", err)
		}
	case <-time.After(130 * time.Millisecond):
		t.Fatal("retry wait ignored cancellation")
	}
	if hits := status.hits.Load(); hits != 1 {
		t.Errorf("requests after cancellation = %d, want 1", hits)
	}
}

type blockingCredentialStore struct {
	started chan struct{}
}

func (s *blockingCredentialStore) Get(ctx context.Context, _ string) (auth.Credential, error) {
	close(s.started)
	<-ctx.Done()
	return auth.EmptyCredential, ctx.Err()
}
func (s *blockingCredentialStore) Put(context.Context, string, auth.Credential) error { return nil }
func (s *blockingCredentialStore) Delete(context.Context, string) error               { return nil }

func TestCanceledOCIRoutingDoesNotCacheFallback(t *testing.T) {
	client := newTestClient(t)
	store := &blockingCredentialStore{started: make(chan struct{})}
	client.credStore = store
	client.registryClientCreds = client.registryClient
	client.routeCache = make(map[string]*registry.Client)
	client.options.credentialsFile = writeDockerConfig(t, "registry.example.com")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.ListChartVersions(ctx, "oci://registry.example.com/charts/test-chart", "")
		done <- err
	}()
	<-store.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("credential routing error = %v, want context.Canceled", err)
	}
	client.routeMu.Lock()
	count := len(client.routeCache)
	client.routeMu.Unlock()
	if count != 0 {
		t.Errorf("cached %d routes after canceled credential lookup", count)
	}
}

func TestCanceledOCIAuthenticationStopsCredentialHelper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	binary := filepath.Join(dir, "docker-credential-mcp-cancel-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf started > \"$MCP_HELM_TEST_HELPER_STARTED\"\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_HELM_TEST_HELPER_STARTED", marker)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := writeDockerConfigRaw(t, map[string]any{"credHelpers": map[string]string{host: "mcp-cancel-test"}})
	client, err := NewClient(WithCredentialsFile(cfg), WithPlainHTTP(true))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = client.ListChartVersions(ctx, "oci://"+host+"/charts/test-chart", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("helper cancellation error = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("credential helper occupied worker for %s", elapsed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("credential helper was not invoked: %v", err)
	}
}
