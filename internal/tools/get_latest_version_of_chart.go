package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/Masterminds/semver/v3"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/lib/helm_client"
	"github.com/zekker6/mcp-helm/lib/logger"
	"go.uber.org/zap"
)

func NewGetLatestVersionOfChartTool() mcp.Tool {
	return mcp.NewTool("get_latest_version_of_chart",
		mcp.WithDescription("Retrieves the latest version of the chart"),
		mcp.WithString("repository_url",
			mcp.Required(),
			mcp.Description("Helm repository URL"),
		),
		mcp.WithString("chart_name",
			mcp.Required(),
			mcp.Description("Chart name"),
		),
	)
}

func GetLatestVersionOfCharHandler(c *helm_client.HelmClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {

		repositoryURL, err := request.RequireString("repository_url")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		chartName, err := request.RequireString("chart_name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		versions, err := c.ListChartVersions(repositoryURL, chartName)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list charts: %v", err)), nil
		}

		if len(versions) == 0 {
			return mcp.NewToolResultError("no versions found"), nil
		}

		vs := make([]*semver.Version, len(versions))
		for i, r := range versions {
			v, err := semver.NewVersion(r)
			if err != nil {
				logger.Warn("failed to parse version",
					zap.String("chart_name", chartName),
					zap.String("version", r),
					zap.String("repository_url", repositoryURL),
					zap.Error(err),
				)
			}

			vs[i] = v
		}

		collection := semver.Collection(vs)
		sort.Sort(collection)
		latest := collection[len(collection)-1]

		return mcp.NewToolResultText(latest.String()), nil
	}
}
