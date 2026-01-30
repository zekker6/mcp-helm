package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/zekker6/mcp-helm/internal/tools"
	"github.com/zekker6/mcp-helm/lib/helm_client"
	"github.com/zekker6/mcp-helm/lib/logger"
)

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read password file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

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

	repoUsername     = flag.String("username", "", "Username for authentication (OCI registries and HTTP repositories)")
	repoPasswordFile = flag.String("password-file", "", "Path to file containing password for authentication (OCI registries and HTTP repositories)")

	registryCredentials = flag.String("registry-credentials", "", "Path to registry credentials file (e.g., Docker config.json)")
	registryPlainHTTP   = flag.Bool("registry-plain-http", false, "Use plain HTTP for OCI registry connections (insecure)")

	tlsCertFile           = flag.String("tls-cert", "", "Path to TLS client certificate file for HTTP repositories")
	tlsKeyFile            = flag.String("tls-key", "", "Path to TLS client key file for HTTP repositories")
	tlsCAFile             = flag.String("tls-ca", "", "Path to CA certificate file for verifying HTTP repository servers")
	tlsInsecureSkipVerify = flag.Bool("tls-insecure-skip-verify", false, "Skip TLS certificate verification for HTTP repositories (insecure)")
	passCredentialsAll    = flag.Bool("pass-credentials-all", false, "Pass credentials to all domains when following redirects")
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

	helmClient := getHelmClient()
	s.AddTool(tools.NewListChartsTool(), tools.GetListChartsHandler(helmClient))
	s.AddTool(tools.NewListChartVersionsTool(), tools.GetListChartVersionsHandler(helmClient))
	s.AddTool(tools.NewGetLatestVersionOfChartTool(), tools.GetLatestVersionOfCharHandler(helmClient))
	s.AddTool(tools.NewGetChartValuesTool(), tools.GetChartValuesHandler(helmClient))
	s.AddTool(tools.NewGetChartContentsTool(), tools.GetChartContentsHandler(helmClient))
	s.AddTool(tools.NewGetChartDependenciesTool(), tools.GetChartDependenciesHandler(helmClient))
	s.AddTool(tools.NewGetChartImagesTool(), tools.GetChartImagesHandler(helmClient))

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

func getHelmClient() *helm_client.HelmClient {
	var clientOpts []helm_client.ClientOption

	// Validate auth flags - both must be provided together
	if (*repoUsername != "") != (*repoPasswordFile != "") {
		if *repoUsername != "" {
			logger.Error("Both -username and -password-file must be provided together (missing -password-file)")
		} else {
			logger.Error("Both -username and -password-file must be provided together (missing -username)")
		}
		os.Exit(1)
	}

	// Validate TLS client cert flags - both must be provided together
	if (*tlsCertFile != "") != (*tlsKeyFile != "") {
		if *tlsCertFile != "" {
			logger.Error("Both -tls-cert and -tls-key must be provided together (missing -tls-key)")
		} else {
			logger.Error("Both -tls-cert and -tls-key must be provided together (missing -tls-cert)")
		}
		os.Exit(1)
	}

	if *repoUsername != "" && *repoPasswordFile != "" {
		password, err := readPasswordFile(*repoPasswordFile)
		if err != nil {
			logger.Error("Failed to read password file", zap.Error(err))
			os.Exit(1)
		}
		clientOpts = append(clientOpts, helm_client.WithBasicAuth(*repoUsername, password))
	}

	if *registryCredentials != "" {
		clientOpts = append(clientOpts, helm_client.WithCredentialsFile(*registryCredentials))
	}
	if *registryPlainHTTP {
		clientOpts = append(clientOpts, helm_client.WithPlainHTTP(true))
	}

	if *tlsCertFile != "" && *tlsKeyFile != "" {
		clientOpts = append(clientOpts, helm_client.WithTLSClientConfig(*tlsCertFile, *tlsKeyFile))
	}
	if *tlsCAFile != "" {
		clientOpts = append(clientOpts, helm_client.WithCAFile(*tlsCAFile))
	}
	if *tlsInsecureSkipVerify {
		clientOpts = append(clientOpts, helm_client.WithInsecureSkipTLSVerify(true))
	}
	if *passCredentialsAll {
		clientOpts = append(clientOpts, helm_client.WithPassCredentialsAll(true))
	}

	helmClient, err := helm_client.NewClient(clientOpts...)
	if err != nil {
		logger.Error("Failed to create Helm client", zap.Error(err))
		os.Exit(1)
	}
	return helmClient
}
