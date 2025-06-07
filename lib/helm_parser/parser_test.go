package helm_parser

import (
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
)

// DependencyItem represents a chart dependency for testing
type DependencyItem struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
}

// createMockChart creates a mock chart for testing
func createMockChart() *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "test-chart",
			Version: "1.0.0",
		},
		Raw: []*chart.File{
			{
				Name: "Chart.yaml",
				Data: []byte(`
name: test-chart
version: 1.0.0
dependencies:
  - name: dependency1
    version: 1.2.3
    repository: https://charts.example.com/
  - name: dependency2
    version: 4.5.6
    repository: https://charts.example.org/
`),
			},
		},
		Files: []*chart.File{
			{
				Name: "values.yaml",
				Data: []byte(`
replicaCount: 1
image:
  repository: nginx
  tag: latest
`),
			},
			{
				Name: "templates/deployment.yaml",
				Data: []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
`),
			},
		},
	}
}

// createMockSubchart creates a mock subchart for testing
func createMockSubchart() *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{
			Name:    "subchart",
			Version: "1.0.0",
		},
		Raw: []*chart.File{
			{
				Name: "Chart.yaml",
				Data: []byte(`
name: subchart
version: 1.0.0
`),
			},
		},
		Files: []*chart.File{
			{
				Name: "values.yaml",
				Data: []byte(`
subchartValue: true
`),
			},
		},
	}
}

func TestGetChartContents(t *testing.T) {
	// Create a mock chart
	mockChart := createMockChart()

	// Test without recursion
	contents, err := GetChartContents(mockChart, false)
	if err != nil {
		t.Fatalf("GetChartContents(recursive=false) error = %v", err)
	}
	if contents == "" {
		t.Fatal("GetChartContents(recursive=false) returned empty contents")
	}

	// Verify that the contents contain expected patterns
	if !strings.Contains(contents, "# file:") {
		t.Fatal("GetChartContents() output doesn't contain expected file markers")
	}

	// Add a subchart for recursive test
	mockSubchart := createMockSubchart()
	mockChart.AddDependency(mockSubchart)

	// Test with recursion
	contentsRecursive, err := GetChartContents(mockChart, true)
	if err != nil {
		t.Fatalf("GetChartContents(recursive=true) error = %v", err)
	}
	if contentsRecursive == "" {
		t.Fatal("GetChartContents(recursive=true) returned empty contents")
	}

	// Recursive contents should be longer than non-recursive contents
	if len(contentsRecursive) <= len(contents) {
		t.Fatal("Recursive contents should be longer than non-recursive contents")
	}

	// Verify subchart content is included
	if !strings.Contains(contentsRecursive, "# Subchart: subchart") {
		t.Fatal("Recursive contents should include subchart marker")
	}
}

func TestGetChartDependencies(t *testing.T) {
	// Create a mock chart with dependencies
	mockChart := createMockChart()

	deps, err := GetChartDependencies(mockChart)
	if err != nil {
		t.Fatalf("GetChartDependencies() error = %v", err)
	}

	// Ensure deps is not nil
	if deps == nil {
		t.Fatal("GetChartDependencies() returned nil")
	}

	// Verify we have the expected number of dependencies
	if len(deps) != 2 {
		t.Fatalf("Expected 2 dependencies, got %d", len(deps))
	}

	// If there are dependencies, verify they are valid JSON
	for _, dep := range deps {
		var depItem DependencyItem
		err := json.Unmarshal([]byte(dep), &depItem)
		if err != nil {
			t.Fatalf("Failed to unmarshal dependency JSON: %v", err)
		}

		// Verify required fields
		if depItem.Name == "" {
			t.Fatal("Dependency name is empty")
		}
		if depItem.Version == "" {
			t.Fatal("Dependency version is empty")
		}
		if depItem.Repository == "" {
			t.Fatal("Dependency repository is empty")
		}
	}

	// Verify dependency content
	var firstDep, secondDep DependencyItem
	_ = json.Unmarshal([]byte(deps[0]), &firstDep)
	_ = json.Unmarshal([]byte(deps[1]), &secondDep)

	if firstDep.Name != "dependency1" {
		t.Fatalf("Expected first dependency name to be 'dependency1', got '%s'", firstDep.Name)
	}

	if secondDep.Name != "dependency2" {
		t.Fatalf("Expected second dependency name to be 'dependency2', got '%s'", secondDep.Name)
	}
}
