package helm_client

import (
	"strings"
	"testing"
)

const (
	testRepoURL   = "https://zekker6.github.io/helm-charts"
	testChartName = "readeck"

	testOCIRepoURL   = "oci://ghcr.io/victoriametrics/helm-charts/victoria-logs-single"
	testOCIChartName = "" // Chart name is in the URL
)

func newTestClient(t *testing.T) *HelmClient {
	t.Helper()
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func TestNewClient(t *testing.T) {
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned nil")
	}
	if client.settings == nil {
		t.Fatal("client.settings is nil")
	}
}

func TestListCharts(t *testing.T) {
	client := newTestClient(t)
	charts, err := client.ListCharts(testRepoURL)
	if err != nil {
		t.Fatalf("ListCharts() error = %v", err)
	}
	if len(charts) == 0 {
		t.Fatal("ListCharts() returned empty list")
	}

	// Check if readeck chart is in the list
	found := false
	for _, chart := range charts {
		if chart == testChartName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListCharts() did not return the expected chart %s", testChartName)
	}
}

func TestListChartVersions(t *testing.T) {
	client := newTestClient(t)
	versions, err := client.ListChartVersions(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("ListChartVersions() error = %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("ListChartVersions() returned empty list")
	}
}

func TestGetChartLatestVersion(t *testing.T) {
	client := newTestClient(t)
	version, err := client.GetChartLatestVersion(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}
	if version == "" {
		t.Fatal("GetChartLatestVersion() returned empty version")
	}
}

func TestGetChartValues(t *testing.T) {
	client := newTestClient(t)

	// Get the latest version first
	version, err := client.GetChartLatestVersion(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}

	values, err := client.GetChartValues(testRepoURL, testChartName, version)
	if err != nil {
		t.Fatalf("GetChartValues() error = %v", err)
	}
	if values == "" {
		t.Fatal("GetChartValues() returned empty values")
	}

	// Check if the values contain some YAML structure (any key-value pair)
	if !strings.Contains(values, ":") {
		t.Fatal("GetChartValues() did not return expected YAML structure")
	}
}

func TestGetChartLatestValues(t *testing.T) {
	client := newTestClient(t)
	values, err := client.GetChartLatestValues(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestValues() error = %v", err)
	}
	if values == "" {
		t.Fatal("GetChartLatestValues() returned empty values")
	}
}

func TestGetChartContents(t *testing.T) {
	client := newTestClient(t)

	// Get the latest version first
	version, err := client.GetChartLatestVersion(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}

	// Test without recursion
	contents, err := client.GetChartContents(testRepoURL, testChartName, version, false)
	if err != nil {
		t.Fatalf("GetChartContents(recursive=false) error = %v", err)
	}
	if contents == "" {
		t.Fatal("GetChartContents(recursive=false) returned empty contents")
	}

	// Test with recursion
	contentsRecursive, err := client.GetChartContents(testRepoURL, testChartName, version, true)
	if err != nil {
		t.Fatalf("GetChartContents(recursive=true) error = %v", err)
	}
	if contentsRecursive == "" {
		t.Fatal("GetChartContents(recursive=true) returned empty contents")
	}

	// Recursive contents should be longer or equal to non-recursive contents
	if len(contentsRecursive) < len(contents) {
		t.Fatal("Recursive contents should be longer or equal to non-recursive contents")
	}
}

func TestGetChartDependencies(t *testing.T) {
	client := newTestClient(t)

	// Get the latest version first
	version, err := client.GetChartLatestVersion(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}

	deps, err := client.GetChartDependencies(testRepoURL, testChartName, version)
	if err != nil {
		t.Fatalf("GetChartDependencies() error = %v", err)
	}

	// Note: The test chart may or may not have dependencies, so we don't assert on the length
	// Just ensure the function runs without error and returns a valid slice
	if deps == nil {
		t.Fatal("GetChartDependencies() returned nil slice")
	}
}

func TestGetChartImages(t *testing.T) {
	client := newTestClient(t)

	// Get the latest version first
	version, err := client.GetChartLatestVersion(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}

	images, err := client.GetChartImages(testRepoURL, testChartName, version, nil, false)
	if err != nil {
		t.Fatalf("GetChartImages() error = %v", err)
	}

	// The chart should have at least one image
	if len(images) == 0 {
		t.Fatal("GetChartImages() returned no images")
	}

	// Check that images have required fields
	for _, img := range images {
		if img.FullImage == "" {
			t.Error("Image has empty FullImage")
		}
		if img.Source == "" {
			t.Error("Image has empty Source")
		}
	}

	t.Logf("Found %d images:", len(images))
	for _, img := range images {
		t.Logf("  - %s (from %s)", img.FullImage, img.Source)
	}
}

func TestIsOCI(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"oci://ghcr.io/org/charts/mychart", true},
		{"oci://docker.io/library/mysql", true},
		{"oci://registry.example.com/helm/app:1.0.0", true},
		{"https://charts.example.com", false},
		{"http://localhost:8080", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := IsOCI(tt.url); got != tt.expected {
				t.Errorf("IsOCI(%q) = %v, want %v", tt.url, got, tt.expected)
			}
		})
	}
}

func TestParseOCIReference(t *testing.T) {
	tests := []struct {
		repoURL   string
		chartName string
		version   string
		expected  string
	}{
		{"oci://ghcr.io/org/charts", "mychart", "1.0.0", "ghcr.io/org/charts/mychart:1.0.0"},
		{"oci://ghcr.io/org/charts/mychart", "", "1.0.0", "ghcr.io/org/charts/mychart:1.0.0"},
		{"oci://ghcr.io/org/charts/mychart", "", "", "ghcr.io/org/charts/mychart"},
		{"oci://docker.io/library/mysql", "", "8.0", "docker.io/library/mysql:8.0"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := parseOCIReference(tt.repoURL, tt.chartName, tt.version); got != tt.expected {
				t.Errorf("parseOCIReference(%q, %q, %q) = %v, want %v", tt.repoURL, tt.chartName, tt.version, got, tt.expected)
			}
		})
	}
}

func TestExtractChartNameFromOCI(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"oci://ghcr.io/org/charts/mychart", "mychart"},
		{"oci://ghcr.io/org/charts/mychart:1.0.0", "mychart"},
		{"oci://docker.io/library/mysql", "mysql"},
		{"oci://registry.example.com/app", "app"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := ExtractChartNameFromOCI(tt.url); got != tt.expected {
				t.Errorf("ExtractChartNameFromOCI(%q) = %v, want %v", tt.url, got, tt.expected)
			}
		})
	}
}

func TestListChartsOCI(t *testing.T) {
	client := newTestClient(t)
	charts, err := client.ListCharts(testOCIRepoURL)
	if err != nil {
		t.Fatalf("ListCharts() error = %v", err)
	}
	if len(charts) == 0 {
		t.Fatal("ListCharts() returned empty list for OCI")
	}

	// For OCI, should return the chart name from the URL
	if charts[0] != "victoria-logs-single" {
		t.Errorf("ListCharts() = %v, expected [victoria-logs-single]", charts)
	}
}

func TestListChartVersionsOCI(t *testing.T) {
	client := newTestClient(t)
	versions, err := client.ListChartVersions(testOCIRepoURL, testOCIChartName)
	if err != nil {
		t.Fatalf("ListChartVersions() error = %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("ListChartVersions() returned empty list for OCI")
	}
	t.Logf("Found %d versions for OCI chart", len(versions))
}

func TestGetChartLatestVersionOCI(t *testing.T) {
	client := newTestClient(t)
	version, err := client.GetChartLatestVersion(testOCIRepoURL, testOCIChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}
	if version == "" {
		t.Fatal("GetChartLatestVersion() returned empty version for OCI")
	}
	t.Logf("Latest OCI chart version: %s", version)
}

func TestGetChartValuesOCI(t *testing.T) {
	client := newTestClient(t)

	version, err := client.GetChartLatestVersion(testOCIRepoURL, testOCIChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}

	values, err := client.GetChartValues(testOCIRepoURL, testOCIChartName, version)
	if err != nil {
		t.Fatalf("GetChartValues() error = %v", err)
	}
	if values == "" {
		t.Fatal("GetChartValues() returned empty values for OCI")
	}

	if !strings.Contains(values, ":") {
		t.Fatal("GetChartValues() did not return expected YAML structure for OCI")
	}
}

func TestClientOptions(t *testing.T) {
	client, err := NewClient(
		WithPlainHTTP(true),
		WithBasicAuth("user", "pass"),
	)
	if err != nil {
		t.Fatalf("NewClient() with options error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() with options returned nil")
	}
	if client.registryClient == nil {
		t.Fatal("client.registryClient is nil")
	}
}
