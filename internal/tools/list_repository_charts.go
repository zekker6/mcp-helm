package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
)

// NewListChartsTool creates a tool for finding chart versions
func NewListChartsTool() mcp.Tool {
	return mcp.NewTool("list_repository_charts",
		mcp.WithDescription("Lists all charts available in the repository"),
		mcp.WithString("repository_url",
			mcp.Required(),
			mcp.Description("Helm repository URL"),
		),
	)
}

// GetListChartsHandler handles chart version finding requests
func GetListChartsHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {

		repositoryURL, err := request.RequireString("repository_url")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		charts, err := c.ListCharts(repositoryURL)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list charts: %v", err)), nil
		}

		return mcp.NewToolResultText(strings.Join(charts, ", ")), nil
	}
}
