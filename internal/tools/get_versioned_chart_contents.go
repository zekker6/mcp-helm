package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

func NewGetChartContentsTool() mcp.Tool {
	return mcp.NewTool("get_chat_contents",
		mcp.WithDescription("Retrieves full chart contents"),
		mcp.WithString("repository_url",
			mcp.Required(),
			mcp.Description("Helm repository URL"),
		),
		mcp.WithString("chart_name",
			mcp.Required(),
			mcp.Description("Chart name"),
		),
		mcp.WithString("chart_version",
			mcp.Description("Chart version. If omitted the latest version will be used")),
	)
}

func GetChartContentsHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
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

		charts, err := c.GetChartContents(repositoryURL, chartName, chartVersion)
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
