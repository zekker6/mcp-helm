package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/zekker6/mcp-helm/lib/logger"
)

const probeTool = "probe_tool"

// newMeteredTelemetry returns a Telemetry whose meter feeds a manual reader, so
// a test can collect what the middleware recorded without an exporter.
func newMeteredTelemetry(t *testing.T) (*Telemetry, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down meter provider: %v", err)
		}
	})

	tel := newNoop()
	tel.meterProvider = provider

	return tel, reader
}

// toolRequest builds a tools/call request for name.
func toolRequest(name string, arguments any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = arguments

	return req
}

// callTool runs handler through the middleware exactly as mcp-go would.
func callTool(
	t *testing.T,
	tel *Telemetry,
	req mcp.CallToolRequest,
	handler server.ToolHandlerFunc,
) (*mcp.CallToolResult, error) {
	t.Helper()

	mw, err := tel.ToolMiddleware()
	if err != nil {
		t.Fatalf("build tool middleware: %v", err)
	}

	return mw(handler)(context.Background(), req)
}

// toolDurationPoints returns the data points recorded for the tool duration
// instrument, failing the test when it was never recorded.
func toolDurationPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != toolDurationInstrument {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("%s unit = %q, want %q", toolDurationInstrument, m.Unit, "s")
			}

			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is a %T, want a float64 histogram", toolDurationInstrument, m.Data)
			}

			return hist.DataPoints
		}
	}

	t.Fatalf("instrument %s was never recorded", toolDurationInstrument)

	return nil
}

// onlyPoint returns the single recorded data point.
func onlyPoint(t *testing.T, reader *sdkmetric.ManualReader) metricdata.HistogramDataPoint[float64] {
	t.Helper()

	points := toolDurationPoints(t, reader)
	if len(points) != 1 {
		t.Fatalf("expected exactly one data point, got %d", len(points))
	}

	return points[0]
}

// assertPoint checks the one recorded call against the expected attributes. An
// empty errorType means the call succeeded and carries no error.type at all.
func assertPoint(t *testing.T, point metricdata.HistogramDataPoint[float64], tool, errorType string) {
	t.Helper()

	if point.Count != 1 {
		t.Errorf("count = %d, want 1", point.Count)
	}

	attrs := callAttributes(tool)
	if errorType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType))
	}

	want := attribute.NewSet(attrs...)
	if !point.Attributes.Equals(&want) {
		t.Errorf("attributes = %v, want %v", point.Attributes.Encoded(attribute.DefaultEncoder()),
			want.Encoded(attribute.DefaultEncoder()))
	}
}

func TestToolMiddlewareRecordsSuccessfulCall(t *testing.T) {
	tel, reader := newMeteredTelemetry(t)

	// The handler blocks for a known interval so the recorded value is checked
	// against a lower bound: a middleware recording a constant would pass any
	// assertion a zero-duration handler can make.
	const handlerDelay = 20 * time.Millisecond

	result, err := callTool(t, tel, toolRequest(probeTool, nil),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			time.Sleep(handlerDelay)

			return mcp.NewToolResultText("ok"), nil
		})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected a successful result, got %+v", result)
	}

	point := onlyPoint(t, reader)
	assertPoint(t, point, probeTool, "")

	if want := handlerDelay.Seconds(); point.Sum < want {
		t.Errorf("sum = %v, want at least %v", point.Sum, want)
	}
}

func TestToolMiddlewareRecordsHandlerError(t *testing.T) {
	tel, reader := newMeteredTelemetry(t)

	wantErr := errors.New("boom")
	_, err := callTool(t, tel, toolRequest(probeTool, nil),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	assertPoint(t, onlyPoint(t, reader), probeTool, "*errors.errorString")
}

// TestToolMiddlewareRecordsErrorResult covers the outcome mcp-go delivers as a
// successful response carrying an error result, which a handler error is not.
func TestToolMiddlewareRecordsErrorResult(t *testing.T) {
	tel, reader := newMeteredTelemetry(t)

	result, err := callTool(t, tel, toolRequest(probeTool, nil),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("chart not found"), nil
		})
	if err != nil {
		t.Fatalf("expected no handler error, got %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected an error result, got %+v", result)
	}

	assertPoint(t, onlyPoint(t, reader), probeTool, errorTypeToolError)
}

// TestToolMiddlewareRecordsPanickingHandler pins the deferred record: under
// server.WithRecovery a panic never reaches the middleware as a return value,
// so a call measured only on the normal path would go missing entirely.
func TestToolMiddlewareRecordsPanickingHandler(t *testing.T) {
	tel, reader := newMeteredTelemetry(t)

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("expected the panic to propagate through the middleware")
			}
		}()

		_, _ = callTool(t, tel, toolRequest(probeTool, nil),
			func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				panic("handler exploded")
			})
	}()

	assertPoint(t, onlyPoint(t, reader), probeTool, errorTypeOther)
}

// TestToolMiddlewareAttributesStayBounded keeps request arguments out of the
// metric: they are caller-supplied chart names and repository URLs, and a
// single unbounded attribute multiplies the series count without limit.
func TestToolMiddlewareAttributesStayBounded(t *testing.T) {
	tel, reader := newMeteredTelemetry(t)

	const unbounded = "https://user:secret@charts.example.com/very/unique/path"
	_, err := callTool(t, tel, toolRequest(probeTool, map[string]any{"repository": unbounded}),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	point := onlyPoint(t, reader)
	for _, kv := range point.Attributes.ToSlice() {
		switch string(kv.Key) {
		case attrToolName, attrMethodName, attrOperationName:
		default:
			t.Errorf("unexpected attribute %s=%s", kv.Key, kv.Value.AsString())
		}
		if strings.Contains(kv.Value.AsString(), unbounded) {
			t.Errorf("attribute %s carries a request argument", kv.Key)
		}
	}
}

// recordingLogProvider keeps every record the zap bridge emits.
type recordingLogProvider struct {
	embedded.LoggerProvider

	logger *recordingLogger
}

func (p *recordingLogProvider) Logger(string, ...log.LoggerOption) log.Logger { return p.logger }

type recordingLogger struct {
	embedded.Logger

	mu      sync.Mutex
	records []emitted
}

// emitted keeps the context alongside the record: the SDK derives the record's
// trace ids from it, so it is the only place the correlation can be observed
// through a fake provider.
type emitted struct {
	ctx    context.Context
	record log.Record
}

func (l *recordingLogger) Emit(ctx context.Context, record log.Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, emitted{ctx: ctx, record: record})
}

func (*recordingLogger) Enabled(context.Context, log.EnabledParameters) bool { return true }

func (l *recordingLogger) all() []emitted {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]emitted(nil), l.records...)
}

// recordAttrs flattens a record's attributes into a map.
func recordAttrs(record log.Record) map[string]string {
	attrs := make(map[string]string)
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value.AsString()
		return true
	})

	return attrs
}

// TestToolMiddlewareLogsCallWithTraceIDs pins the per-call record: it is what
// links an exported log line back to the tool span that produced it.
func TestToolMiddlewareLogsCallWithTraceIDs(t *testing.T) {
	provider := &recordingLogProvider{logger: &recordingLogger{}}
	logger.AttachProvider(provider)

	tel, _ := newMeteredTelemetry(t)
	mw, err := tel.ToolMiddleware()
	if err != nil {
		t.Fatalf("build tool middleware: %v", err)
	}

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})

	ctx, span := tp.Tracer("test").Start(context.Background(), "tool."+probeTool)
	defer span.End()

	handler := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	if _, err := mw(handler)(ctx, toolRequest(probeTool, nil)); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var found []emitted
	for _, e := range provider.logger.all() {
		if recordAttrs(e.record)[attrToolName] == probeTool {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one record for %s, got %d", probeTool, len(found))
	}

	record := found[0].record
	if record.Severity() != log.SeverityInfo {
		t.Errorf("severity = %v, want info", record.Severity())
	}

	attrs := recordAttrs(record)
	if _, ok := attrs[string(semconv.ErrorTypeKey)]; ok {
		t.Errorf("record carries error.type %q, want none on a successful call", attrs[string(semconv.ErrorTypeKey)])
	}

	// The ids reach the exporter through the record's own trace fields, which
	// the SDK derives from the emit context, so the context is what the bridge
	// has to be handed - not a pair of attributes duplicating them.
	got := trace.SpanContextFromContext(found[0].ctx)
	want := trace.SpanContextFromContext(ctx)
	if !got.Equal(want) {
		t.Errorf("emit context span = %v, want %v", got, want)
	}
}
