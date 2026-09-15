package helm_parser

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gobwas/glob"
	"gopkg.in/yaml.v2"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
)

type chartSchema struct {
	Dependencies []dependencyItem `json:"dependencies" yaml:"dependencies"`
}

type dependencyItem struct {
	Name       string `json:"name" yaml:"name"`
	Version    string `json:"version" yaml:"version"`
	Repository string `json:"repository" yaml:"repository"`
}

func GetChartDependencies(chart *chartv2.Chart) ([]string, error) {
	var chartYAML []byte
	for _, file := range chart.Raw {
		if file.Name == "Chart.yaml" {
			chartYAML = file.Data
			break
		}
	}

	if len(chartYAML) == 0 {
		return nil, fmt.Errorf("`Chart.yaml` not found in the chart")
	}

	var schema chartSchema
	if err := yaml.Unmarshal(chartYAML, &schema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Chart.yaml: %v", err)
	}

	if len(schema.Dependencies) == 0 {
		return nil, nil
	}

	dependentCharts := chart.Dependencies()
	dependencies := make([]string, 0, len(schema.Dependencies))
	for _, dep := range schema.Dependencies {
		if dep.Name == "" || dep.Version == "" || dep.Repository == "" {
			return nil, fmt.Errorf("dependency item is missing required fields: %v", dep)
		}
		depJSON, err := json.Marshal(dep)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal dependency item: %v", err)
		}
		dependencies = append(dependencies, string(depJSON))
		for _, dependentChart := range dependentCharts {
			if dependentChart.Name() == dep.Name && dependentChart.Metadata.Version == dep.Version {
				dependantChartDeps, err := GetChartDependencies(dependentChart)
				if err != nil {
					return nil, fmt.Errorf("failed to get dependencies for chart %s: %v", dependentChart.Name(), err)
				}
				dependencies = append(dependencies, dependantChartDeps...)
				break
			}
		}
	}

	return dependencies, nil
}

// GetChartContents returns the chart files whose path inside the chart matches any of paths
// (gobwas/glob syntax, `*` stops at `/`, `**` does not). Empty paths returns all files.
func GetChartContents(c *chartv2.Chart, recursive bool, paths []string) (string, error) {
	patterns := make([]*glob.Pattern, 0, len(paths))
	for _, p := range paths {
		pattern, err := glob.Compile(p, '/')
		if err != nil {
			return "", fmt.Errorf("invalid path pattern %q: %v", p, err)
		}
		patterns = append(patterns, pattern)
	}
	return chartContents(c, recursive, patterns), nil
}

func chartContents(c *chartv2.Chart, recursive bool, patterns []*glob.Pattern) string {
	sb := strings.Builder{}
	// Files only holds leftovers (README, LICENSE, ...); Raw has every file in the archive.
	// charts/ entries are vendored subcharts, emitted via Dependencies() when recursive.
	for _, file := range c.Raw {
		if strings.HasPrefix(file.Name, "charts/") || !matchesAny(patterns, file.Name) {
			continue
		}
		fmt.Fprintf(&sb, "# file: %s/%s\n", c.Name(), file.Name)
		sb.Write(file.Data)
		sb.WriteString("\n\n")
	}
	if recursive {
		for _, subChart := range c.Dependencies() {
			subContent := chartContents(subChart, recursive, patterns)
			if subContent == "" {
				continue
			}
			fmt.Fprintf(&sb, "# Subchart: %s\n", subChart.Name())
			sb.WriteString(subContent)
		}
	}
	return sb.String()
}

func matchesAny(patterns []*glob.Pattern, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p.Match(name) {
			return true
		}
	}
	return false
}
