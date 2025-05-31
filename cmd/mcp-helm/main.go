package main

import (
	"flag"
	"fmt"

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
	mode           = flag.String("mode", "stdio", "Mode to run the MCP server in (stdio, sse)")
	httpListenAddr = flag.String("httpListenAddr", ":8012", "Address to listen for http connections in sse mode")
)

func main() {
	flag.Parse()

	logger.Init()
	defer logger.Stop()

	// verify config
	if *mode != "stdio" && *mode != "sse" {
		logger.Error("Invalid mode specified", zap.String("mode", *mode))
		fmt.Printf("Invalid mode specified: %s. Supported modes are 'stdio' and 'sse'.\n", *mode)
		return
	}

	if *mode == "sse" && *httpListenAddr == "" {
		logger.Error("HTTP listen address must be specified in sse mode", zap.String("httpListenAddr", *httpListenAddr))
		fmt.Println("HTTP listen address must be specified in sse mode. Use --httpListenAddr to set it.")
		return
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

	logger.Info("Starting MCP Helm server",
		zap.String("version", version),
		zap.String("commit", commit),
		zap.String("date", date),
		zap.String("mode", *mode),
		zap.String("httpListenAddr", *httpListenAddr),
	)

	if *mode == "sse" {
		srv := server.NewSSEServer(s)
		if err := srv.Start(*httpListenAddr); err != nil {
			logger.Error("Failed to start SSE server", zap.Error(err))
		}
	} else {
		if err := server.ServeStdio(s); err != nil {
			logger.Error("Failed to start MCP server in stdio mode", zap.Error(err))
		}
	}
}
