package telemetry

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/zekker6/mcp-helm/lib/logger"
)

// toolDurationInstrument is the histogram the MCP semantic conventions define
// for the receiving side. Its _count series doubles as the call rate, so there
// is no separate counter.
const toolDurationInstrument = "mcp.server.operation.duration"

const (
	attrToolName      = "gen_ai.tool.name"
	attrOperationName = "gen_ai.operation.name"
	attrMethodName    = "mcp.method.name"
	// Elapsed time on the log record. No registry attribute covers it, and
	// unprefixed names are reserved for the specification.
	attrDuration = "cloud.zekker.mcp.tool.duration"
)

// Every call this middleware sees is a tools/call executing one tool, so both
// are constants and the instrument's attribute set stays small.
const (
	methodToolsCall      = "tools/call"
	operationExecuteTool = "execute_tool"
)

// errorTypeToolError is the error.type for a handler that reported the failure
// to its caller rather than returning an error. mcp-go delivers that as a
// successful response carrying an error result, and the two need different
// alerts.
const errorTypeToolError = "tool_error"

// errorTypeOther classifies a call that ended without the handler returning at
// all, which is the only failure this middleware cannot name.
var errorTypeOther = semconv.ErrorTypeOther.Value.AsString()

// callAttributes are the attributes every call carries, whatever its outcome.
func callAttributes(tool string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(attrMethodName, methodToolsCall),
		attribute.String(attrToolName, tool),
		attribute.String(attrOperationName, operationExecuteTool),
	}
}

// ToolMiddleware returns an mcp-go tool middleware recording
// toolDurationInstrument and one INFO record per call.
//
// Register it after server.WithTracer: mcp-go applies tool middlewares in
// reverse registration order (server.go:2010-2013), and only innermost does ctx
// carry the tool.<name> span the log record has to reference.
func (t *Telemetry) ToolMiddleware() (server.ToolHandlerMiddleware, error) {
	duration, err := t.Meter().Float64Histogram(
		toolDurationInstrument,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of MCP tool handler invocations."),
	)
	if err != nil {
		return nil, fmt.Errorf("build %s instrument: %w", toolDurationInstrument, err)
	}

	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := request.Params.Name
			start := time.Now()

			// mcp-go labels its span with the deprecated mcp.tool.name, so the
			// current names are added here. Running innermost, the span in ctx
			// is the tool.<name> one mcp-go just started.
			span := trace.SpanFromContext(ctx)
			span.SetAttributes(callAttributes(name)...)

			// A panicking handler unwinds through the deferred record before
			// server.WithRecovery turns the panic into a result, so this starts
			// unclassified and is narrowed once next returns.
			errorType := errorTypeOther

			defer func() {
				elapsed := time.Since(start)
				attrs := callAttributes(name)
				fields := []zap.Field{
					zap.String(attrToolName, name),
					zap.Duration(attrDuration, elapsed),
				}

				// The conventions make absence the success signal.
				if errorType != "" {
					attrs = append(attrs, semconv.ErrorTypeKey.String(errorType))
					fields = append(fields, zap.String(string(semconv.ErrorTypeKey), errorType))
					span.SetAttributes(semconv.ErrorTypeKey.String(errorType))
				}

				duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attrs...))
				logger.Ctx(ctx).Info("mcp tool call", fields...)
			}()

			result, err := next(ctx, request)
			errorType = toolErrorType(result, err)

			return result, err
		}
	}, nil
}

// toolErrorType classifies a finished call. An empty string means success and
// no error.type. It separates a handler that failed from one that reported a
// failure to its caller, which the conventions name tool_error.
func toolErrorType(result *mcp.CallToolResult, err error) string {
	switch {
	case err != nil:
		return semconv.ErrorType(err).Value.AsString()
	case result != nil && result.IsError:
		return errorTypeToolError
	default:
		return ""
	}
}
