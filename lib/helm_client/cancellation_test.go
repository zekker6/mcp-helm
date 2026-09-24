package helm_client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCanceledRepositoryOperationsDoNotContactServers(t *testing.T) {
	archive := testChartArchive(t)
	var httpHits atomic.Int32
	var repoURL string
	repo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHits.Add(1)
		if r.URL.Path == "/index.yaml" {
			_, _ = w.Write(createTestIndex(repoURL))
		} else {
			_, _ = w.Write(archive)
		}
	}))
	defer repo.Close()
	repoURL = repo.URL

	client := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListChartVersions(ctx, repoURL, "test-chart"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled index fetch error = %v", err)
	}
	if got := httpHits.Load(); got != 0 {
		t.Errorf("canceled index fetch made %d requests", got)
	}

	// Cache the index to isolate the archive download from the index fetch.
	if _, err := client.ListChartVersions(context.Background(), repoURL, "test-chart"); err != nil {
		t.Fatal(err)
	}
	hits := httpHits.Load()
	if _, err := client.GetChartValues(ctx, repoURL, "test-chart", "1.0.0"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled archive fetch error = %v", err)
	}
	if got := httpHits.Load(); got != hits {
		t.Errorf("canceled archive fetch made %d requests", got-hits)
	}

	var ociHits atomic.Int32
	artifact := buildOCIArtifact(t, "charts/"+matrixChart, matrixVersion, buildMatrixChartTGZ(t))
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ociHits.Add(1)
		artifact.ServeHTTP(w, r)
	}))
	defer registry.Close()
	ociClient, err := NewClient(WithPlainHTTP(true))
	if err != nil {
		t.Fatal(err)
	}
	ociURL := "oci://" + strings.TrimPrefix(registry.URL, "http://") + "/charts/" + matrixChart
	if _, err := ociClient.ListChartVersions(ctx, ociURL, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled OCI tags error = %v", err)
	}
	if _, err := ociClient.GetChartValues(ctx, ociURL, "", matrixVersion); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled OCI pull error = %v", err)
	}
	if got := ociHits.Load(); got != 0 {
		t.Errorf("canceled OCI operations made %d requests", got)
	}
}
