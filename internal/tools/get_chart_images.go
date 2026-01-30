package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zekker6/mcp-helm/lib/helm_client"
	"github.com/zekker6/mcp-helm/lib/helm_parser"
)

func NewGetChartImagesTool() mcp.Tool {
	return mcp.NewTool("get_chart_images",
		mcp.WithDescription("Extracts container images used in a Helm chart by rendering templates and parsing Kubernetes manifests. Supports both HTTP repositories and OCI registries."),
		mcp.WithString("repository_url",
			mcp.Required(),
			mcp.Description("Helm repository URL. Supports HTTP repos (e.g., https://charts.example.com) and OCI registries (e.g., oci://ghcr.io/org/charts/mychart)"),
		),
		mcp.WithString("chart_name",
			mcp.Required(),
			mcp.Description("Chart name. For OCI URLs that already include the chart name, this can be empty."),
		),
		mcp.WithString("chart_version",
			mcp.Description("Chart version. If omitted the latest version will be used"),
		),
		mcp.WithBoolean("recursive",
			mcp.Description("If true, extracts images from subcharts as well. Defaults to false"),
		),
		mcp.WithString("custom_values",
			mcp.Description("JSON object of custom values to override chart defaults (e.g., {\"image.tag\": \"v2\"})"),
		),
	)
}

type chartImagesResult struct {
	Chart      string                       `json:"chart"`
	Version    string                       `json:"version"`
	ImageCount int                          `json:"imageCount"`
	Images     []helm_parser.ImageReference `json:"images"`
}

func GetChartImagesHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repositoryURL, err := request.RequireString("repository_url")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		repositoryURL = strings.TrimSpace(repositoryURL)

		chartName, err := request.RequireString("chart_name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		chartName = strings.TrimSpace(chartName)

		chartVersion := request.GetString("chart_version", "")
		if chartVersion == "" {
			chartVersion, err = c.GetChartLatestVersion(repositoryURL, chartName)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to get the latest chart version: %v", err)), nil
			}
		}

		recursive := request.GetBool("recursive", false)

		// Parse custom values if provided
		var customValues map[string]interface{}
		customValuesStr := request.GetString("custom_values", "")
		if customValuesStr != "" {
			if err := json.Unmarshal([]byte(customValuesStr), &customValues); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to parse custom_values JSON: %v", err)), nil
			}
		}

		images, err := c.GetChartImages(repositoryURL, chartName, chartVersion, customValues, recursive)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to extract images: %v", err)), nil
		}

		result := chartImagesResult{
			Chart:      chartName,
			Version:    chartVersion,
			ImageCount: len(images),
			Images:     images,
		}

		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to marshal result: %v", err)), nil
		}

		return mcp.NewToolResultText(string(encoded)), nil
	}
}
