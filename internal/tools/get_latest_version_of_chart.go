package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

func NewGetLatestVersionOfChartTool() mcp.Tool {
	return mcp.NewTool("get_latest_version_of_chart",
		mcp.WithDescription("Retrieves the latest version of the chart. For OCI registries, returns the latest semver tag."),
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

func GetLatestVersionOfCharHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params, errResult := ExtractCommonParams(request, c, false)
		if errResult != nil {
			return errResult, nil
		}

		version, err := c.GetChartLatestVersion(params.RepositoryURL, params.ChartName)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list charts: %v", err)), nil
		}

		return mcp.NewToolResultText(version), nil
	}
}
