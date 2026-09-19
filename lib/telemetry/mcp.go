package telemetry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/mark3labs/mcp-go/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// The MCP conventions, the gen_ai.* attributes and jsonrpc.request.id are at
// Development stability and rpc.response.status_code is a Release Candidate,
// so any of these names may still change. semconv v1.43.0 has no helpers for
// the mcp.* and gen_ai.* ones.
// https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/mcp.md
const (
	operationDurationInstrument = "mcp.server.operation.duration"

	attrMethodName      = "mcp.method.name"
	attrProtocolVersion = "mcp.protocol.version"
	attrToolName        = "gen_ai.tool.name"
	attrOperationName   = "gen_ai.operation.name"
)

const (
	methodToolsCall      = "tools/call"
	operationExecuteTool = "execute_tool"

	// methodOther is the mcp.method.name of a request mcp-go has no handler
	// for. Its method is whatever the caller sent, so recording it would open
	// one span name and one series per value.
	methodOther = "_OTHER"
	// unknownMethodSpanName names such a request's span, the way HTTP
	// instrumentation names a request with an unknown method "HTTP".
	unknownMethodSpanName = "MCP"
)

// errorTypeToolError is the error.type for a tool call whose handler reported
// the failure to its caller rather than returning an error. mcp-go delivers
// that as a successful response carrying an error result.
const errorTypeToolError = "tool_error"

// DurationBucketBoundaries are the histogram buckets, in seconds, the MCP
// conventions give mcp.server.operation.duration. The SDK default is meant for
// milliseconds and would put nearly every call into its first bucket.
var DurationBucketBoundaries = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// callerErrorCodes are the JSON-RPC codes the MCP conventions do not count as
// errors: the caller sent something the server could not serve. They still
// reach rpc.response.status_code, but not error.type or the span status.
var callerErrorCodes = map[int]bool{
	mcp.PARSE_ERROR:        true,
	mcp.INVALID_REQUEST:    true,
	mcp.METHOD_NOT_FOUND:   true,
	mcp.INVALID_PARAMS:     true,
	mcp.RESOURCE_NOT_FOUND: true,
}

// unregisteredAttributes are keys mcp-go sets inside the registry's mcp.*
// namespace although the registry does not define them. mcp.method.name and
// gen_ai.tool.name carry the same information.
var unregisteredAttributes = map[string]bool{
	"mcp.method":    true,
	"mcp.tool.name": true,
}

// Transport is how MCP messages reach the server.
type Transport int

const (
	TransportStdio Transport = iota
	TransportHTTP
)

// attributes are what the MCP conventions tell transports apart by, together
// with mcp.protocol.version.
func (t Transport) attributes() []attribute.KeyValue {
	if t == TransportStdio {
		return []attribute.KeyValue{semconv.NetworkTransportPipe}
	}

	return []attribute.KeyValue{semconv.NetworkTransportTCP, semconv.NetworkProtocolName("http")}
}

// MCPInstrumentation traces and measures the requests an mcp-go server
// dispatches, following the MCP semantic conventions.
type MCPInstrumentation struct {
	tracer    trace.Tracer
	duration  metric.Float64Histogram
	transport []attribute.KeyValue
}

// NewMCPInstrumentation builds the instrumentation on the given tracer and
// meter.
func NewMCPInstrumentation(tracer trace.Tracer, meter metric.Meter, transport Transport) (*MCPInstrumentation, error) {
	duration, err := meter.Float64Histogram(
		operationDurationInstrument,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of MCP requests, from receipt until the response is ready."),
		metric.WithExplicitBucketBoundaries(DurationBucketBoundaries...),
	)
	if err != nil {
		return nil, fmt.Errorf("build %s instrument: %w", operationDurationInstrument, err)
	}

	return &MCPInstrumentation{tracer: tracer, duration: duration, transport: transport.attributes()}, nil
}

// MCPInstrumentation builds the instrumentation on this service's providers.
func (t *Telemetry) MCPInstrumentation(transport Transport) (*MCPInstrumentation, error) {
	return NewMCPInstrumentation(t.Tracer(), t.Meter(), transport)
}

// ServerOptions installs the instrumentation on an mcp-go server. Tool
// middlewares registered after them run inside the tool.<name> span: mcp-go
// applies tool middlewares in reverse registration order, and its tracer option
// registers the one that opens that span.
//
// No header propagator is installed. mcp-go extracts from the inbound headers
// before opening its span, which over HTTP would make the MCP span a sibling of
// the otelhttp span instead of its child. Trace context a client puts in
// params._meta is honoured, as the conventions ask: it belongs to the MCP
// client span, which is the right parent.
func (m *MCPInstrumentation) ServerOptions() []server.ServerOption {
	hooks := &server.Hooks{}
	hooks.AddBeforeAny(describeRequest)
	hooks.AddOnSuccess(recordSuccess)
	hooks.AddOnError(recordRequestError)

	return []server.ServerOption{
		server.WithTracer(mcpTracer{m: m}),
		server.WithHooks(hooks),
		server.WithMetaPropagator(metaPropagator{}),
	}
}

// mcpTracer adapts OpenTelemetry to mcp-go's tracing interface, in place of
// github.com/mark3labs/mcp-go/otel: the conventions need the spans renamed,
// relabelled and measured, and that adapter's spans cannot be.
type mcpTracer struct {
	m *MCPInstrumentation
}

func (t mcpTracer) Start(
	ctx context.Context,
	name string,
	kind tracing.SpanKind,
	attrs ...tracing.Attribute,
) (context.Context, tracing.Span) {
	// The only server span mcp-go opens is the one per dispatched request, as
	// mcp.<method> (see server.WithTracer). That method is the caller's until
	// mcp-go matches it to a handler, so the hooks name the span once it has.
	if kind == tracing.SpanKindServer {
		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(append(toOTelAttributes(attrs), t.m.transport...)...),
		}
		if ambient, ok := ctx.Value(ambientSpanKey{}).(trace.SpanContext); ok && ambient.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: ambient}))
		}
		ctx, span := t.m.tracer.Start(ctx, unknownMethodSpanName, opts...)
		s := &messageSpan{span: span, start: time.Now(), m: t.m}
		s.recordProtocolVersion(ctx)

		return tracing.ContextWithSpan(ctx, s), s
	}

	ctx, span := t.m.tracer.Start(ctx, name,
		trace.WithSpanKind(toOTelKind(kind)),
		trace.WithAttributes(toOTelAttributes(attrs)...),
	)
	s := otelSpan{span: span}

	return tracing.ContextWithSpan(ctx, s), s
}

// otelSpan is the plain adapter, for the spans mcp-go opens besides the
// per-request one.
type otelSpan struct {
	span trace.Span
}

func (s otelSpan) SetAttributes(attrs ...tracing.Attribute) {
	s.span.SetAttributes(toOTelAttributes(attrs)...)
}

func (s otelSpan) RecordError(err error) { s.span.RecordError(err) }

func (s otelSpan) SetStatus(code tracing.StatusCode, description string) {
	s.span.SetStatus(toOTelStatus(code), description)
}

func (s otelSpan) End() { s.span.End() }

// messageSpan is the server span of one request. The hooks fill in what mcp-go
// does not pass to the tracer, and End settles the status and records the
// duration from it.
type messageSpan struct {
	span  trace.Span
	start time.Time
	m     *MCPInstrumentation

	// method stays empty when mcp-go never matched the request to a handler.
	method string
	// tool is set for registered tools only.
	tool            string
	protocolVersion string
	statusCode      string
	errorType       string
	// errorMessage is what mcp-go reported a failure with. It becomes the
	// status description if the failure turns out to be the server's.
	errorMessage string
}

func (s *messageSpan) SetAttributes(attrs ...tracing.Attribute) {
	s.span.SetAttributes(toOTelAttributes(attrs)...)
}

func (s *messageSpan) RecordError(err error) { s.span.RecordError(err) }

// SetStatus holds an error back until End: mcp-go marks every error response,
// including the ones the conventions attribute to the caller.
func (s *messageSpan) SetStatus(code tracing.StatusCode, description string) {
	if code == tracing.StatusError {
		s.errorMessage = description
		return
	}

	s.span.SetStatus(toOTelStatus(code), description)
}

func (s *messageSpan) End() {
	if s.method == "" {
		s.method = methodOther
		s.span.SetAttributes(attribute.String(attrMethodName, methodOther))

		// Each of mcp-go's handlers runs a hook on the way out, so an error
		// sent without one is the reply to a method mcp-go does not implement.
		if s.errorMessage != "" {
			s.statusCode = strconv.Itoa(mcp.METHOD_NOT_FOUND)
			s.span.SetAttributes(semconv.RPCResponseStatusCode(s.statusCode))
		}
	}

	if s.errorType != "" {
		s.span.SetStatus(codes.Error, cmp.Or(s.errorMessage, s.errorType))
	}

	// The span in ctx lets the SDK attach it to the data point as an exemplar.
	ctx := trace.ContextWithSpan(context.Background(), s.span)
	s.m.duration.Record(ctx, time.Since(s.start).Seconds(), metric.WithAttributes(s.metricAttributes()...))

	s.span.End()
}

// metricAttributes include only registered tools and supported protocol versions.
func (s *messageSpan) metricAttributes() []attribute.KeyValue {
	attrs := append([]attribute.KeyValue{attribute.String(attrMethodName, s.method)}, s.m.transport...)
	if s.method == methodToolsCall {
		attrs = append(attrs, attribute.String(attrOperationName, operationExecuteTool))
	}
	if s.tool != "" {
		attrs = append(attrs, attribute.String(attrToolName, s.tool))
	}
	if s.protocolVersion != "" {
		attrs = append(attrs, attribute.String(attrProtocolVersion, s.protocolVersion))
	}
	if s.statusCode != "" {
		attrs = append(attrs, semconv.RPCResponseStatusCode(s.statusCode))
	}
	if s.errorType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(s.errorType))
	}

	return attrs
}

// describe names the span "{mcp.method.name} {target}" and sets what is known
// before the method runs.
func (s *messageSpan) describe(ctx context.Context, id any, method mcp.MCPMethod, message any) {
	s.method = string(method)
	name := s.method
	attrs := []attribute.KeyValue{
		attribute.String(attrMethodName, s.method),
		semconv.JSONRPCRequestID(requestID(id)),
	}

	if method == mcp.MethodToolsCall {
		attrs = append(attrs, attribute.String(attrOperationName, operationExecuteTool))

		if req, ok := message.(*mcp.CallToolRequest); ok && req.Params.Name != "" {
			attrs = append(attrs, attribute.String(attrToolName, req.Params.Name))

			// The tool name is the caller's, so only a registered one is
			// bounded enough for the span name and the metric.
			if srv := server.ServerFromContext(ctx); srv != nil && srv.GetTool(req.Params.Name) != nil {
				s.tool = req.Params.Name
				name += " " + s.tool
			}
		}
	}

	s.span.SetName(name)
	s.span.SetAttributes(attrs...)
	s.recordProtocolVersion(ctx)
}

// Legacy request metadata can override the negotiated version without being
// validated by mcp-go. Never turn those arbitrary values into metric labels.
func (s *messageSpan) recordProtocolVersion(ctx context.Context) {
	if v := server.RequestProtocolVersion(ctx); mcp.IsValidProtocolVersion(v) {
		s.protocolVersion = v
		s.span.SetAttributes(attribute.String(attrProtocolVersion, v))
	}
}

// messageSpanFrom returns the request span mcpTracer opened for ctx, or nil.
func messageSpanFrom(ctx context.Context) *messageSpan {
	s, _ := tracing.SpanFromContext(ctx).(*messageSpan)
	return s
}

func describeRequest(ctx context.Context, id any, method mcp.MCPMethod, message any) {
	if s := messageSpanFrom(ctx); s != nil {
		s.describe(ctx, id, method, message)
	}
}

// recordSuccess picks up the protocol version initialize has just negotiated,
// and marks a tool call whose handler reported a failure to its caller: mcp-go
// sends that as a successful response, so the error hook never sees it.
func recordSuccess(ctx context.Context, _ any, _ mcp.MCPMethod, _ any, result any) {
	s := messageSpanFrom(ctx)
	if s == nil {
		return
	}

	s.recordProtocolVersion(ctx)

	if r, ok := result.(*mcp.CallToolResult); ok && r.IsError {
		s.errorType = errorTypeToolError
		s.span.SetAttributes(semconv.ErrorTypeKey.String(s.errorType))
	}
}

// recordRequestError records the JSON-RPC error a request was answered with.
// It describes the request as well: one mcp-go rejects before dispatch never
// reaches the before hooks.
func recordRequestError(ctx context.Context, id any, method mcp.MCPMethod, message any, err error) {
	// mcp-go also reports failed outbound notifications through this hook,
	// with no id and from inside whatever span sent them.
	if id == nil {
		return
	}

	s := messageSpanFrom(ctx)
	if s == nil {
		return
	}

	s.describe(ctx, id, method, message)

	// mcp-go's request errors are unexported, but each knows the JSON-RPC
	// error it is sent as.
	var rpcErr interface{ ToJSONRPCError() mcp.JSONRPCError }
	if !errors.As(err, &rpcErr) {
		s.errorType = semconv.ErrorType(err).Value.AsString()
		s.span.SetAttributes(semconv.ErrorTypeKey.String(s.errorType))
		return
	}

	code := rpcErr.ToJSONRPCError().Error.Code
	s.statusCode = strconv.Itoa(code)
	attrs := []attribute.KeyValue{semconv.RPCResponseStatusCode(s.statusCode)}
	if !callerErrorCodes[code] {
		s.errorType = s.statusCode
		attrs = append(attrs, semconv.ErrorTypeKey.String(s.errorType))
	}

	s.span.SetAttributes(attrs...)
}

// metaPropagator carries trace context in the params._meta bag with the
// configured OpenTelemetry propagators, as SEP-414 and the MCP conventions
// specify. The keys are the propagators' own, unprefixed: traceparent,
// tracestate and baggage.
type metaPropagator struct{}

type ambientSpanKey struct{}

func (metaPropagator) ExtractMeta(ctx context.Context, meta *mcp.Meta) context.Context {
	if meta == nil {
		return ctx
	}

	carrier := propagation.MapCarrier{}
	for key, value := range meta.AdditionalFields {
		if s, ok := value.(string); ok {
			carrier[key] = s
		}
	}

	ambient := trace.SpanContextFromContext(ctx)
	extracted := otel.GetTextMapPropagator().Extract(ctx, carrier)
	parent := trace.SpanContextFromContext(extracted)
	if ambient.IsValid() && parent.IsValid() &&
		(ambient.TraceID() != parent.TraceID() || ambient.SpanID() != parent.SpanID()) {
		extracted = context.WithValue(extracted, ambientSpanKey{}, ambient)
	}

	return extracted
}

// InjectMeta follows the tracing.MetaPropagator contract: a nil meta is
// allocated only when there is something to write.
func (metaPropagator) InjectMeta(ctx context.Context, meta *mcp.Meta) *mcp.Meta {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return meta
	}

	if meta == nil {
		meta = &mcp.Meta{}
	}
	if meta.AdditionalFields == nil {
		meta.AdditionalFields = make(map[string]any, len(carrier))
	}
	for key, value := range carrier {
		meta.AdditionalFields[key] = value
	}

	return meta
}

// requestID renders a JSON-RPC id as jsonrpc.request.id expects. encoding/json
// decodes numeric ids as float64, which %v would print in exponent form once
// they grow past a few digits.
func requestID(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func toOTelAttributes(attrs []tracing.Attribute) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		// Protocol versions come from recordProtocolVersion, not raw headers.
		if a.Key != attrProtocolVersion && !unregisteredAttributes[a.Key] {
			out = append(out, attribute.String(a.Key, a.Value))
		}
	}

	return out
}

func toOTelKind(kind tracing.SpanKind) trace.SpanKind {
	switch kind {
	case tracing.SpanKindServer:
		return trace.SpanKindServer
	case tracing.SpanKindClient:
		return trace.SpanKindClient
	case tracing.SpanKindInternal:
		return trace.SpanKindInternal
	default:
		return trace.SpanKindUnspecified
	}
}

func toOTelStatus(code tracing.StatusCode) codes.Code {
	switch code {
	case tracing.StatusOK:
		return codes.Ok
	case tracing.StatusError:
		return codes.Error
	default:
		return codes.Unset
	}
}
