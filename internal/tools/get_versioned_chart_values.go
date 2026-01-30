package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

func NewGetChartValuesTool() mcp.Tool {
	return mcp.NewTool("get_chart_values",
		mcp.WithDescription("Retrieves values file for the chart. Supports both HTTP repositories and OCI registries."),
		mcp.WithString("repository_url",
			mcp.Required(),
			mcp.Description("Helm repository URL. Supports HTTP repos (e.g., https://charts.example.com) and OCI registries (e.g., oci://ghcr.io/org/charts/mychart)"),
		),
		mcp.WithString("chart_name",
			mcp.Required(),
			mcp.Description("Chart name. For OCI URLs that already include the chart name, this can be empty."),
		),
		mcp.WithString("chart_version",
			mcp.Description("Chart version. If omitted the latest version will be used")),
	)
}

func GetChartValuesHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params, errResult := ExtractCommonParams(request, c, true)
		if errResult != nil {
			return errResult, nil
		}

		values, err := c.GetChartValues(params.RepositoryURL, params.ChartName, params.ChartVersion)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to get chart values: %v", err)), nil
		}
		encoded, err := json.MarshalIndent(values, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to marshal values: %v", err)), nil
		}

		return mcp.NewToolResultText(string(encoded)), nil
	}
}
