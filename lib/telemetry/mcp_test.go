package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/mark3labs/mcp-go/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	probeTool     = "probe_tool"
	failingTool   = "failing_tool"
	reportingTool = "reporting_tool"
)

const (
	keyErrorType  = string(semconv.ErrorTypeKey)
	keyRequestID  = string(semconv.JSONRPCRequestIDKey)
	keyStatusCode = string(semconv.RPCResponseStatusCodeKey)
	keyTransport  = string(semconv.NetworkTransportKey)
	keyProtocol   = string(semconv.NetworkProtocolNameKey)
)

// newSpanRecorder returns a tracer whose ended spans are kept in memory.
func newSpanRecorder(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	return tp.Tracer("test"), sr
}

// newMetricReader returns a meter whose data points a test can collect.
func newMetricReader(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down meter provider: %v", err)
		}
	})

	return mp, reader
}

// spanAttrs flattens a span's attributes into a map.
func spanAttrs(span sdktrace.ReadOnlySpan) map[string]string {
	attrs := make(map[string]string)
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}

	return attrs
}

// instrumented is an MCP server with the instrumentation installed, and where
// its telemetry lands.
type instrumented struct {
	server *server.MCPServer
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

// newInstrumented builds a server with one tool per outcome the
// instrumentation classifies.
func newInstrumented(t *testing.T, transport Transport) instrumented {
	t.Helper()

	tracer, sr := newSpanRecorder(t)
	mp, reader := newMetricReader(t)

	m, err := NewMCPInstrumentation(tracer, mp.Meter("test"), transport)
	if err != nil {
		t.Fatalf("build instrumentation: %v", err)
	}

	s := server.NewMCPServer("test", "1.0",
		append(m.ServerOptions(), server.WithToolCapabilities(false))...)

	s.AddTool(mcp.NewTool(probeTool), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	s.AddTool(mcp.NewTool(failingTool), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("boom")
	})
	s.AddTool(mcp.NewTool(reportingTool), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("chart not found"), nil
	})

	return instrumented{server: s, spans: sr, reader: reader}
}

// serverSpan returns the single server span recorded.
func (i instrumented) serverSpan(t *testing.T) sdktrace.ReadOnlySpan {
	t.Helper()

	var found []sdktrace.ReadOnlySpan
	for _, span := range i.spans.Ended() {
		if span.SpanKind() == trace.SpanKindServer {
			found = append(found, span)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one server span, got %d", len(found))
	}

	return found[0]
}

// durationPoint returns the single mcp.server.operation.duration data point,
// checking the instrument's unit and buckets on the way.
func (i instrumented) durationPoint(t *testing.T) metricdata.HistogramDataPoint[float64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := i.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != operationDurationInstrument {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("unit = %q, want s", m.Unit)
			}

			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is a %T, want a float64 histogram", operationDurationInstrument, m.Data)
			}
			if len(hist.DataPoints) != 1 {
				t.Fatalf("expected exactly one data point, got %d", len(hist.DataPoints))
			}

			point := hist.DataPoints[0]
			if !slices.Equal(point.Bounds, DurationBucketBoundaries) {
				t.Errorf("bounds = %v, want the conventions' %v", point.Bounds, DurationBucketBoundaries)
			}

			return point
		}
	}

	t.Fatalf("%s was never recorded", operationDurationInstrument)

	return metricdata.HistogramDataPoint[float64]{}
}

func pointAttrs(point metricdata.HistogramDataPoint[float64]) map[string]string {
	attrs := make(map[string]string)
	for _, kv := range point.Attributes.ToSlice() {
		attrs[string(kv.Key)] = kv.Value.String()
	}

	return attrs
}

func callMessage(id, tool string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
}

// withAttrs returns base with extra added.
func withAttrs(base map[string]string, extra map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, extra)

	return out
}

func TestMCPInstrumentation(t *testing.T) {
	httpTransport := map[string]string{keyTransport: "tcp", keyProtocol: "http"}
	toolCall := withAttrs(httpTransport, map[string]string{
		attrMethodName:    methodToolsCall,
		attrOperationName: operationExecuteTool,
	})

	tests := []struct {
		name       string
		message    string
		wantName   string
		wantAttrs  map[string]string
		absent     []string
		wantStatus codes.Code
		wantMetric map[string]string
	}{
		{
			name:     "tool call",
			message:  callMessage("1", probeTool),
			wantName: "tools/call " + probeTool,
			wantAttrs: withAttrs(toolCall, map[string]string{
				keyRequestID: "1",
				attrToolName: probeTool,
			}),
			absent:     []string{keyErrorType, keyStatusCode},
			wantMetric: withAttrs(toolCall, map[string]string{attrToolName: probeTool}),
		},
		{
			// A caller can name any tool, so an unregistered one stays out of
			// the span name and the metric, and asking for one is the
			// caller's error rather than the server's.
			name:     "unknown tool",
			message:  callMessage("2", "no_such_tool"),
			wantName: methodToolsCall,
			wantAttrs: withAttrs(toolCall, map[string]string{
				attrToolName:  "no_such_tool",
				keyStatusCode: "-32602",
			}),
			absent:     []string{keyErrorType},
			wantMetric: withAttrs(toolCall, map[string]string{keyStatusCode: "-32602"}),
		},
		{
			name:     "handler error",
			message:  callMessage("3", failingTool),
			wantName: "tools/call " + failingTool,
			wantAttrs: map[string]string{
				keyErrorType:  "-32603",
				keyStatusCode: "-32603",
			},
			wantStatus: codes.Error,
			wantMetric: withAttrs(toolCall, map[string]string{
				attrToolName:  failingTool,
				keyErrorType:  "-32603",
				keyStatusCode: "-32603",
			}),
		},
		{
			name:       "error result",
			message:    callMessage("4", reportingTool),
			wantName:   "tools/call " + reportingTool,
			wantAttrs:  map[string]string{keyErrorType: errorTypeToolError},
			absent:     []string{keyStatusCode},
			wantStatus: codes.Error,
			wantMetric: withAttrs(toolCall, map[string]string{
				attrToolName: reportingTool,
				keyErrorType: errorTypeToolError,
			}),
		},
		{
			// Rejected before dispatch, so only the error hook runs.
			name:     "unparsable request",
			message:  `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":"x"}`,
			wantName: methodToolsCall,
			wantAttrs: withAttrs(toolCall, map[string]string{
				keyRequestID:  "5",
				keyStatusCode: "-32600",
			}),
			absent:     []string{keyErrorType},
			wantMetric: withAttrs(toolCall, map[string]string{keyStatusCode: "-32600"}),
		},
		{
			name:     "ping with a string id",
			message:  `{"jsonrpc":"2.0","id":"abc","method":"ping"}`,
			wantName: "ping",
			wantAttrs: withAttrs(httpTransport, map[string]string{
				attrMethodName: "ping",
				keyRequestID:   "abc",
			}),
			absent:     []string{attrToolName, attrOperationName, keyErrorType},
			wantMetric: withAttrs(httpTransport, map[string]string{attrMethodName: "ping"}),
		},
		{
			name:       "large numeric id",
			message:    `{"jsonrpc":"2.0","id":12345678901,"method":"ping"}`,
			wantName:   "ping",
			wantAttrs:  map[string]string{keyRequestID: "12345678901"},
			wantMetric: withAttrs(httpTransport, map[string]string{attrMethodName: "ping"}),
		},
		{
			// The method is the caller's, so it reaches neither the span name
			// nor the metric.
			name:     "unknown method",
			message:  `{"jsonrpc":"2.0","id":6,"method":"no/such/method"}`,
			wantName: unknownMethodSpanName,
			wantAttrs: map[string]string{
				attrMethodName: methodOther,
				keyStatusCode:  "-32601",
			},
			absent: []string{keyErrorType},
			wantMetric: withAttrs(httpTransport, map[string]string{
				attrMethodName: methodOther,
				keyStatusCode:  "-32601",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := newInstrumented(t, TransportHTTP)
			i.server.HandleMessage(context.Background(), json.RawMessage(tt.message))

			span := i.serverSpan(t)
			if span.Name() != tt.wantName {
				t.Errorf("span name = %q, want %q", span.Name(), tt.wantName)
			}
			if span.Status().Code != tt.wantStatus {
				t.Errorf("span status = %v, want %v", span.Status().Code, tt.wantStatus)
			}

			attrs := spanAttrs(span)
			for key, want := range tt.wantAttrs {
				if attrs[key] != want {
					t.Errorf("%s = %q, want %q", key, attrs[key], want)
				}
			}
			for _, key := range append(tt.absent, "mcp.method", "mcp.tool.name") {
				if value, ok := attrs[key]; ok {
					t.Errorf("%s = %q, want it unset", key, value)
				}
			}

			if got := pointAttrs(i.durationPoint(t)); !maps.Equal(got, tt.wantMetric) {
				t.Errorf("metric attributes = %v, want %v", got, tt.wantMetric)
			}
		})
	}
}

func TestMCPInstrumentationRecordsStdioTransport(t *testing.T) {
	i := newInstrumented(t, TransportStdio)
	i.server.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))

	attrs := spanAttrs(i.serverSpan(t))
	if attrs[keyTransport] != "pipe" {
		t.Errorf("%s = %q, want pipe", keyTransport, attrs[keyTransport])
	}
	if value, ok := attrs[keyProtocol]; ok {
		t.Errorf("%s = %q, want it unset on stdio", keyProtocol, value)
	}
}

// TestMCPInstrumentationRecordsNegotiatedProtocolVersion drives initialize
// through the streamable transport, which is what gives the request a session
// to negotiate the version on.
func TestMCPInstrumentationRecordsNegotiatedProtocolVersion(t *testing.T) {
	i := newInstrumented(t, TransportHTTP)
	httpSrv := httptest.NewServer(server.NewStreamableHTTPServer(i.server))
	t.Cleanup(httpSrv.Close)

	const version = "2025-03-26"
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"` + version + `","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpSrv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	if got := spanAttrs(i.serverSpan(t))[attrProtocolVersion]; got != version {
		t.Errorf("span %s = %q, want %q", attrProtocolVersion, got, version)
	}
	if got := pointAttrs(i.durationPoint(t))[attrProtocolVersion]; got != version {
		t.Errorf("metric %s = %q, want %q", attrProtocolVersion, got, version)
	}
}

// TestRequestErrorHookIgnoresNotifications pins the nil-id guard: mcp-go
// reports failed outbound notifications through the same hook, from inside
// whatever span sent them, and that span's outcome must not change.
func TestRequestErrorHookIgnoresNotifications(t *testing.T) {
	tracer, sr := newSpanRecorder(t)
	mp, _ := newMetricReader(t)

	m, err := NewMCPInstrumentation(tracer, mp.Meter("test"), TransportHTTP)
	if err != nil {
		t.Fatalf("build instrumentation: %v", err)
	}

	ctx, span := mcpTracer{m: m}.Start(context.Background(), "mcp.ping", tracing.SpanKindServer)
	describeRequest(ctx, float64(1), mcp.MethodPing, &mcp.PingRequest{})
	recordRequestError(ctx, nil, "notification", map[string]any{}, errors.New("send failed"))
	span.End()

	got := sr.Ended()[0]
	if got.Name() != "ping" {
		t.Errorf("span name = %q, want ping", got.Name())
	}

	attrs := spanAttrs(got)
	for _, key := range []string{keyErrorType, keyStatusCode} {
		if value, ok := attrs[key]; ok {
			t.Errorf("%s = %q, want it unset", key, value)
		}
	}
}

// withTraceContextPropagator installs the W3C propagator globally, where
// metaPropagator reads the configured one from.
func withTraceContextPropagator(t *testing.T) {
	t.Helper()

	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })
}

func TestMetaPropagatorRoundTrip(t *testing.T) {
	withTraceContextPropagator(t)

	if meta := (metaPropagator{}).InjectMeta(context.Background(), nil); meta != nil {
		t.Errorf("InjectMeta with nothing to write = %+v, want nil", meta)
	}

	tracer, _ := newSpanRecorder(t)
	ctx, span := tracer.Start(context.Background(), "client")
	defer span.End()

	meta := (metaPropagator{}).InjectMeta(ctx, nil)
	if meta == nil {
		t.Fatal("InjectMeta wrote nothing for an active span")
	}

	got := trace.SpanContextFromContext((metaPropagator{}).ExtractMeta(context.Background(), meta))
	if got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("extracted %v, want %v", got, span.SpanContext())
	}
}

func TestMCPSpanParentsOnMetaTraceContext(t *testing.T) {
	withTraceContextPropagator(t)

	const (
		traceID = "0af7651916cd43dd8448eb211c80319c"
		spanID  = "b7ad6b7169203331"
	)

	i := newInstrumented(t, TransportHTTP)
	i.server.HandleMessage(context.Background(), json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"traceparent":"00-`+traceID+`-`+spanID+`-01"}}}`))

	span := i.serverSpan(t)
	if span.SpanContext().TraceID().String() != traceID {
		t.Errorf("trace = %s, want %s", span.SpanContext().TraceID(), traceID)
	}
	if span.Parent().SpanID().String() != spanID {
		t.Errorf("parent = %s, want the _meta span %s", span.Parent().SpanID(), spanID)
	}
}

// Protocol headers are ignored even when supported; the effective version
// comes from the validated request context instead.
func TestToOTelAttributesDropsUnregisteredAndUnvalidatedKeys(t *testing.T) {
	got := toOTelAttributes([]tracing.Attribute{
		tracing.String("mcp.tool.name", probeTool),
		tracing.String("mcp.method", methodToolsCall),
		tracing.String("mcp.session.id", "abc"),
		tracing.String(attrProtocolVersion, "0-probe"),
		tracing.String(attrProtocolVersion, mcp.ProtocolVersion20250326),
	})

	want := []attribute.KeyValue{attribute.String("mcp.session.id", "abc")}
	if !slices.Equal(got, want) {
		t.Errorf("attributes = %v, want %v", got, want)
	}
}
