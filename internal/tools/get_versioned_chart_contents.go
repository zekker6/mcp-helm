package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

func NewGetChartContentsTool() mcp.Tool {
	return mcp.NewTool("get_chart_contents",
		mcp.WithDescription("Retrieves full chart contents. Supports both HTTP repositories and OCI registries."),
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
			mcp.Description("If true, also includes files from subcharts. Defaults to false"),
		),
		mcp.WithArray("paths",
			mcp.WithStringItems(),
			mcp.Description("Glob patterns matched against file paths inside the chart and, when recursive, inside each subchart (e.g. [\"Chart.yaml\", \"templates/**\"]). `*` does not cross `/`, `**` does. If omitted, all files are returned"),
		),
	)
}

func GetChartContentsHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params, errResult := ExtractCommonParams(request, c, true)
		if errResult != nil {
			return errResult, nil
		}

		recursive := request.GetBool("recursive", false)
		paths := request.GetStringSlice("paths", nil)

		charts, err := c.GetChartContents(params.RepositoryURL, params.ChartName, params.ChartVersion, recursive, paths)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list charts: %v", err)), nil
		}
		encoded, err := json.MarshalIndent(charts, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to marshal charts: %v", err)), nil
		}

		return mcp.NewToolResultText(string(encoded)), nil
	}
}
