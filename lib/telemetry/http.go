package telemetry

import (
	"encoding/json"
	"io"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// attrMessageKind names the JSON-RPC message shape a POST carried. No registry
// attribute covers it, and without it a client's reply to a server ping looks
// the same as a request at the HTTP level.
const attrMessageKind = Namespace + "jsonrpc.message.kind"

const maxJSONRPCBodySize = 64 << 10

const (
	messageKindRequest      = "request"
	messageKindNotification = "notification"
	messageKindResponse     = "response"
)

// wellKnownMethods are the registry's mcp.method.name values. Any other method
// a client sends is recorded as _OTHER, since mcp-go does not validate
// notification methods and the value is otherwise the caller's.
// https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/registry/attributes/mcp.md
var wellKnownMethods = map[string]bool{
	"completion/complete":                  true,
	"elicitation/create":                   true,
	"initialize":                           true,
	"logging/setLevel":                     true,
	"notifications/cancelled":              true,
	"notifications/initialized":            true,
	"notifications/message":                true,
	"notifications/progress":               true,
	"notifications/prompts/list_changed":   true,
	"notifications/resources/list_changed": true,
	"notifications/resources/updated":      true,
	"notifications/roots/list_changed":     true,
	"notifications/tools/list_changed":     true,
	"ping":                                 true,
	"prompts/get":                          true,
	"prompts/list":                         true,
	"resources/list":                       true,
	"resources/read":                       true,
	"resources/subscribe":                  true,
	"resources/templates/list":             true,
	"resources/unsubscribe":                true,
	"roots/list":                           true,
	"sampling/createMessage":               true,
	"tools/call":                           true,
	"tools/list":                           true,
}

// AnnotateJSONRPC labels the HTTP server span in the request context with the
// JSON-RPC message the body carried. mcp-go opens its own span only for the
// requests it dispatches, so for notifications and for a client's replies to
// server pings the HTTP span is the only record there is. A request gets only
// its kind: the method and id are on the MCP span, and recording them here too
// would count every request twice.
//
// The body is captured as the transport reads it rather than read ahead, so the
// transport sees the same stream, errors included, that it would unwrapped.
// Bodies over 64 KiB and nonrecording spans are not annotated.
func AnnotateJSONRPC(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := trace.SpanFromContext(r.Context())
		if r.Method != http.MethodPost || !span.IsRecording() || r.ContentLength > maxJSONRPCBodySize {
			next.ServeHTTP(w, r)
			return
		}

		var body jsonRPCBodyCapture
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(r.Body, &body), r.Body}

		next.ServeHTTP(w, r)

		if !body.overflow {
			span.SetAttributes(messageAttributes(body.data[:body.n])...)
		}
	})
}

type jsonRPCBodyCapture struct {
	data     [maxJSONRPCBodySize]byte
	n        int
	overflow bool
}

func (b *jsonRPCBodyCapture) Write(p []byte) (int, error) {
	n := copy(b.data[b.n:], p)
	b.n += n
	b.overflow = b.overflow || n < len(p)
	return len(p), nil
}

// messageAttributes describes a single JSON-RPC message. Anything else, a
// batch included, yields no attributes.
func messageAttributes(body []byte) []attribute.KeyValue {
	var msg struct {
		Method string          `json:"method"`
		ID     any             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil
	}

	switch {
	case msg.Method != "" && msg.ID != nil:
		return []attribute.KeyValue{attribute.String(attrMessageKind, messageKindRequest)}
	case msg.Method != "":
		method := msg.Method
		if !wellKnownMethods[method] {
			method = methodOther
		}

		return []attribute.KeyValue{
			attribute.String(attrMessageKind, messageKindNotification),
			attribute.String(attrMethodName, method),
		}
	case msg.ID != nil && (msg.Result != nil || msg.Error != nil):
		return []attribute.KeyValue{
			attribute.String(attrMessageKind, messageKindResponse),
			semconv.JSONRPCRequestID(requestID(msg.ID)),
		}
	default:
		return nil
	}
}
