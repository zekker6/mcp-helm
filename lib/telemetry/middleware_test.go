package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/zekker6/mcp-helm/lib/logger"
)

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

// attachRecorder tees the logger onto a fresh recording provider.
func attachRecorder() *recordingLogger {
	provider := &recordingLogProvider{logger: &recordingLogger{}}
	logger.AttachProvider(provider)

	return provider.logger
}

// recordFor returns the single record the middleware emitted for tool.
func recordFor(t *testing.T, recorder *recordingLogger, tool string) emitted {
	t.Helper()

	var found []emitted
	for _, e := range recorder.all() {
		if recordAttrs(e.record)[attrToolName] == tool {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one record for %s, got %d", tool, len(found))
	}

	return found[0]
}

func toolRequest(name string) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = name

	return req
}

func TestToolMiddlewareLogsOutcome(t *testing.T) {
	recorder := attachRecorder()

	tests := []struct {
		name          string
		tool          string
		handler       func() (*mcp.CallToolResult, error)
		wantErrorType string
	}{
		{
			name:    "success",
			tool:    "succeeding_tool",
			handler: func() (*mcp.CallToolResult, error) { return mcp.NewToolResultText("ok"), nil },
		},
		{
			name:          "handler error",
			tool:          "failing_tool",
			handler:       func() (*mcp.CallToolResult, error) { return nil, errors.New("boom") },
			wantErrorType: "*errors.errorString",
		},
		{
			// mcp-go delivers this as a successful response carrying an error
			// result, which a handler error is not.
			name:          "error result",
			tool:          "reporting_tool",
			handler:       func() (*mcp.CallToolResult, error) { return mcp.NewToolResultError("chart not found"), nil },
			wantErrorType: errorTypeToolError,
		},
		{
			// Under server.WithRecovery a panic never reaches the middleware
			// as a return value, so a call logged only on the normal path
			// would go missing entirely.
			name:          "panic",
			tool:          "panicking_tool",
			handler:       func() (*mcp.CallToolResult, error) { panic("handler exploded") },
			wantErrorType: errorTypeOther,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			func() {
				defer func() { _ = recover() }()

				_, _ = ToolMiddleware(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return tt.handler()
				})(context.Background(), toolRequest(tt.tool))
			}()

			record := recordFor(t, recorder, tt.tool).record
			if _, ok := recordAttrs(record)["mcp_helm.mcp.tool.duration"]; !ok {
				t.Error("tool duration missing its application-specific namespace")
			}
			if record.Severity() != log.SeverityInfo {
				t.Errorf("severity = %v, want info", record.Severity())
			}

			// The conventions make absence the success signal.
			got, ok := recordAttrs(record)[string(semconv.ErrorTypeKey)]
			if tt.wantErrorType == "" && ok {
				t.Errorf("error.type = %q, want none on a successful call", got)
			}
			if tt.wantErrorType != "" && got != tt.wantErrorType {
				t.Errorf("error.type = %q, want %q", got, tt.wantErrorType)
			}
		})
	}
}

// TestToolMiddlewareLogsCallWithTraceIDs pins the per-call record: it is what
// links an exported log line back to the tool span that produced it.
func TestToolMiddlewareLogsCallWithTraceIDs(t *testing.T) {
	recorder := attachRecorder()

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
	if _, err := ToolMiddleware(handler)(ctx, toolRequest(probeTool)); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// The ids reach the exporter through the record's own trace fields, which
	// the SDK derives from the emit context, so the context is what the bridge
	// has to be handed - not a pair of attributes duplicating them.
	got := trace.SpanContextFromContext(recordFor(t, recorder, probeTool).ctx)
	want := trace.SpanContextFromContext(ctx)
	if !got.Equal(want) {
		t.Errorf("emit context span = %v, want %v", got, want)
	}
}
