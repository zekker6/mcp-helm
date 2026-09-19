package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"

	"github.com/zekker6/mcp-helm/internal/tools"
	"github.com/zekker6/mcp-helm/lib/helm_client"
	"github.com/zekker6/mcp-helm/lib/logger"
	"github.com/zekker6/mcp-helm/lib/telemetry"
)

// serviceName loses to OTEL_SERVICE_NAME when that is set.
const serviceName = "mcp-helm"

// Separate budgets: the two phases run in sequence, so a client that never
// disconnects must not spend the time the flush needs. Their sum has to stay
// under the deployment's terminationGracePeriodSeconds.
const (
	drainTimeout          = 10 * time.Second
	telemetryFlushTimeout = 5 * time.Second
)

// streamableEndpointPath is mcp-go's default http endpoint. Injecting an
// *http.Server takes over the routing that would otherwise mount it there.
const streamableEndpointPath = "/mcp"

// unroutedSpanName is a constant so neither the path nor the method of a probe
// can reach the traces backend as an operation name.
const unroutedSpanName = "HTTP"

// telemetryFieldNamespace prefixes the startup fields naming each signal's
// endpoint. "otel." is reserved for spec-defined attributes, and these records
// reach the collector through the log bridge like any other.
const telemetryFieldNamespace = telemetry.Namespace + "telemetry."

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
	heartbeatInterval    = flag.Duration("httpHeartbeatInterval", 30*time.Second, "Interval for sending heartbeat messages in seconds. Only used when -mode=http")
	sseKeepAliveInterval = flag.Duration("sseKeepAliveInterval", 30*time.Second, "Interval for sending keep-alive messages in seconds. Only used when -mode=sse")

	repoUsername     = flag.String("username", "", "Username for authentication (OCI registries and HTTP repositories)")
	repoPasswordFile = flag.String("password-file", "", "Path to file containing password for authentication (OCI registries and HTTP repositories)")

	registryCredentials = flag.String("registry-credentials", "", "Path to registry credentials file (e.g., Docker config.json)")
	registryPlainHTTP   = flag.Bool("registry-plain-http", false, "Use plain HTTP for OCI registry connections (insecure)")

	tlsCertFile           = flag.String("tls-cert", "", "Path to TLS client certificate file for HTTP repositories")
	tlsKeyFile            = flag.String("tls-key", "", "Path to TLS client key file for HTTP repositories")
	tlsCAFile             = flag.String("tls-ca", "", "Path to CA certificate file for verifying HTTP repository servers")
	tlsInsecureSkipVerify = flag.Bool("tls-insecure-skip-verify", false, "Skip TLS certificate verification for HTTP repositories (insecure)")
	passCredentialsAll    = flag.Bool("pass-credentials-all", false, "Pass credentials to all domains when following redirects")

	repoIndexMaxAge = flag.Duration("repo-index-max-age", helm_client.DefaultRepoIndexMaxAge, "How long to reuse a downloaded HTTP repository index before downloading it again. 0 downloads it on every request")
)

func main() {
	flag.Parse()

	logger.Init()

	if err := run(context.Background()); err != nil {
		logger.Stop()
		os.Exit(1)
	}

	logger.Stop()
}

// run holds the whole lifecycle, so failures are returned errors rather than an
// os.Exit and stay assertable from tests. It also owns the "fatal" record:
// logging that from main would put it after shutdownTelemetry closed the log
// provider, and the line explaining the exit would never leave the process.
func run(ctx context.Context) error {
	if err := validateTransportFlags(*mode, *httpListenAddr); err != nil {
		logFatal(err)

		return err
	}

	cfg, err := telemetry.ConfigFromEnv()
	if err != nil {
		err = fmt.Errorf("invalid telemetry configuration: %w", err)
		logFatal(err)

		return err
	}

	tel, err := telemetry.Setup(ctx, cfg, telemetry.ServiceInfo{Name: serviceName, Version: version})
	if err != nil {
		err = fmt.Errorf("failed to set up telemetry: %w", err)
		logFatal(err)

		return err
	}
	// Not an Init option: the logger already exists and Init returns early.
	logger.AttachProvider(tel.LoggerProvider())

	err = serveInstrumented(ctx, cfg, tel)
	if err != nil {
		logFatal(err)
	}

	shutdownErr := shutdownTelemetry(ctx, tel)
	if shutdownErr != nil {
		// stderr-only by construction: the provider that would export it is
		// the one that just failed to shut down.
		logFatal(shutdownErr)
	}

	return errors.Join(err, shutdownErr)
}

// serveInstrumented runs everything that needs the providers, giving run one
// place to log the failure and shut them down.
func serveInstrumented(ctx context.Context, cfg telemetry.Config, tel *telemetry.Telemetry) error {
	helmClient, err := newHelmClient()
	if err != nil {
		return err
	}

	instrumentation, err := tel.MCPInstrumentation(mcpTransport(*mode))
	if err != nil {
		return fmt.Errorf("failed to build MCP instrumentation: %w", err)
	}

	s := buildServer(helmClient, mcpServerOptions(cfg.Enabled, instrumentation, telemetry.ToolMiddleware)...)

	logger.Info("Starting MCP Helm server", append([]zap.Field{
		zap.String("version", version),
		zap.String("commit", commit),
		zap.String("date", date),
		zap.String("mode", *mode),
		zap.String("httpListenAddr", *httpListenAddr),
	}, telemetryFields(cfg)...)...)

	return serve(ctx, s, cfg.Enabled)
}

func logFatal(err error) {
	logger.Error("fatal", zap.Error(err))
}

// serve runs the transport selected by -mode until it fails or ctx is done.
//
// The signal handler here is the only one in the process, taken over from
// mcp-go's transport helpers so the telemetry flush can run after the drain.
// The injected servers get an empty Addr: Start fills it in, and a value that
// disagrees is rejected as a conflicting listen address.
func serve(ctx context.Context, s *server.MCPServer, otelEnabled bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *mode {
	case "stdio":
		return serveStdio(ctx, s)
	case "sse":
		return serveHTTPTransport(ctx, stop, newSSETransport(&http.Server{}, s, otelEnabled), *httpListenAddr, "SSE")
	case "http":
		return serveHTTPTransport(ctx, stop, newStreamableHTTPTransport(&http.Server{}, s, otelEnabled), *httpListenAddr, "HTTP")
	default:
		// validateTransportFlags rejected anything else, so this is a bug. An
		// error keeps it from exiting 0 as if the server had run.
		return fmt.Errorf("unreachable: mode %q passed validation", *mode)
	}
}

// newSSETransport builds the sse-mode transport serving through srv. The SSE
// server routes from its own ServeHTTP, so it is wrapped directly rather than
// mounted on a mux.
func newSSETransport(srv *http.Server, s *server.MCPServer, otelEnabled bool) *server.SSEServer {
	opts := []server.SSEOption{server.WithHTTPServer(srv)}
	if *sseKeepAliveInterval > 0 {
		opts = append(opts, server.WithKeepAliveInterval(*sseKeepAliveInterval))
	}

	t := server.NewSSEServer(s, opts...)
	srv.Handler = buildHTTPHandler(t, otelEnabled, transportRoutes{
		paths:  []string{t.CompleteSsePath(), t.CompleteMessagePath()},
		stream: t.CompleteSsePath(),
	})

	return t
}

// newStreamableHTTPTransport builds the http-mode transport serving through
// srv, the only place its handler can be wrapped.
//
// Injecting the server takes over the routing Start does by default: it serves
// whatever Handler srv carries and never fills one in, so a nil Handler here
// would serve 404s.
func newStreamableHTTPTransport(srv *http.Server, s *server.MCPServer, otelEnabled bool) *server.StreamableHTTPServer {
	opts := []server.StreamableHTTPOption{server.WithStreamableHTTPServer(srv)}
	if *heartbeatInterval > 0 {
		opts = append(opts, server.WithHeartbeatInterval(*heartbeatInterval))
	}

	t := server.NewStreamableHTTPServer(s, opts...)

	mux := http.NewServeMux()
	mux.Handle(streamableEndpointPath, t)
	srv.Handler = buildHTTPHandler(mux, otelEnabled, transportRoutes{
		paths:  []string{streamableEndpointPath},
		stream: streamableEndpointPath,
	})

	return t
}

// serveStdio drives the stdio transport from ctx.
//
// server.ServeStdio is not used: it builds its own context and signal handler,
// so ctx could neither stop it nor sequence the telemetry flush after it.
// Listen reports a cancelled ctx as context.Canceled, which is a clean exit.
func serveStdio(ctx context.Context, s *server.MCPServer) error {
	if err := server.NewStdioServer(s).Listen(ctx, os.Stdin, os.Stdout); err != nil &&
		!errors.Is(err, context.Canceled) {
		return fmt.Errorf("failed to start MCP server in stdio mode: %w", err)
	}

	return nil
}

// httpTransport is the part of the sse and http servers the lifecycle drives.
type httpTransport interface {
	Start(addr string) error
	Shutdown(ctx context.Context) error
}

// serveHTTPTransport starts srv and drains it within drainTimeout once ctx is
// done. stop is the signal-derived cancel, called before the drain so a second
// signal reaches the default disposition.
func serveHTTPTransport(ctx context.Context, stop context.CancelFunc, srv httpTransport, addr, name string) error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.Start(addr)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("failed to start %s server: %w", name, err)
		}
		return nil
	case <-ctx.Done():
	}

	// A second SIGTERM must be able to kill a server that is not draining.
	stop()

	logger.Info("Shutting down MCP Helm server", zap.String("mode", *mode))

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("failed to shut down %s server: %w", name, err)
	}

	return nil
}

// shutdownTelemetry flushes zap before closing the providers its OpenTelemetry
// core writes into. The reverse order drops every record emitted while the
// transport was draining.
func shutdownTelemetry(ctx context.Context, tel *telemetry.Telemetry) error {
	logger.Stop()

	// ctx is normally already cancelled, and the exporters need a live one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
	defer cancel()

	if err := tel.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shut down telemetry: %w", err)
	}

	return nil
}

// telemetryFields reports where each signal is going, so a collector pointed at
// the wrong endpoint shows up in the first log line instead of as missing data.
func telemetryFields(cfg telemetry.Config) []zap.Field {
	fields := []zap.Field{zap.Bool("otelEnabled", cfg.Enabled)}
	if !cfg.Enabled {
		return fields
	}

	signals := []struct {
		name   string
		signal telemetry.SignalConfig
	}{
		{"traces", cfg.Traces},
		{"metrics", cfg.Metrics},
		{"logs", cfg.Logs},
	}
	for _, s := range signals {
		fields = append(fields,
			zap.String(telemetryFieldNamespace+s.name+".protocol", string(s.signal.Protocol)),
			zap.String(telemetryFieldNamespace+s.name+".endpoint", s.signal.Endpoint),
		)
	}

	return fields
}

// validateTransportFlags rejects an unknown -mode and a missing listen address
// for the two HTTP-based transports.
func validateTransportFlags(mode, httpListenAddr string) error {
	switch mode {
	case "stdio":
	case "sse", "http":
		if httpListenAddr == "" {
			return fmt.Errorf("HTTP listen address must be specified in %s mode. Use -httpListenAddr to set it", mode)
		}
	default:
		return fmt.Errorf("invalid mode specified: %s. Supported modes are 'stdio', 'sse', and 'http'", mode)
	}

	return nil
}

// buildServer constructs the MCP server and registers every tool. Callers pass
// extra options, which are applied after the defaults.
func buildServer(helmClient *helm_client.HelmClient, opts ...server.ServerOption) *server.MCPServer {
	serverOpts := append([]server.ServerOption{
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	}, opts...)

	s := server.NewMCPServer(
		"Helm MCP Server",
		fmt.Sprintf("v%s (commit: %s, date: %s)", version, commit, date),
		serverOpts...,
	)

	s.AddTool(tools.NewListChartsTool(), tools.GetListChartsHandler(helmClient))
	s.AddTool(tools.NewListChartVersionsTool(), tools.GetListChartVersionsHandler(helmClient))
	s.AddTool(tools.NewGetLatestVersionOfChartTool(), tools.GetLatestVersionOfCharHandler(helmClient))
	s.AddTool(tools.NewGetChartValuesTool(), tools.GetChartValuesHandler(helmClient))
	s.AddTool(tools.NewGetChartContentsTool(), tools.GetChartContentsHandler(helmClient))
	s.AddTool(tools.NewGetChartDependenciesTool(), tools.GetChartDependenciesHandler(helmClient))
	s.AddTool(tools.NewGetChartImagesTool(), tools.GetChartImagesHandler(helmClient))

	return s
}

// mcpServerOptions installs the MCP instrumentation and the tool middleware.
//
// Order matters: mcp-go applies tool middlewares in reverse registration order,
// and the instrumentation registers the one opening tool.<name>, so it has to
// come first for the middleware to run inside that span.
func mcpServerOptions(
	enabled bool,
	instrumentation *telemetry.MCPInstrumentation,
	toolMiddleware server.ToolHandlerMiddleware,
) []server.ServerOption {
	if !enabled {
		return nil
	}

	return append(instrumentation.ServerOptions(), server.WithToolHandlerMiddleware(toolMiddleware))
}

// mcpTransport maps -mode to the transport the MCP conventions record.
func mcpTransport(mode string) telemetry.Transport {
	if mode == "stdio" {
		return telemetry.TransportStdio
	}

	return telemetry.TransportHTTP
}

// transportRoutes bounds the span names otelhttp produces to the real routes,
// and names the route whose GET opens a long-lived stream.
type transportRoutes struct {
	paths  []string
	stream string
}

// isStream reports whether r opens a long-lived stream. Tracing one would send
// hour-long spans and put session durations in the request-latency histogram.
func (t transportRoutes) isStream(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == t.stream
}

// spanName names a request by its method and route. Unrouted requests get a
// constant instead: net/http accepts any RFC 7230 token as a method, so a
// scanner inventing one per request would mint one operation name per request.
func (t transportRoutes) spanName(r *http.Request) string {
	for _, route := range t.paths {
		if route == r.URL.Path {
			return r.Method + " " + route
		}
	}

	return unroutedSpanName
}

// buildHTTPHandler is the single place the sse and http transport handlers get
// wrapped.
func buildHTTPHandler(next http.Handler, otelEnabled bool, routes transportRoutes) http.Handler {
	if !otelEnabled {
		return next
	}

	return otelhttp.NewHandler(telemetry.AnnotateJSONRPC(next), serviceName,
		otelhttp.WithServerName(otelServerName(*httpListenAddr)),
		otelhttp.WithFilter(func(r *http.Request) bool { return !routes.isStream(r) }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return routes.spanName(r) }),
	)
}

// otelServerName pins the server.address and server.port otelhttp puts on
// http.server.request.duration. Left unset it derives both from the
// client-controlled Host header (otelhttp internal/semconv/server.go:363): one
// series per distinct value a probe sends. The port is included because
// otelhttp falls back to the header for it alone when the name carries none.
func otelServerName(listenAddr string) string {
	if strings.HasPrefix(listenAddr, ":") {
		return serviceName + listenAddr
	}

	return listenAddr
}

// validateHelmAuthFlags rejects the credential flag pairs that are only
// meaningful together.
func validateHelmAuthFlags(username, passwordFile, tlsCert, tlsKey string) error {
	if (username != "") != (passwordFile != "") {
		if username != "" {
			return errors.New("both -username and -password-file must be provided together (missing -password-file)")
		}
		return errors.New("both -username and -password-file must be provided together (missing -username)")
	}

	if (tlsCert != "") != (tlsKey != "") {
		if tlsCert != "" {
			return errors.New("both -tls-cert and -tls-key must be provided together (missing -tls-key)")
		}
		return errors.New("both -tls-cert and -tls-key must be provided together (missing -tls-cert)")
	}

	return nil
}

func newHelmClient() (*helm_client.HelmClient, error) {
	if err := validateHelmAuthFlags(*repoUsername, *repoPasswordFile, *tlsCertFile, *tlsKeyFile); err != nil {
		return nil, err
	}

	var clientOpts []helm_client.ClientOption

	if *repoIndexMaxAge < 0 {
		return nil, fmt.Errorf("-repo-index-max-age must not be negative: %s", *repoIndexMaxAge)
	}
	clientOpts = append(clientOpts, helm_client.WithRepoIndexMaxAge(*repoIndexMaxAge))

	if *repoUsername != "" && *repoPasswordFile != "" {
		password, err := readPasswordFile(*repoPasswordFile)
		if err != nil {
			return nil, err
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
		return nil, fmt.Errorf("failed to create Helm client: %w", err)
	}

	return helmClient, nil
}
