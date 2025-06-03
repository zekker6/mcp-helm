package helm_parser

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/chart"
)

type chartSchema struct {
	Dependencies []dependencyItem `json:"dependencies" yaml:"dependencies"`
}

type dependencyItem struct {
	Name       string `json:"name" yaml:"name"`
	Version    string `json:"version" yaml:"version"`
	Repository string `json:"repository" yaml:"repository"`
}

func GetChartDependencies(chart *chart.Chart) ([]string, error) {
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
