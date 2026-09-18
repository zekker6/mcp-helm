package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/zekker6/mcp-helm/lib/logger"
	"github.com/zekker6/mcp-helm/lib/telemetry"
)

func TestValidateHelmAuthFlags(t *testing.T) {
	tests := []struct {
		name         string
		username     string
		passwordFile string
		tlsCert      string
		tlsKey       string
		wantErr      string
	}{
		{
			name: "no credentials",
		},
		{
			name:         "username with password file",
			username:     "user",
			passwordFile: "/tmp/password",
		},
		{
			name:    "tls cert with tls key",
			tlsCert: "/tmp/cert.pem",
			tlsKey:  "/tmp/key.pem",
		},
		{
			name:     "username without password file",
			username: "user",
			wantErr:  "missing -password-file",
		},
		{
			name:         "password file without username",
			passwordFile: "/tmp/password",
			wantErr:      "missing -username",
		},
		{
			name:    "tls cert without tls key",
			tlsCert: "/tmp/cert.pem",
			wantErr: "missing -tls-key",
		},
		{
			name:    "tls key without tls cert",
			tlsKey:  "/tmp/key.pem",
			wantErr: "missing -tls-cert",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHelmAuthFlags(tt.username, tt.passwordFile, tt.tlsCert, tt.tlsKey)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestValidateTransportFlags(t *testing.T) {
	tests := []struct {
		name           string
		mode           string
		httpListenAddr string
		wantErr        string
	}{
		{name: "stdio", mode: "stdio"},
		{name: "stdio without listen address", mode: "stdio"},
		{name: "sse", mode: "sse", httpListenAddr: ":8012"},
		{name: "http", mode: "http", httpListenAddr: ":8012"},
		{
			name:    "sse without listen address",
			mode:    "sse",
			wantErr: "HTTP listen address must be specified in sse mode",
		},
		{
			name:    "http without listen address",
			mode:    "http",
			wantErr: "HTTP listen address must be specified in http mode",
		},
		{
			name:           "unknown mode",
			mode:           "grpc",
			httpListenAddr: ":8012",
			wantErr:        "invalid mode specified: grpc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTransportFlags(tt.mode, tt.httpListenAddr)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestBuildServerRegistersAllTools(t *testing.T) {
	s := buildServer(nil)

	want := []string{
		"get_chart_contents",
		"get_chart_dependencies",
		"get_chart_images",
		"get_chart_values",
		"get_latest_version_of_chart",
		"list_chart_versions",
		"list_repository_charts",
	}

	registered := s.ListTools()
	if len(registered) != len(want) {
		t.Fatalf("expected %d tools registered, got %d", len(want), len(registered))
	}

	got := make([]string, 0, len(registered))
	for name, tool := range registered {
		if tool.Handler == nil {
			t.Errorf("tool %q registered without a handler", name)
		}
		got = append(got, name)
	}
	sort.Strings(got)

	for i, name := range want {
		if got[i] != name {
			t.Fatalf("registered tools = %v, want %v", got, want)
		}
	}
}

// withFlag points a flag-backed global at v for the duration of the test.
func withFlag[T any](t *testing.T, target *T, v T) {
	t.Helper()

	original := *target
	*target = v
	t.Cleanup(func() { *target = original })
}

func TestRunRejectsInvalidTelemetryConfig(t *testing.T) {
	withFlag(t, mode, "stdio")
	t.Setenv(telemetry.EnvEnabled, "true")
	t.Setenv(telemetry.EnvEndpoint, "ftp://collector:4318")
	t.Setenv(telemetry.EnvTracesEndpoint, "")
	t.Setenv(telemetry.EnvMetricsEndpoint, "")
	t.Setenv(telemetry.EnvLogsEndpoint, "")

	err := run(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unsupported endpoint scheme, got nil")
	}
	if !strings.Contains(err.Error(), telemetry.EnvEndpoint) {
		t.Fatalf("expected the error to name %s, got %q", telemetry.EnvEndpoint, err.Error())
	}
}

// TestRunReturnsOnContextCancellation covers the shutdown path with telemetry
// off: run must return rather than block on stdin, so the process can exit.
func TestRunReturnsOnContextCancellation(t *testing.T) {
	withFlag(t, mode, "stdio")
	t.Setenv(telemetry.EnvEnabled, "false")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
	}
}

// logCollector is a stand-in OTLP/HTTP receiver that keeps the body of every
// request, so a test can assert which records actually left the process.
type logCollector struct {
	*httptest.Server

	mu       sync.Mutex
	requests []collectedRequest
}

type collectedRequest struct {
	path string
	body []byte
}

func newLogCollector(t *testing.T) *logCollector {
	t.Helper()

	c := &logCollector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		c.mu.Lock()
		c.requests = append(c.requests, collectedRequest{path: r.URL.Path, body: body})
		c.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)

	return c
}

// received reports whether any request to path carried want. The OTLP payload
// is uncompressed protobuf, so a string body appears in it verbatim.
func (c *logCollector) received(path, want string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, req := range c.requests {
		if req.path == path && bytes.Contains(req.body, []byte(want)) {
			return true
		}
	}

	return false
}

func (c *logCollector) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	paths := make([]string, 0, len(c.requests))
	for _, req := range c.requests {
		paths = append(paths, req.path)
	}

	return paths
}

// fakeTransport stands in for the sse and http servers. Its Shutdown logs, so
// a test can prove the teardown order still delivers that record.
type fakeTransport struct {
	started     chan struct{}
	stopped     chan struct{}
	shutdownMsg string
}

func (f *fakeTransport) Start(string) error {
	close(f.started)
	<-f.stopped

	return http.ErrServerClosed
}

func (f *fakeTransport) Shutdown(context.Context) error {
	logger.Info(f.shutdownMsg)
	close(f.stopped)

	return nil
}

// TestShutdownDeliversLogsEmittedWhileDraining pins the teardown order:
// telemetry.Shutdown closes the provider the zap bridge writes into, so running
// it before the drain would silently drop every line the shutdown path emits.
func TestShutdownDeliversLogsEmittedWhileDraining(t *testing.T) {
	col := newLogCollector(t)

	withFlag(t, mode, "http")
	t.Setenv(telemetry.EnvEnabled, "true")
	t.Setenv(telemetry.EnvEndpoint, col.URL)
	t.Setenv(telemetry.EnvTracesEndpoint, "")
	t.Setenv(telemetry.EnvMetricsEndpoint, "")
	t.Setenv(telemetry.EnvLogsEndpoint, "")
	// Pin the wire format: the assertion reads the payload as plain protobuf.
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")

	cfg, err := telemetry.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	tel, err := telemetry.Setup(context.Background(), cfg, telemetry.ServiceInfo{Name: "mcp-helm", Version: "test"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	logger.AttachProvider(tel.LoggerProvider())

	const msg = "transport is draining"
	transport := &fakeTransport{
		started:     make(chan struct{}),
		stopped:     make(chan struct{}),
		shutdownMsg: msg,
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveHTTPTransport(ctx, cancel, transport, "127.0.0.1:0", "test") }()

	<-transport.started
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveHTTPTransport: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveHTTPTransport did not return after its context was cancelled")
	}

	if err := shutdownTelemetry(context.Background(), tel); err != nil {
		t.Fatalf("shutdownTelemetry: %v", err)
	}

	if !col.received("/v1/logs", msg) {
		t.Fatalf("collector never received the %q record emitted during shutdown, paths seen: %v", msg, col.paths())
	}
}

func TestTelemetryFields(t *testing.T) {
	disabled := telemetryFields(telemetry.Config{})
	if len(disabled) != 1 {
		t.Fatalf("expected only the enabled field when telemetry is off, got %v", disabled)
	}
	if disabled[0].Key != "otelEnabled" || disabled[0].Integer != 0 {
		t.Fatalf("expected otelEnabled=false, got %v", disabled[0])
	}

	enabled := telemetryFields(telemetry.Config{
		Enabled: true,
		Traces:  telemetry.SignalConfig{Protocol: telemetry.ProtocolGRPC, Endpoint: "collector:4317"},
		Metrics: telemetry.SignalConfig{Protocol: telemetry.ProtocolHTTP, Endpoint: "http://collector:4318/v1/metrics"},
		Logs:    telemetry.SignalConfig{Protocol: telemetry.ProtocolHTTP, Endpoint: "http://collector:4318/v1/logs"},
	})

	got := make(map[string]string, len(enabled))
	for _, f := range enabled {
		got[f.Key] = f.String
	}

	want := map[string]string{
		"cloud.zekker.telemetry.traces.protocol":  "grpc",
		"cloud.zekker.telemetry.traces.endpoint":  "collector:4317",
		"cloud.zekker.telemetry.metrics.protocol": "http",
		"cloud.zekker.telemetry.metrics.endpoint": "http://collector:4318/v1/metrics",
		"cloud.zekker.telemetry.logs.protocol":    "http",
		"cloud.zekker.telemetry.logs.endpoint":    "http://collector:4318/v1/logs",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("field %s = %q, want %q", key, got[key], value)
		}
	}
}

// traceProbeTool is registered on top of the real tools so a tools/call can be
// traced without reaching a Helm repository.
const traceProbeTool = "trace_probe"

// callToolMessage is a tools/call for traceProbeTool.
const callToolMessage = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
	traceProbeTool + `","arguments":{}}}`

// newRecordingTracerProvider returns a provider whose spans are kept in memory.
func newRecordingTracerProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	return tp, sr
}

// passthroughMiddleware stands in for the telemetry tool middleware in tests
// that only care about spans.
func passthroughMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc { return next }

// newTracedServer builds the real server with the options run() would pass,
// plus a handler that always succeeds.
func newTracedServer(t *testing.T, enabled bool, tracer trace.Tracer, mw server.ToolHandlerMiddleware) *server.MCPServer {
	t.Helper()

	s := buildServer(nil, mcpServerOptions(enabled, tracer, mw)...)
	s.AddTool(
		mcp.NewTool(traceProbeTool, mcp.WithDescription("records a span and returns")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		},
	)

	return s
}

// spanNamed returns the single recorded span with the given name.
func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	var found []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == name {
			found = append(found, span)
		}
	}

	if len(found) != 1 {
		t.Fatalf("expected exactly one %q span, got %d out of %v", name, len(found), spanNames(spans))
	}

	return found[0]
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}

	return names
}

func TestBuildServerTracesToolCalls(t *testing.T) {
	tp, sr := newRecordingTracerProvider(t)
	s := newTracedServer(t, true, tp.Tracer("test"), passthroughMiddleware)

	resp := s.HandleMessage(context.Background(), json.RawMessage(callToolMessage))
	if err, isErr := resp.(mcp.JSONRPCError); isErr {
		t.Fatalf("tools/call failed: %s", err.Error.Message)
	}

	spans := sr.Ended()
	message := spanNamed(t, spans, "mcp.tools/call")
	tool := spanNamed(t, spans, "tool."+traceProbeTool)

	if message.SpanKind() != trace.SpanKindServer {
		t.Errorf("mcp.tools/call kind = %v, want server", message.SpanKind())
	}
	if tool.Parent().SpanID() != message.SpanContext().SpanID() {
		t.Fatalf("tool.%s parent = %s, want the mcp.tools/call span %s",
			traceProbeTool, tool.Parent().SpanID(), message.SpanContext().SpanID())
	}
}

// TestToolMiddlewareRunsInsideTheToolSpan pins the order mcpServerOptions
// returns. mcp-go applies tool middlewares in reverse registration order, so
// ours registered first would run around mcp.tools/call instead of inside
// tool.<name>, and every per-call log line would name the wrong span.
func TestToolMiddlewareRunsInsideTheToolSpan(t *testing.T) {
	tp, sr := newRecordingTracerProvider(t)

	var seen trace.SpanContext
	probe := func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen = trace.SpanContextFromContext(ctx)
			return next(ctx, request)
		}
	}

	s := newTracedServer(t, true, tp.Tracer("test"), probe)

	resp := s.HandleMessage(context.Background(), json.RawMessage(callToolMessage))
	if err, isErr := resp.(mcp.JSONRPCError); isErr {
		t.Fatalf("tools/call failed: %s", err.Error.Message)
	}

	spans := sr.Ended()
	message := spanNamed(t, spans, "mcp.tools/call")
	tool := spanNamed(t, spans, "tool."+traceProbeTool)

	if seen.SpanID() == message.SpanContext().SpanID() {
		t.Fatal("tool middleware ran outside the tool span: it is registered before the tracer")
	}
	if seen.SpanID() != tool.SpanContext().SpanID() {
		t.Fatalf("tool middleware span = %s, want the tool.%s span %s",
			seen.SpanID(), traceProbeTool, tool.SpanContext().SpanID())
	}
}

func TestBuildServerRecordsNoSpansWhenTelemetryIsDisabled(t *testing.T) {
	tp, sr := newRecordingTracerProvider(t)
	s := newTracedServer(t, false, tp.Tracer("test"), passthroughMiddleware)

	resp := s.HandleMessage(context.Background(), json.RawMessage(callToolMessage))
	if err, isErr := resp.(mcp.JSONRPCError); isErr {
		t.Fatalf("tools/call failed: %s", err.Error.Message)
	}

	if spans := sr.Ended(); len(spans) != 0 {
		t.Fatalf("expected no spans with telemetry disabled, got %v", spanNames(spans))
	}
}

// TestMCPSpanParentsToTheHTTPSpan pins the decision not to install an MCP
// propagator: with one, mcp-go extracts the inbound traceparent and starts
// mcp.tools/call under the remote span, a sibling of the otelhttp server span
// rather than its child.
func TestMCPSpanParentsToTheHTTPSpan(t *testing.T) {
	tp, sr := newRecordingTracerProvider(t)
	s := newTracedServer(t, true, tp.Tracer("test"), passthroughMiddleware)

	handler := otelhttp.NewHandler(
		server.NewStreamableHTTPServer(s, server.WithStateLess(true)),
		"mcp",
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(propagation.TraceContext{}),
	)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	const (
		remoteTraceID = "0af7651916cd43dd8448eb211c80319c"
		remoteSpanID  = "b7ad6b7169203331"
	)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpSrv.URL+"/mcp",
		strings.NewReader(callToolMessage))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("traceparent", "00-"+remoteTraceID+"-"+remoteSpanID+"-01")

	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST tools/call: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST tools/call: status %d, body %s", resp.StatusCode, body)
	}

	spans := sr.Ended()
	message := spanNamed(t, spans, "mcp.tools/call")
	// otelhttp names the server span after the request method.
	httpSpan := spanNamed(t, spans, http.MethodPost)

	if message.SpanContext().TraceID().String() != remoteTraceID {
		t.Errorf("mcp.tools/call trace = %s, want the inbound trace %s",
			message.SpanContext().TraceID(), remoteTraceID)
	}
	if message.Parent().SpanID().String() == remoteSpanID {
		t.Fatal("mcp.tools/call parented to the remote span: an MCP propagator is installed")
	}
	if message.Parent().SpanID() != httpSpan.SpanContext().SpanID() {
		t.Fatalf("mcp.tools/call parent = %s, want the otelhttp span %s",
			message.Parent().SpanID(), httpSpan.SpanContext().SpanID())
	}
}

// httpDurationMetric is the instrument otelhttp records per served request.
const httpDurationMetric = "http.server.request.duration"

// initializeMessage is the one request the streamable transport accepts without
// a session id, which makes it the cheapest way to drive a full POST.
const initializeMessage = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
	`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`

// withRecordingProviders points the OpenTelemetry globals at in-memory
// recorders, which is where otelhttp resolves its meter and tracer from.
//
// They are not restored: OpenTelemetry refuses to reinstall its delegating
// default over a real provider, and every test that cares installs its own.
func withRecordingProviders(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()

	tp, sr := newRecordingTracerProvider(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down meter provider: %v", err)
		}
	})

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return sr, reader
}

// httpDurationPoints returns the data points otelhttp recorded for served
// requests.
func httpDurationPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != httpDurationMetric {
				continue
			}

			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", httpDurationMetric, m.Data)
			}

			return hist.DataPoints
		}
	}

	return nil
}

// serveStreamableTransport wires the http-mode transport exactly as serve()
// does and puts its handler behind a test listener.
func serveStreamableTransport(t *testing.T, otelEnabled bool) *httptest.Server {
	t.Helper()

	srv := &http.Server{} //nolint:gosec // no timeouts, matching the production server
	transport := newStreamableHTTPTransport(srv, newTracedServer(t, false, nil, nil), otelEnabled)
	t.Cleanup(func() {
		if err := transport.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down streamable transport: %v", err)
		}
	})

	httpSrv := httptest.NewServer(srv.Handler)
	t.Cleanup(httpSrv.Close)

	return httpSrv
}

// postJSONRPC sends body to the transport endpoint the way an MCP client would.
func postJSONRPC(t *testing.T, httpSrv *httptest.Server, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		httpSrv.URL+streamableEndpointPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", streamableEndpointPath, err)
	}

	return resp
}

func TestBuildHTTPHandlerRecordsRequestDuration(t *testing.T) {
	sr, reader := withRecordingProviders(t)
	httpSrv := serveStreamableTransport(t, true)

	resp := postJSONRPC(t, httpSrv, initializeMessage)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST initialize: status %d, body %s", resp.StatusCode, body)
	}

	points := httpDurationPoints(t, reader)
	if len(points) != 1 {
		t.Fatalf("expected one %s data point, got %d", httpDurationMetric, len(points))
	}
	if points[0].Count != 1 {
		t.Errorf("%s count = %d, want 1", httpDurationMetric, points[0].Count)
	}

	// The span is named by method and route, not by the raw path.
	spanNamed(t, sr.Ended(), http.MethodPost+" "+streamableEndpointPath)
}

// TestBuildHTTPHandlerNamesUnroutedRequests pins the cardinality guard on the
// span name: net/http accepts any RFC 7230 token as a method, so echoing an
// unrouted request's method lets a scanner mint one operation name per probe.
func TestBuildHTTPHandlerNamesUnroutedRequests(t *testing.T) {
	sr, _ := withRecordingProviders(t)
	httpSrv := serveStreamableTransport(t, true)

	req, err := http.NewRequestWithContext(
		context.Background(), "ZZZPROBE", httpSrv.URL+"/not-a-route", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("probe an unrouted path: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close the response: %v", err)
	}

	spanNamed(t, sr.Ended(), unroutedSpanName)
}

// TestBuildHTTPHandlerPinsTheServerAttributes is the metric counterpart. Unless
// a server name is set, otelhttp reads server.address and server.port from the
// Host header and puts both on http.server.request.duration, so probes varying
// that header would open one series each.
func TestBuildHTTPHandlerPinsTheServerAttributes(t *testing.T) {
	_, reader := withRecordingProviders(t)
	httpSrv := serveStreamableTransport(t, true)

	for _, host := range []string{"probe-one.example", "probe-two.example:9999"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			httpSrv.URL+streamableEndpointPath, strings.NewReader(initializeMessage))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		// Host is the header, not the address dialled: httpSrv.URL still
		// decides where the request goes.
		req.Host = host

		resp, err := httpSrv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST with Host %q: %v", host, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close the response: %v", err)
		}
	}

	points := httpDurationPoints(t, reader)
	if len(points) != 1 {
		t.Fatalf("two Host headers produced %d %s series, want 1: %v",
			len(points), httpDurationMetric, pointAttributes(points))
	}

	wantAddress, wantPort := serviceName, int64(8012)
	if got, _ := points[0].Attributes.Value("server.address"); got.AsString() != wantAddress {
		t.Errorf("server.address = %q, want %q", got.AsString(), wantAddress)
	}
	if got, _ := points[0].Attributes.Value("server.port"); got.AsInt64() != wantPort {
		t.Errorf("server.port = %d, want %d", got.AsInt64(), wantPort)
	}
}

// pointAttributes renders one attribute set per data point, so a cardinality
// failure names the attribute that split the series.
func pointAttributes(points []metricdata.HistogramDataPoint[float64]) []string {
	rendered := make([]string, 0, len(points))
	for _, p := range points {
		rendered = append(rendered, p.Attributes.Encoded(attribute.DefaultEncoder()))
	}

	return rendered
}

func TestOtelServerName(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		want       string
	}{
		{"port only gets the service name", ":8012", "mcp-helm:8012"},
		{"host and port pass through", "0.0.0.0:9000", "0.0.0.0:9000"},
		{"ipv6 passes through", "[::1]:9000", "[::1]:9000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := otelServerName(tt.listenAddr); got != tt.want {
				t.Errorf("otelServerName(%q) = %q, want %q", tt.listenAddr, got, tt.want)
			}
		})
	}
}

// TestBuildHTTPHandlerFiltersStreamRequests covers the filter: a GET on the
// transport endpoint lives as long as the client session, so tracing it would
// ship hour-long spans and put session durations in the latency histogram.
func TestBuildHTTPHandlerFiltersStreamRequests(t *testing.T) {
	sr, reader := withRecordingProviders(t)
	httpSrv := serveStreamableTransport(t, true)

	// A 200 proves the request reached the transport; the stream is then
	// dropped, because it would otherwise stay open for the whole session.
	resp := getStream(t, httpSrv, streamableEndpointPath)
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close the stream: %v", err)
	}

	if spans := sr.Ended(); len(spans) != 0 {
		t.Errorf("expected no span for a stream request, got %v", spanNames(spans))
	}
	if points := httpDurationPoints(t, reader); len(points) != 0 {
		t.Errorf("expected no %s data point for a stream request, got %d", httpDurationMetric, len(points))
	}
}

func TestBuildHTTPHandlerPassesRequestsThrough(t *testing.T) {
	sr, reader := withRecordingProviders(t)

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})

	routes := transportRoutes{paths: []string{streamableEndpointPath}, stream: streamableEndpointPath}
	handler := buildHTTPHandler(next, false, routes)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, streamableEndpointPath, nil))

	if !called {
		t.Fatal("expected the wrapped handler to be called")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected status %d, got %d", http.StatusTeapot, rec.Code)
	}

	if spans := sr.Ended(); len(spans) != 0 {
		t.Errorf("expected no spans with telemetry disabled, got %v", spanNames(spans))
	}
	if points := httpDurationPoints(t, reader); len(points) != 0 {
		t.Errorf("expected no %s data point with telemetry disabled, got %d", httpDurationMetric, len(points))
	}
}

// TestWrappedHandlerKeepsStreaming covers what otelhttp could break: it replaces
// the ResponseWriter, and both transports need http.Flusher on it. Without it
// the streamable transport buffers every SSE response and sse refuses outright.
func TestWrappedHandlerKeepsStreaming(t *testing.T) {
	t.Run("wrapped writer still flushes", func(t *testing.T) {
		withRecordingProviders(t)

		released := make(chan struct{})
		var flushable bool
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flusher, ok := w.(http.Flusher)
			flushable = ok
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: first\n\n")
			flusher.Flush()

			<-released
			_, _ = io.WriteString(w, "event: second\n\n")
			flusher.Flush()
		})

		routes := transportRoutes{paths: []string{streamableEndpointPath}, stream: streamableEndpointPath}
		httpSrv := httptest.NewServer(buildHTTPHandler(next, true, routes))
		t.Cleanup(httpSrv.Close)

		// POST, so the request is wrapped rather than skipped by the filter.
		resp := postJSONRPC(t, httpSrv, initializeMessage)
		defer func() { _ = resp.Body.Close() }()

		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		close(released)
		if err != nil {
			t.Fatalf("read the flushed chunk: %v", err)
		}
		if !flushable {
			t.Fatal("the wrapped ResponseWriter does not implement http.Flusher")
		}
		if strings.TrimSpace(line) != "event: first" {
			t.Fatalf("first flushed line = %q, want %q", strings.TrimSpace(line), "event: first")
		}
	})

	t.Run("sse stream reaches the client", func(t *testing.T) {
		withRecordingProviders(t)

		srv := &http.Server{} //nolint:gosec // no timeouts, matching the production server
		transport := newSSETransport(srv, newTracedServer(t, false, nil, nil), true)
		t.Cleanup(func() {
			if err := transport.Shutdown(context.Background()); err != nil {
				t.Errorf("shut down sse transport: %v", err)
			}
		})

		httpSrv := httptest.NewServer(srv.Handler)
		t.Cleanup(httpSrv.Close)

		resp := getStream(t, httpSrv, transport.CompleteSsePath())
		defer func() { _ = resp.Body.Close() }()

		// The endpoint event is written and flushed before the handler blocks
		// on the session, so receiving it proves the stream is not buffered.
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			t.Fatalf("read the first SSE event: %v", err)
		}
		if strings.TrimSpace(line) != "event: endpoint" {
			t.Fatalf("first SSE line = %q, want %q", strings.TrimSpace(line), "event: endpoint")
		}
	})
}

// readUntil scans body until a line contains want, failing the test if the
// stream ends or its deadline passes first.
func readUntil(t *testing.T, body io.Reader, want string) {
	t.Helper()

	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), want) {
			return
		}
	}

	t.Fatalf("stream ended without a line containing %q: %v", want, scanner.Err())
}

// TestIntervalFlagsStillReachTheTransports covers -sseKeepAliveInterval and
// -httpHeartbeatInterval. Injecting an *http.Server rebuilt how both transports
// are constructed, so their pings are asserted on the wire.
func TestIntervalFlagsStillReachTheTransports(t *testing.T) {
	// The ping both transports emit on their stream.
	const ping = `"method":"ping"`

	t.Run("sse keep-alive", func(t *testing.T) {
		withRecordingProviders(t)
		withFlag(t, sseKeepAliveInterval, 25*time.Millisecond)

		srv := &http.Server{} //nolint:gosec // no timeouts, matching the production server
		transport := newSSETransport(srv, newTracedServer(t, false, nil, nil), true)
		t.Cleanup(func() {
			if err := transport.Shutdown(context.Background()); err != nil {
				t.Errorf("shut down sse transport: %v", err)
			}
		})
		if srv.Handler == nil {
			t.Fatal("sse transport left the injected server without a handler")
		}

		httpSrv := httptest.NewServer(srv.Handler)
		t.Cleanup(httpSrv.Close)

		resp := getStream(t, httpSrv, transport.CompleteSsePath())
		defer func() { _ = resp.Body.Close() }()

		readUntil(t, resp.Body, ping)
	})

	t.Run("http heartbeat", func(t *testing.T) {
		withRecordingProviders(t)
		withFlag(t, heartbeatInterval, 25*time.Millisecond)

		srv := &http.Server{} //nolint:gosec // no timeouts, matching the production server
		transport := newStreamableHTTPTransport(srv, newTracedServer(t, false, nil, nil), true)
		t.Cleanup(func() {
			if err := transport.Shutdown(context.Background()); err != nil {
				t.Errorf("shut down streamable transport: %v", err)
			}
		})
		if srv.Handler == nil {
			t.Fatal("http transport left the injected server without a handler")
		}

		httpSrv := httptest.NewServer(srv.Handler)
		t.Cleanup(httpSrv.Close)

		resp := getStream(t, httpSrv, streamableEndpointPath)
		defer func() { _ = resp.Body.Close() }()

		readUntil(t, resp.Body, ping)
	})
}

// getStream opens the long-lived stream at path. Its deadline bounds every read
// on the returned body, so a stream that never produces anything fails the test
// instead of hanging it.
func getStream(t *testing.T, httpSrv *httptest.Server, path string) *http.Response {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}

	return resp
}

// TestRunServesToolCallsOverHTTP drives run() in http mode with a real MCP
// client. The e2e suite only ever launches stdio, so nothing else covers it.
func TestRunServesToolCallsOverHTTP(t *testing.T) {
	col := newLogCollector(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	withFlag(t, mode, "http")
	withFlag(t, httpListenAddr, addr)
	t.Setenv(telemetry.EnvEnabled, "true")
	t.Setenv(telemetry.EnvEndpoint, col.URL)
	t.Setenv(telemetry.EnvTracesEndpoint, "")
	t.Setenv(telemetry.EnvMetricsEndpoint, "")
	t.Setenv(telemetry.EnvLogsEndpoint, "")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("run did not return after its context was cancelled")
		}
	}()

	waitForListener(t, addr)

	callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
	defer callCancel()

	c, err := client.NewStreamableHttpClient("http://" + addr + streamableEndpointPath)
	if err != nil {
		t.Fatalf("build MCP client: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Initialize(callCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "mcp-helm-http-test", Version: "1.0.0"},
		},
	}); err != nil {
		t.Fatalf("initialize over http: %v", err)
	}

	// A repository on a closed local port: the call completes end to end and
	// reports the failure as a tool result, without leaving the machine.
	result, err := c.CallTool(callCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "list_repository_charts",
			Arguments: map[string]any{"repository_url": "http://127.0.0.1:1"},
		},
	})
	if err != nil {
		t.Fatalf("tools/call over http: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected an error result for an unreachable repository, got %+v", result.Content)
	}
}

// waitForListener blocks until addr accepts connections.
func waitForListener(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("nothing accepted connections on %s", addr)
}
