package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"
	"github.com/zekker6/mcp-helm/internal/tools"
	"github.com/zekker6/mcp-helm/lib/helm_client"
	"github.com/zekker6/mcp-helm/lib/logger"
	"go.uber.org/zap"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	mode                 = flag.String("mode", "stdio", "Mode to run the MCP server in (stdio, sse, http)")
	httpListenAddr       = flag.String("httpListenAddr", ":8012", "Address to listen for http connections in sse mode")
	heartbeatInterval    = flag.Duration("httpHeartbeatInterval", 30, "Interval for sending heartbeat messages in seconds. Only used when -mode=http (default: 30 seconds)")
	sseKeepAliveInterval = flag.Duration("sseKeepAliveInterval", 30, "Interval for sending keep-alive messages in seconds. Only used when -mode=sse (default: 30 seconds)")
)

func main() {
	flag.Parse()

	logger.Init()
	defer logger.Stop()

	switch *mode {
	case "stdio", "sse", "http":
	default:
		logger.Error("Invalid mode specified: %s. Supported modes are 'stdio', 'sse', and 'http'", zap.String("mode", *mode))
		os.Exit(1)
	}

	switch *mode {
	case "sse", "http":
		if *httpListenAddr == "" {
			logger.Error("HTTP listen address must be specified in sse mode. Use -httpListenAddr to set it", zap.String("httpListenAddr", *httpListenAddr))
			os.Exit(1)
		}
	}

	s := server.NewMCPServer(
		"Helm MCP Server",
		fmt.Sprintf("v%s (commit: %s, date: %s)", version, commit, date),
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	helmClient := helm_client.NewClient()
	s.AddTool(tools.NewListChartsTool(), tools.GetListChartsHandler(helmClient))
	s.AddTool(tools.NewGetLatestVersionOfChartTool(), tools.GetLatestVersionOfCharHandler(helmClient))
	s.AddTool(tools.NewGetChartValuesTool(), tools.GetChartValuesHandler(helmClient))
	s.AddTool(tools.NewGetChartContentsTool(), tools.GetChartContentsHandler(helmClient))
	s.AddTool(tools.NewGetChartDependenciesTool(), tools.GetChartDependenciesHandler(helmClient))

	logger.Info("Starting MCP Helm server",
		zap.String("version", version),
		zap.String("commit", commit),
		zap.String("date", date),
		zap.String("mode", *mode),
		zap.String("httpListenAddr", *httpListenAddr),
	)

	switch *mode {
	case "stdio":
		if err := server.ServeStdio(s); err != nil {
			logger.Error("Failed to start MCP server in stdio mode", zap.Error(err))
		}
	case "sse":
		var opts []server.SSEOption
		if *sseKeepAliveInterval > 0 {
			opts = append(opts, server.WithKeepAliveInterval(*sseKeepAliveInterval))
		}

		srv := server.NewSSEServer(s, opts...)
		if err := srv.Start(*httpListenAddr); err != nil {
			logger.Error("Failed to start SSE server", zap.Error(err))
		}
	case "http":
		var opts []server.StreamableHTTPOption
		if *heartbeatInterval > 0 {
			opts = append(opts, server.WithHeartbeatInterval(*heartbeatInterval))
		}
		srv := server.NewStreamableHTTPServer(s, opts...)
		if err := srv.Start(*httpListenAddr); err != nil {
			logger.Error("Failed to start HTTP server", zap.Error(err))
		}
	default:
		logger.Error("Unsupported mode specified", zap.String("mode", *mode))
	}
}
