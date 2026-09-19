package telemetry

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func TestJSONRPCBodyCaptureBounded(t *testing.T) {
	const limit = 64 << 10
	for _, size := range []int{1, 997, limit, limit + 1} {
		var capture jsonRPCBodyCapture
		chunk := []byte(strings.Repeat("x", size))
		total := 0
		for total <= 2*limit {
			n, err := capture.Write(chunk)
			if n != len(chunk) || err != nil {
				t.Fatalf("chunk size %d: Write = (%d, %v)", size, n, err)
			}
			total += n
			if capture.n != min(total, limit) || len(capture.data) != limit {
				t.Fatalf("chunk size %d: captured %d bytes in %d-byte storage", size, capture.n, len(capture.data))
			}
			if capture.overflow != (total > limit) {
				t.Fatalf("chunk size %d: overflow = %v after %d bytes", size, capture.overflow, total)
			}
		}
		if string(capture.data[:capture.n]) != strings.Repeat("x", limit) {
			t.Errorf("chunk size %d: captured prefix changed", size)
		}
	}
}

func TestAnnotateJSONRPCBodyLimit(t *testing.T) {
	const limit = 64 << 10
	message := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	prefix := message + strings.Repeat(" ", limit-len(message))

	for _, tt := range []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "exact limit",
			body: prefix,
			want: map[string]string{
				attrMessageKind: messageKindNotification,
				attrMethodName:  "notifications/initialized",
			},
		},
		{name: "one byte over", body: prefix + " "},
		{name: "valid prefix invalid suffix", body: prefix + "invalid"},
		{name: "large valid body", body: prefix + strings.Repeat(" ", 4*limit)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tracer, sr := newSpanRecorder(t)
			ctx, span := tracer.Start(context.Background(), "POST /mcp")
			body := &jsonRPCReadCloser{reader: strings.NewReader(tt.body), chunkSize: 997}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", body)
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				if string(got) != tt.body {
					t.Errorf("downstream body changed: got %d bytes, want %d", len(got), len(tt.body))
				}
			})
			AnnotateJSONRPC(next).ServeHTTP(httptest.NewRecorder(), req)
			span.End()
			if got := spanAttrs(sr.Ended()[0]); !maps.Equal(got, tt.want) {
				t.Errorf("attributes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAnnotateJSONRPCKnownOversizedBody(t *testing.T) {
	tracer, sr := newSpanRecorder(t)
	ctx, span := tracer.Start(context.Background(), "POST /mcp")
	payload := strings.Repeat(" ", 64<<10) + `{"method":"ping"}`
	body := &jsonRPCReadCloser{reader: strings.NewReader(payload)}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", body)
	req.ContentLength = int64(len(payload))
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Body != body {
			t.Error("known oversized body must not be wrapped")
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != payload {
			t.Errorf("downstream body changed: bytes = %d, err = %v", len(got), err)
		}
	})
	AnnotateJSONRPC(next).ServeHTTP(httptest.NewRecorder(), req)
	span.End()
	if attrs := sr.Ended()[0].Attributes(); len(attrs) != 0 {
		t.Errorf("oversized body was annotated: %v", attrs)
	}
}

func TestAnnotateJSONRPCNonRecording(t *testing.T) {
	body := &jsonRPCReadCloser{reader: strings.NewReader(`{"method":"ping"}`)}
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if r.Body != body {
			t.Error("nonrecording span must leave Body untouched")
		}
	})
	AnnotateJSONRPC(next).ServeHTTP(httptest.NewRecorder(), req)
	if !called || body.reads != 0 || body.closes != 0 {
		t.Errorf("called = %v, reads = %d, closes = %d", called, body.reads, body.closes)
	}
}

func TestAnnotateJSONRPCBodyErrors(t *testing.T) {
	tracer, _ := newSpanRecorder(t)
	ctx, span := tracer.Start(context.Background(), "POST /mcp")
	defer span.End()
	readErr := errors.New("body read failed")
	closeErr := errors.New("body close failed")
	message := `{"method":"ping"}`
	body := &jsonRPCReadCloser{
		reader: strings.NewReader(message), readErr: readErr, closeErr: closeErr,
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", body)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if body.reads != 0 {
			t.Error("middleware read body ahead of downstream handler")
		}
		buf := make([]byte, 128)
		n, err := r.Body.Read(buf)
		if string(buf[:n]) != message || err != readErr {
			t.Errorf("Read = (%q, %v), want (%q, %v)", buf[:n], err, message, readErr)
		}
		if err := r.Body.Close(); err != closeErr {
			t.Errorf("Close = %v, want %v", err, closeErr)
		}
	})
	AnnotateJSONRPC(next).ServeHTTP(httptest.NewRecorder(), req)
	if body.reads != 1 || body.closes != 1 {
		t.Errorf("reads = %d, closes = %d, want one each", body.reads, body.closes)
	}
}

type jsonRPCReadCloser struct {
	reader    *strings.Reader
	chunkSize int
	readErr   error
	closeErr  error
	reads     int
	closes    int
}

func (b *jsonRPCReadCloser) Read(p []byte) (int, error) {
	b.reads++
	if b.chunkSize > 0 {
		p = p[:min(len(p), b.chunkSize)]
	}
	n, err := b.reader.Read(p)
	if b.reader.Len() == 0 && b.readErr != nil {
		err = b.readErr
	}
	return n, err
}

func (b *jsonRPCReadCloser) Close() error {
	b.closes++
	return b.closeErr
}

func TestAnnotateJSONRPC(t *testing.T) {
	const requestID = string(semconv.JSONRPCRequestIDKey)

	tests := []struct {
		name   string
		method string
		body   string
		want   map[string]string
	}{
		{
			// The method and id are on the MCP span, so repeating them here
			// would count every request twice.
			name:   "request",
			method: http.MethodPost,
			body:   `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`,
			want:   map[string]string{"mcp_helm.jsonrpc.message.kind": messageKindRequest},
		},
		{
			name:   "notification",
			method: http.MethodPost,
			body:   `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			want: map[string]string{
				attrMessageKind: messageKindNotification,
				attrMethodName:  "notifications/initialized",
			},
		},
		{
			name:   "unknown notification",
			method: http.MethodPost,
			body:   `{"jsonrpc":"2.0","method":"notifications/invented-by-the-client"}`,
			want: map[string]string{
				attrMessageKind: messageKindNotification,
				attrMethodName:  methodOther,
			},
		},
		{
			name:   "ping reply",
			method: http.MethodPost,
			body:   `{"jsonrpc":"2.0","id":123,"result":{}}`,
			want: map[string]string{
				attrMessageKind: messageKindResponse,
				requestID:       "123",
			},
		},
		{
			name:   "error reply",
			method: http.MethodPost,
			body:   `{"jsonrpc":"2.0","id":"a","error":{"code":-32601,"message":"no"}}`,
			want: map[string]string{
				attrMessageKind: messageKindResponse,
				requestID:       "a",
			},
		},
		{
			name:   "batch",
			method: http.MethodPost,
			body:   `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		},
		{
			name:   "not json",
			method: http.MethodPost,
			body:   `hello`,
		},
		{
			name:   "delete",
			method: http.MethodDelete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, sr := newSpanRecorder(t)

			var received string
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				received = string(body)
			})

			ctx, span := tracer.Start(context.Background(), "POST /mcp")
			req := httptest.NewRequestWithContext(ctx, tt.method, "/mcp", strings.NewReader(tt.body))
			AnnotateJSONRPC(next).ServeHTTP(httptest.NewRecorder(), req)
			span.End()

			if received != tt.body {
				t.Errorf("handler read %q, want the body unchanged: %q", received, tt.body)
			}
			if got := spanAttrs(sr.Ended()[0]); !maps.Equal(got, tt.want) {
				t.Errorf("attributes = %v, want %v", got, tt.want)
			}
		})
	}
}
