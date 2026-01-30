package tools

import (
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

// CommonParams holds the common request parameters used across chart tools.
type CommonParams struct {
	RepositoryURL string
	ChartName     string
	ChartVersion  string
}

// ExtractCommonParams extracts repository_url, chart_name, and optionally chart_version
// from the MCP request. If resolveLatestVersion is true and chart_version is empty,
// it will fetch the latest version from the repository.
//
// For OCI URLs, chart_name is optional - if not provided, it will be extracted from the URL.
// For HTTP repositories, chart_name is required.
func ExtractCommonParams(request mcp.CallToolRequest, c *helm_client.HelmClient, resolveLatestVersion bool) (*CommonParams, *mcp.CallToolResult) {
	repositoryURL, err := request.RequireString("repository_url")
	if err != nil {
		return nil, mcp.NewToolResultError(err.Error())
	}
	repositoryURL = strings.TrimSpace(repositoryURL)

	// chart_name is optional for OCI URLs (can be extracted from URL)
	chartName := strings.TrimSpace(request.GetString("chart_name", ""))

	// For OCI URLs, extract chart name from URL if not provided
	if helm_client.IsOCI(repositoryURL) {
		if chartName == "" {
			chartName = helm_client.ExtractChartNameFromOCI(repositoryURL)
			if chartName == "" {
				return nil, mcp.NewToolResultError("chart_name is required: could not extract chart name from OCI URL")
			}
		}
	} else {
		// For HTTP repositories, chart_name is required
		if chartName == "" {
			return nil, mcp.NewToolResultError("chart_name is required for HTTP repositories")
		}
	}

	chartVersion := request.GetString("chart_version", "")
	if chartVersion == "" && resolveLatestVersion {
		chartVersion, err = c.GetChartLatestVersion(repositoryURL, chartName)
		if err != nil {
			return nil, mcp.NewToolResultError(fmt.Sprintf("failed to get the latest chart version: %v", err))
		}
	}

	return &CommonParams{
		RepositoryURL: repositoryURL,
		ChartName:     chartName,
		ChartVersion:  chartVersion,
	}, nil
}

// ExtractRepositoryURL extracts and trims the repository_url parameter from the request.
func ExtractRepositoryURL(request mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	repositoryURL, err := request.RequireString("repository_url")
	if err != nil {
		return "", mcp.NewToolResultError(err.Error())
	}
	return strings.TrimSpace(repositoryURL), nil
}
