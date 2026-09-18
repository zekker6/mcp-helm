package logger

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// recordingProvider is a log.LoggerProvider that keeps every emitted record.
type recordingProvider struct {
	embedded.LoggerProvider

	logger *recordingLogger
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{logger: &recordingLogger{}}
}

func (p *recordingProvider) Logger(string, ...log.LoggerOption) log.Logger {
	return p.logger
}

type recordingLogger struct {
	embedded.Logger

	mu      sync.Mutex
	records []emitted
}

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

func (l *recordingLogger) bodies() []string {
	out := []string{}
	for _, e := range l.all() {
		out = append(out, e.record.Body().AsString())
	}

	return out
}

// resetLogger clears the package singleton so each test starts from a known
// state and leaves nothing behind for the next one.
func resetLogger(t *testing.T) {
	t.Helper()

	logger = nil
	baseLogger = nil
	level = nil
	t.Cleanup(func() {
		logger = nil
		baseLogger = nil
		level = nil
	})
}

// captureStderr redirects os.Stderr to a temp file. It must run before Init,
// which resolves the stderr sink at build time.
func captureStderr(t *testing.T) func() string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create temp stderr: %v", err)
	}

	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = orig
		_ = f.Close()
	})

	return func() string {
		out, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("read captured stderr: %v", err)
		}

		return string(out)
	}
}

// observeLogger installs a logger writing into an observer, wrapped the same
// way Init wraps the stderr core.
func observeLogger(t *testing.T) *observer.ObservedLogs {
	t.Helper()

	resetLogger(t)
	core, logs := observer.New(zap.DebugLevel)
	logger = zap.New(&fieldFilteringCore{Core: core, drop: isContextField})

	return logs
}

func sampledContext() context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
		TraceFlags: trace.FlagsSampled,
	})

	return trace.ContextWithSpanContext(context.Background(), sc)
}

func fieldByKey(fields []zapcore.Field, key string) (zapcore.Field, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f, true
		}
	}

	return zapcore.Field{}, false
}

// TestAttachProviderAfterInit pins the singleton-guard regression: Init returns
// early once the logger exists, so the bridge has to be attachable afterwards.
func TestAttachProviderAfterInit(t *testing.T) {
	resetLogger(t)
	captureStderr(t)

	Init()
	Info("before attach")

	provider := newRecordingProvider()
	AttachProvider(provider)

	Info("after attach")

	got := provider.logger.bodies()
	if len(got) != 1 || got[0] != "after attach" {
		t.Fatalf("provider records = %v, want [\"after attach\"]", got)
	}
}

func TestAttachProviderKeepsStderrOutput(t *testing.T) {
	resetLogger(t)
	readStderr := captureStderr(t)

	Init()
	AttachProvider(newRecordingProvider())
	Info("tee'd line")
	Stop()

	if out := readStderr(); !strings.Contains(out, "tee'd line") {
		t.Fatalf("stderr output = %q, want it to contain the logged message", out)
	}
}

func TestCtxEmitsTraceFields(t *testing.T) {
	logs := observeLogger(t)

	Ctx(sampledContext()).Info("with span")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("observed %d entries, want 1", len(entries))
	}

	traceID, ok := fieldByKey(entries[0].Context, "trace_id")
	if !ok {
		t.Fatalf("fields = %v, want a trace_id field", entries[0].Context)
	}
	if traceID.String != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %q, want the span context's trace id", traceID.String)
	}

	spanID, ok := fieldByKey(entries[0].Context, "span_id")
	if !ok {
		t.Fatalf("fields = %v, want a span_id field", entries[0].Context)
	}
	if spanID.String != "1112131415161718" {
		t.Errorf("span_id = %q, want the span context's span id", spanID.String)
	}
}

func TestCtxWithoutSpanEmitsNoTraceFields(t *testing.T) {
	logs := observeLogger(t)

	Ctx(context.Background()).Info("no span")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("observed %d entries, want 1", len(entries))
	}

	for _, key := range []string{"trace_id", "span_id", contextFieldKey} {
		if _, ok := fieldByKey(entries[0].Context, key); ok {
			t.Errorf("fields = %v, want no %s field", entries[0].Context, key)
		}
	}
}

func TestCtxStderrOutputOmitsContextField(t *testing.T) {
	resetLogger(t)
	readStderr := captureStderr(t)

	Init()
	AttachProvider(newRecordingProvider())
	Ctx(sampledContext()).Info("correlated")
	Stop()

	out := strings.TrimSpace(readStderr())
	if out == "" {
		t.Fatal("stderr output is empty, want one JSON log line")
	}

	var line map[string]any
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("stderr line is not JSON (%v): %q", err, out)
	}

	if _, ok := line[contextFieldKey]; ok {
		t.Errorf("stderr line = %v, want no encoded %q field", line, contextFieldKey)
	}
	if line["trace_id"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("stderr trace_id = %v, want the span context's trace id", line["trace_id"])
	}
}

// TestCtxCarriesContextToProvider checks the field the bridge consumes survives
// the tee, so records land under the active trace and not a detached one.
func TestCtxCarriesContextToProvider(t *testing.T) {
	resetLogger(t)
	captureStderr(t)

	provider := newRecordingProvider()
	Init()
	AttachProvider(provider)

	ctx := sampledContext()
	Ctx(ctx).Info("correlated")

	records := provider.logger.all()
	if len(records) != 1 {
		t.Fatalf("provider recorded %d records, want 1", len(records))
	}

	got := trace.SpanContextFromContext(records[0].ctx)
	want := trace.SpanContextFromContext(ctx)
	if !got.Equal(want) {
		t.Errorf("emit context span = %v, want %v", got, want)
	}
}

// TestBridgedRecordOmitsTraceFields is the other half of the correlation: the
// SDK derives the record's trace ids from the emit context, so exporting the
// stderr line's copies as attributes would put both on the wire twice.
func TestBridgedRecordOmitsTraceFields(t *testing.T) {
	resetLogger(t)
	captureStderr(t)

	provider := newRecordingProvider()
	Init()
	AttachProvider(provider)

	Ctx(sampledContext()).Info("correlated")

	records := provider.logger.all()
	if len(records) != 1 {
		t.Fatalf("provider recorded %d records, want 1", len(records))
	}

	records[0].record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == traceIDFieldKey || kv.Key == spanIDFieldKey {
			t.Errorf("record carries a %q attribute, want it on stderr only", kv.Key)
		}

		return true
	})
}

func TestRecordBeforeStopReachesProvider(t *testing.T) {
	resetLogger(t)
	captureStderr(t)

	provider := newRecordingProvider()
	Init()
	AttachProvider(provider)

	Info("last line")
	Stop()

	if got := provider.logger.bodies(); len(got) != 1 || got[0] != "last line" {
		t.Fatalf("provider records = %v, want [\"last line\"]", got)
	}
}

func TestFilterLeavesInputUntouched(t *testing.T) {
	ctxField := zap.Any(contextFieldKey, context.Background())
	fields := []zapcore.Field{ctxField, zap.String("k", "v")}

	kept := (&fieldFilteringCore{drop: isContextField}).filter(fields)

	if len(kept) != 1 || kept[0].Key != "k" {
		t.Fatalf("kept = %v, want only the string field", kept)
	}
	if len(fields) != 2 || fields[0].Key != contextFieldKey {
		t.Fatalf("input slice was modified: %v", fields)
	}
}

func TestFilterWithoutMatchReturnsInput(t *testing.T) {
	fields := []zapcore.Field{zap.String("k", "v")}

	if kept := (&fieldFilteringCore{drop: isContextField}).filter(fields); len(kept) != 1 || kept[0].Key != "k" {
		t.Fatalf("kept = %v, want the input unchanged", kept)
	}
}

// TestBaseBypassesProvider pins the loop-breaker: records the OpenTelemetry SDK
// produces about its own export failures must not be queued for export, or a
// collector outage would keep the log queue permanently non-empty.
func TestBaseBypassesProvider(t *testing.T) {
	resetLogger(t)
	readStderr := captureStderr(t)

	Init()
	provider := newRecordingProvider()
	AttachProvider(provider)

	Base().Error("sdk error")
	Error("bridged error")
	Stop()

	got := provider.logger.bodies()
	if len(got) != 1 || got[0] != "bridged error" {
		t.Fatalf("provider records = %v, want only the bridged one", got)
	}

	out := readStderr()
	for _, want := range []string{"sdk error", "bridged error"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to contain %q", out, want)
		}
	}
}

// TestLogLevelFiltersBothSinks covers the level gate contextStrippingCore.Check
// re-implements: without it every record would be written and shipped to OTLP
// regardless of -logLevel.
func TestLogLevelFiltersBothSinks(t *testing.T) {
	tests := []struct {
		level   string
		emitted []string
	}{
		{level: "debug", emitted: []string{"dbg", "inf", "wrn", "err"}},
		{level: "info", emitted: []string{"inf", "wrn", "err"}},
		{level: "warn", emitted: []string{"wrn", "err"}},
		{level: "error", emitted: []string{"err"}},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			resetLogger(t)
			readStderr := captureStderr(t)

			orig := *logLevel
			*logLevel = tt.level
			t.Cleanup(func() { *logLevel = orig })

			Init()
			provider := newRecordingProvider()
			AttachProvider(provider)

			Debug("dbg")
			Info("inf")
			Warn("wrn")
			Error("err")
			Stop()

			if got := provider.logger.bodies(); !slices.Equal(got, tt.emitted) {
				t.Errorf("provider records = %v, want %v", got, tt.emitted)
			}

			out := readStderr()
			for _, msg := range []string{"dbg", "inf", "wrn", "err"} {
				want := slices.Contains(tt.emitted, msg)
				if got := strings.Contains(out, `"msg":"`+msg+`"`); got != want {
					t.Errorf("stderr contains %q = %v, want %v (output %q)", msg, got, want, out)
				}
			}
		})
	}
}

// TestInitRejectsUnknownLogLevel pins the flag validation: an unparseable level
// must not silently fall back to a permissive one.
func TestInitRejectsUnknownLogLevel(t *testing.T) {
	resetLogger(t)

	orig := *logLevel
	*logLevel = "verbose"
	t.Cleanup(func() { *logLevel = orig })

	defer func() {
		if recover() == nil {
			t.Error("Init() with an unknown level did not panic")
		}
	}()

	Init()
}
