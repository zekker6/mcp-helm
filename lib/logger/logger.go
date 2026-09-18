package logger

import (
	"context"
	"flag"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// scopeName is the instrumentation scope reported for records bridged to
// OpenTelemetry.
const scopeName = "github.com/zekker6/mcp-helm"

// contextFieldKey names the field Ctx attaches so the OpenTelemetry bridge can
// find the emitting context. The bridge matches on the value's type, not the
// key, and the stderr core drops it by that same check.
const contextFieldKey = "context"

// The correlation ids Ctx attaches for the stderr line.
const (
	traceIDFieldKey = "trace_id"
	spanIDFieldKey  = "span_id"
)

var logger *zap.Logger

// baseLogger is the stderr-only logger Init builds. AttachProvider leaves it
// untouched, so Base keeps a route that skips the OpenTelemetry bridge.
var baseLogger *zap.Logger

// level is the threshold Init resolved from -logLevel. AttachProvider needs it
// separately because the bridge core does not derive one of its own.
var level zapcore.LevelEnabler

var (
	logLevel = flag.String("logLevel", "info", "Set the log level (debug, info, warn, error)")
)

func Init() {
	if logger != nil {
		return
	}

	cfg := zap.NewProductionConfig()
	switch *logLevel {
	case "debug":
		cfg.Level.SetLevel(zap.DebugLevel)
	case "info":
		cfg.Level.SetLevel(zap.InfoLevel)
	case "warn":
		cfg.Level.SetLevel(zap.WarnLevel)
	case "error":
		cfg.Level.SetLevel(zap.ErrorLevel)
	default:
		panic("unknown log level: " + *logLevel)
	}

	l, _ := cfg.Build(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return &fieldFilteringCore{Core: c, drop: isContextField}
	}))
	logger = l
	baseLogger = l
	level = cfg.Level
}

// Base returns the stderr-only logger, bypassing the bridge AttachProvider tees
// onto the package logger.
//
// It exists for records the telemetry pipeline produces about itself: the SDK
// reports export failures through otel.Handle, and bridging those closes a loop
// where each failed export queues an ERROR record for the next one.
func Base() *zap.Logger {
	Init()

	return baseLogger
}

// AttachProvider tees the active logger onto an OpenTelemetry log bridge, so
// every subsequent record reaches lp in addition to stderr.
//
// Not an Init option: Init returns early once the singleton exists, so anything
// logged before telemetry is configured would bind the logger without the
// bridge and keep every later record out of OTLP.
func AttachProvider(lp log.LoggerProvider) {
	Init()

	otelCore := &levelGatedCore{
		Core: &fieldFilteringCore{
			Core: otelzap.NewCore(scopeName, otelzap.WithLoggerProvider(lp)),
			drop: isTraceField,
		},
		level: level,
	}
	logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, otelCore)
	}))
}

// levelGatedCore applies the -logLevel threshold to a core that would otherwise
// accept every record. The bridge delegates Enabled to the log SDK, which
// enables every severity, and zapcore.NewTee consults each core separately: a
// binary started with -logLevel=error would still ship DEBUG to the collector.
type levelGatedCore struct {
	zapcore.Core

	level zapcore.LevelEnabler
}

func (c *levelGatedCore) Enabled(l zapcore.Level) bool {
	return c.level.Enabled(l) && c.Core.Enabled(l)
}

func (c *levelGatedCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !c.Enabled(ent.Level) {
		return ce
	}

	return c.Core.Check(ent, ce)
}

func (c *levelGatedCore) With(fields []zapcore.Field) zapcore.Core {
	return &levelGatedCore{Core: c.Core.With(fields), level: c.level}
}

// Ctx returns a logger bound to ctx: the stderr line carries the trace_id and
// span_id of the active span, and the context itself rides along so the bridge
// emits the record under that same trace.
//
// Only stderr gets the two ids as fields. The bridge fills the record's own
// trace fields in from the context, so exporting them as attributes as well
// would put each on the wire twice - see fieldFilteringCore.
func Ctx(ctx context.Context) *zap.Logger {
	Init()

	fields := []zap.Field{zap.Any(contextFieldKey, ctx)}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		fields = append(fields,
			zap.String(traceIDFieldKey, sc.TraceID().String()),
			zap.String(spanIDFieldKey, sc.SpanID().String()),
		)
	}

	return logger.With(fields...)
}

func Error(msg string, fields ...zap.Field) {
	Init()
	logger.WithOptions(zap.AddCallerSkip(1)).Error(msg, fields...)
}

func Info(msg string, fields ...zap.Field) {
	Init()

	logger.WithOptions(zap.AddCallerSkip(1)).Info(msg, fields...)
}

func Debug(msg string, fields ...zap.Field) {
	Init()

	logger.WithOptions(zap.AddCallerSkip(1)).Debug(msg, fields...)
}

func Warn(msg string, fields ...zap.Field) {
	Init()

	logger.WithOptions(zap.AddCallerSkip(1)).Warn(msg, fields...)
}

func With(fields ...zap.Field) *zap.Logger {
	return logger.WithOptions(zap.AddCallerSkip(1)).With(fields...)
}

func Stop() {
	if logger != nil {
		_ = logger.Sync()
	}
}

// fieldFilteringCore drops the fields drop selects before the wrapped core
// encodes them.
//
// zapcore.NewTee hands the same field set to every core, but the two fields Ctx
// attaches are each wanted on one side only: the JSON encoder would
// reflect-marshal the context into the stderr line, and the bridge would export
// the trace ids a second time beside the ones it fills in from that context.
type fieldFilteringCore struct {
	zapcore.Core

	drop func(zapcore.Field) bool
}

func (c *fieldFilteringCore) With(fields []zapcore.Field) zapcore.Core {
	return &fieldFilteringCore{Core: c.Core.With(c.filter(fields)), drop: c.drop}
}

// Check cannot be inherited: the embedded core would add itself to the checked
// entry and Write would never run.
func (c *fieldFilteringCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}

	return ce
}

func (c *fieldFilteringCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(ent, c.filter(fields))
}

// filter returns fields without the ones drop selects. The input slice is
// shared with the other cores of the tee, so it is never modified in place.
func (c *fieldFilteringCore) filter(fields []zapcore.Field) []zapcore.Field {
	kept := make([]zapcore.Field, 0, len(fields))
	for _, f := range fields {
		if c.drop(f) {
			continue
		}
		kept = append(kept, f)
	}

	return kept
}

func isContextField(f zapcore.Field) bool {
	_, ok := f.Interface.(context.Context)

	return ok
}

func isTraceField(f zapcore.Field) bool {
	return f.Key == traceIDFieldKey || f.Key == spanIDFieldKey
}
