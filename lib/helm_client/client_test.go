package helm_client

import (
	"strings"
	"testing"
)

const (
	testRepoURL   = "https://zekker6.github.io/helm-charts"
	testChartName = "readeck"
)

func TestNewClient(t *testing.T) {
	client := NewClient()
	if client == nil {
		t.Fatal("NewClient() returned nil")
	}
	if client.settings == nil {
		t.Fatal("client.settings is nil")
	}
	if client.repos == nil {
		// This is fine, repos is initialized on first use
	}
}

func TestListCharts(t *testing.T) {
	client := NewClient()
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
	client := NewClient()
	versions, err := client.ListChartVersions(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("ListChartVersions() error = %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("ListChartVersions() returned empty list")
	}
}

func TestGetChartLatestVersion(t *testing.T) {
	client := NewClient()
	version, err := client.GetChartLatestVersion(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestVersion() error = %v", err)
	}
	if version == "" {
		t.Fatal("GetChartLatestVersion() returned empty version")
	}
}

func TestGetChartValues(t *testing.T) {
	client := NewClient()

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
	client := NewClient()
	values, err := client.GetChartLatestValues(testRepoURL, testChartName)
	if err != nil {
		t.Fatalf("GetChartLatestValues() error = %v", err)
	}
	if values == "" {
		t.Fatal("GetChartLatestValues() returned empty values")
	}
}

func TestGetChartContents(t *testing.T) {
	client := NewClient()

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
	client := NewClient()

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
