package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

func NewListChartVersionsTool() mcp.Tool {
	return mcp.NewTool("list_chart_versions",
		mcp.WithDescription("Lists all available versions (tags) for a chart. For OCI registries, this lists all tags. For HTTP repositories, lists all versions from the index."),
		mcp.WithString("repository_url",
			mcp.Required(),
			mcp.Description("Helm repository URL. Supports HTTP repos (e.g., https://charts.example.com) and OCI registries (e.g., oci://ghcr.io/org/charts/mychart)"),
		),
		mcp.WithString("chart_name",
			mcp.Required(),
			mcp.Description("Chart name. For OCI URLs that already include the chart name, this can be empty."),
		),
	)
}

func GetListChartVersionsHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
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

		versions, err := c.ListChartVersions(repositoryURL, chartName)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list chart versions: %v", err)), nil
		}

		if len(versions) == 0 {
			return mcp.NewToolResultText("No versions found"), nil
		}

		return mcp.NewToolResultText(strings.Join(versions, ", ")), nil
	}
}
