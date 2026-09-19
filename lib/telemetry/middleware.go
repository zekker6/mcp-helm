package telemetry

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.uber.org/zap"

	"github.com/zekker6/mcp-helm/lib/logger"
)

// No registry attribute covers the elapsed tool time on the log record.
const attrDuration = Namespace + "mcp.tool.duration"

// errorTypeOther classifies a call that ended without the handler returning at
// all, which is the only failure this middleware cannot name.
var errorTypeOther = semconv.ErrorTypeOther.Value.AsString()

// ToolMiddleware is an mcp-go tool middleware logging one INFO record per call.
//
// Register it after MCPInstrumentation.ServerOptions: mcp-go applies tool
// middlewares in reverse registration order (server.go:2010-2013), and only
// innermost does ctx carry the tool.<name> span the record has to reference.
func ToolMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := request.Params.Name
		start := time.Now()

		// A panicking handler unwinds through the deferred record before
		// server.WithRecovery turns the panic into a result, so this starts
		// unclassified and is narrowed once next returns.
		errorType := errorTypeOther

		defer func() {
			fields := []zap.Field{
				zap.String(attrToolName, name),
				zap.Duration(attrDuration, time.Since(start)),
			}

			// The conventions make absence the success signal.
			if errorType != "" {
				fields = append(fields, zap.String(string(semconv.ErrorTypeKey), errorType))
			}

			logger.Ctx(ctx).Info("mcp tool call", fields...)
		}()

		result, err := next(ctx, request)
		errorType = toolErrorType(result, err)

		return result, err
	}
}

// toolErrorType classifies a finished call. An empty string means success and
// no error.type. The record is about the handler rather than the JSON-RPC
// response, so a returned error is named by its Go type where the span and
// metric carry the error code mcp-go answered with.
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
