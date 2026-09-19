package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestMCPMetaPropagationPreservesAmbientContext(t *testing.T) {
	withTraceContextPropagator(t)

	const remote = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	for _, transport := range []Transport{TransportHTTP, TransportStdio} {
		for _, tc := range []struct {
			name     string
			parent   string
			wantLink bool
		}{
			{name: "remote parent", parent: remote, wantLink: true},
			{name: "absent parent"},
			{name: "invalid parent", parent: "invalid"},
			{name: "same parent", parent: "same"},
		} {
			t.Run(fmt.Sprintf("%d/%s", transport, tc.name), func(t *testing.T) {
				tracer, _ := newSpanRecorder(t)
				ctx, ambient := tracer.Start(context.Background(), "transport")
				defer ambient.End()
				parent := tc.parent
				if parent == "same" {
					carrier := propagation.MapCarrier{}
					propagation.TraceContext{}.Inject(ctx, carrier)
					parent = carrier.Get("traceparent")
				}
				i := newInstrumented(t, transport)
				message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"traceparent":%q}}}`, parent)
				i.server.HandleMessage(ctx, json.RawMessage(message))
				span := i.serverSpan(t)
				if tc.wantLink {
					if span.Parent().SpanID().String() != "b7ad6b7169203331" {
						t.Errorf("parent = %s, want the remote MCP parent", span.Parent().SpanID())
					}
					if links := span.Links(); len(links) != 1 || !links[0].SpanContext.Equal(ambient.SpanContext()) {
						t.Errorf("links = %v, want the ambient span %v", links, ambient.SpanContext())
					}
				} else {
					if span.Parent().TraceID() != ambient.SpanContext().TraceID() || span.Parent().SpanID() != ambient.SpanContext().SpanID() {
						t.Errorf("parent = %v, want %v", span.Parent(), ambient.SpanContext())
					}
					if len(span.Links()) != 0 {
						t.Errorf("links = %v, want no redundant link", span.Links())
					}
				}
			})
		}
	}
}

func TestMCPProtocolVersionCardinality(t *testing.T) {
	i := newInstrumented(t, TransportStdio)
	for n := range 2 {
		message := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"0-probe-%d"}}}`, n+1, n)
		if _, ok := i.server.HandleMessage(context.Background(), json.RawMessage(message)).(mcp.JSONRPCError); ok {
			t.Fatal("probe was rejected before it exercised the instrumentation")
		}
	}
	for _, span := range i.spans.Ended() {
		if v, ok := spanAttrs(span)[attrProtocolVersion]; ok {
			t.Errorf("unsupported span protocol version = %q", v)
		}
	}
	point := i.durationPoint(t)
	if v, ok := pointAttrs(point)[attrProtocolVersion]; ok {
		t.Errorf("unsupported metric protocol version = %q", v)
	}
	if point.Count != 2 {
		t.Errorf("count = %d, want both requests aggregated", point.Count)
	}
}

func TestMCPProtocolVersionSupportedMetadata(t *testing.T) {
	for _, version := range mcp.ValidProtocolVersions {
		t.Run(version, func(t *testing.T) {
			i := newInstrumented(t, TransportStdio)
			message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":%q,"io.modelcontextprotocol/clientCapabilities":{}}}}`, version)
			if _, ok := i.server.HandleMessage(context.Background(), json.RawMessage(message)).(mcp.JSONRPCError); ok {
				t.Fatal("supported version was rejected")
			}
			if got := spanAttrs(i.serverSpan(t))[attrProtocolVersion]; got != version {
				t.Errorf("span protocol = %q, want %q", got, version)
			}
			if got := pointAttrs(i.durationPoint(t))[attrProtocolVersion]; got != version {
				t.Errorf("metric protocol = %q, want %q", got, version)
			}
		})
	}
}

func TestMCPMetaPropagationWithoutAmbientLink(t *testing.T) {
	withTraceContextPropagator(t)
	i := newInstrumented(t, TransportStdio)
	i.server.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"traceparent":"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}}}`))
	span := i.serverSpan(t)
	if links := span.Links(); len(links) != 0 {
		t.Errorf("links = %v, want none without an ambient span", links)
	}
	if !span.Parent().IsRemote() || !span.Parent().IsValid() || span.SpanKind() != trace.SpanKindServer {
		t.Errorf("unexpected span parent or kind: %v, %v", span.Parent(), span.SpanKind())
	}
}
