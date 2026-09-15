package helm_parser

import (
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
)

// DependencyItem represents a chart dependency for testing
type DependencyItem struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
}

// createMockChart creates a mock chart for testing
func createMockChart() *chartv2.Chart {
	return &chartv2.Chart{
		Metadata: &chartv2.Metadata{
			Name:    "test-chart",
			Version: "1.0.0",
		},
		Raw: []*common.File{
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
			{
				Name: "README.md",
				Data: []byte("# test-chart\n"),
			},
			{
				Name: "charts/dependency1-1.2.3.tgz",
				Data: []byte("binary subchart archive"),
			},
		},
		Files: []*common.File{
			{
				Name: "README.md",
				Data: []byte("# test-chart\n"),
			},
		},
	}
}

// createMockSubchart creates a mock subchart for testing
func createMockSubchart() *chartv2.Chart {
	return &chartv2.Chart{
		Metadata: &chartv2.Metadata{
			Name:    "subchart",
			Version: "1.0.0",
		},
		Raw: []*common.File{
			{
				Name: "Chart.yaml",
				Data: []byte(`
name: subchart
version: 1.0.0
`),
			},
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
	contents, err := GetChartContents(mockChart, false, nil)
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
	for _, want := range []string{
		"# file: test-chart/Chart.yaml\n",
		"# file: test-chart/values.yaml\n",
		"# file: test-chart/templates/deployment.yaml\n",
		"# file: test-chart/README.md\n",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("GetChartContents() output missing %q", want)
		}
	}
	if strings.Contains(contents, "# file: test-chart/charts/") {
		t.Error("GetChartContents() output should not include vendored subchart archives")
	}

	// Add a subchart for recursive test
	mockSubchart := createMockSubchart()
	mockChart.AddDependency(mockSubchart)

	// Test with recursion
	contentsRecursive, err := GetChartContents(mockChart, true, nil)
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
	if !strings.Contains(contentsRecursive, "# file: subchart/values.yaml\n") {
		t.Fatal("Recursive contents should include subchart values.yaml")
	}
}

func TestGetChartContentsPaths(t *testing.T) {
	tests := []struct {
		name      string
		paths     []string
		recursive bool
		want      []string
		notWant   []string
	}{
		{
			name:    "exact file",
			paths:   []string{"Chart.yaml"},
			want:    []string{"# file: test-chart/Chart.yaml\n"},
			notWant: []string{"# file: test-chart/values.yaml\n", "# file: test-chart/templates/", "# file: test-chart/README.md\n"},
		},
		{
			name:    "double star crosses directories",
			paths:   []string{"templates/**"},
			want:    []string{"# file: test-chart/templates/deployment.yaml\n", "# file: test-chart/templates/backend/service.yaml\n"},
			notWant: []string{"# file: test-chart/Chart.yaml\n"},
		},
		{
			name:    "single star stops at directory separator",
			paths:   []string{"templates/*.yaml"},
			want:    []string{"# file: test-chart/templates/deployment.yaml\n"},
			notWant: []string{"# file: test-chart/templates/backend/service.yaml\n"},
		},
		{
			name:    "any of multiple patterns",
			paths:   []string{"Chart.yaml", "README.md"},
			want:    []string{"# file: test-chart/Chart.yaml\n", "# file: test-chart/README.md\n"},
			notWant: []string{"# file: test-chart/values.yaml\n"},
		},
		{
			name:      "recursive matches paths inside subcharts",
			paths:     []string{"values.yaml"},
			recursive: true,
			want:      []string{"# file: test-chart/values.yaml\n", "# Subchart: subchart\n", "# file: subchart/values.yaml\n"},
			notWant:   []string{"Chart.yaml\n"},
		},
		{
			name:      "no match returns empty contents",
			paths:     []string{"missing/**"},
			recursive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := createMockChart()
			c.Raw = append(c.Raw, &common.File{Name: "templates/backend/service.yaml", Data: []byte("kind: Service\n")})
			c.AddDependency(createMockSubchart())

			contents, err := GetChartContents(c, tt.recursive, tt.paths)
			if err != nil {
				t.Fatalf("GetChartContents() error = %v", err)
			}
			if len(tt.want) == 0 && contents != "" {
				t.Fatalf("GetChartContents() = %q, want empty", contents)
			}
			for _, want := range tt.want {
				if !strings.Contains(contents, want) {
					t.Errorf("GetChartContents() output missing %q\n%s", want, contents)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(contents, notWant) {
					t.Errorf("GetChartContents() output should not contain %q\n%s", notWant, contents)
				}
			}
		})
	}
}

func TestGetChartContentsInvalidPath(t *testing.T) {
	if _, err := GetChartContents(createMockChart(), false, []string{"["}); err == nil {
		t.Fatal("GetChartContents() expected error for invalid pattern")
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
