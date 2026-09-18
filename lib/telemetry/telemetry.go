package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zekker6/mcp-helm/lib/logger"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

// scopeName is the instrumentation scope reported for every tracer and meter
// handed out by Telemetry.
const scopeName = "github.com/zekker6/mcp-helm"

// Read by resource.Default(). Checked here only so the compiled-in service name
// does not silently win over either.
const (
	envServiceName       = "OTEL_SERVICE_NAME"
	envResourceAttrs     = "OTEL_RESOURCE_ATTRIBUTES"
	resourceServiceNameK = "service.name"
)

// ServiceInfo identifies this service in the OpenTelemetry resource.
type ServiceInfo struct {
	Name    string
	Version string
}

// Telemetry owns the providers built by Setup. Its accessors always return
// usable providers, so instrumented call sites never branch on whether
// telemetry is enabled.
type Telemetry struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	loggerProvider log.LoggerProvider

	shutdownFuncs []func(context.Context) error
}

// Tracer returns a tracer for this service's instrumentation scope.
func (t *Telemetry) Tracer() trace.Tracer {
	return t.tracerProvider.Tracer(scopeName)
}

// Meter returns a meter for this service's instrumentation scope.
func (t *Telemetry) Meter() metric.Meter {
	return t.meterProvider.Meter(scopeName)
}

// LoggerProvider returns the provider the zap bridge writes into.
func (t *Telemetry) LoggerProvider() log.LoggerProvider {
	return t.loggerProvider
}

// Shutdown flushes and stops every provider Setup created. Failures are joined,
// so one provider refusing to flush cannot hide what the others did.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, shutdown := range t.shutdownFuncs {
		if err := shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Setup builds one exporter per signal from cfg, registers the providers
// globally along with a W3C propagator, and starts Go runtime metrics.
//
// With cfg.Enabled false it returns no-op providers instead: no exporter, no
// connection, nothing registered, and Shutdown does nothing.
func Setup(ctx context.Context, cfg Config, info ServiceInfo) (*Telemetry, error) {
	if !cfg.Enabled {
		return newNoop(), nil
	}

	res, err := newResource(info)
	if err != nil {
		return nil, err
	}

	t := &Telemetry{}
	// A half-built Setup would leak the exporters that did succeed.
	fail := func(err error) (*Telemetry, error) {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}

	traceExporter, err := newTraceExporter(ctx, cfg.Traces)
	if err != nil {
		return fail(fmt.Errorf("build trace exporter: %w", err))
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	t.tracerProvider = tracerProvider
	t.shutdownFuncs = append(t.shutdownFuncs, tracerProvider.Shutdown)

	metricExporter, err := newMetricExporter(ctx, cfg.Metrics)
	if err != nil {
		return fail(fmt.Errorf("build metric exporter: %w", err))
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	t.meterProvider = meterProvider
	t.shutdownFuncs = append(t.shutdownFuncs, meterProvider.Shutdown)

	logExporter, err := newLogExporter(ctx, cfg.Logs)
	if err != nil {
		return fail(fmt.Errorf("build log exporter: %w", err))
	}
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	t.loggerProvider = loggerProvider
	t.shutdownFuncs = append(t.shutdownFuncs, loggerProvider.Shutdown)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	logglobal.SetLoggerProvider(loggerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// The span batcher reports export failures here, so without a handler a
	// misrouted traces endpoint looks like "no traces" rather than an error.
	//
	// stderr only: the log SDK reports its own export failures through this
	// same handler, and bridging them would keep the queue from ever draining.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Base().Error("opentelemetry sdk error", zap.Error(err))
	}))

	if err := runtime.Start(runtime.WithMeterProvider(meterProvider)); err != nil {
		return fail(fmt.Errorf("start runtime metrics: %w", err))
	}

	return t, nil
}

// newTraceExporter builds the OTLP span exporter for the resolved protocol.
//
// The endpoint is always passed explicitly: the exporters read
// OTEL_EXPORTER_OTLP_ENDPOINT themselves and would not understand a grpc://
// value there. Options are applied after the environment, so ours win.
func newTraceExporter(ctx context.Context, cfg SignalConfig) (sdktrace.SpanExporter, error) {
	switch cfg.Protocol {
	case ProtocolGRPC:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	case ProtocolHTTP:
		// WithEndpointURL takes the URL verbatim, signal path included, and
		// derives TLS from its scheme, so no separate WithInsecure is wanted.
		return otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	default:
		return nil, unsupportedProtocol(cfg.Protocol)
	}
}

// newMetricExporter builds the OTLP metric exporter for the resolved protocol.
func newMetricExporter(ctx context.Context, cfg SignalConfig) (sdkmetric.Exporter, error) {
	switch cfg.Protocol {
	case ProtocolGRPC:
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, opts...)
	case ProtocolHTTP:
		return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
	default:
		return nil, unsupportedProtocol(cfg.Protocol)
	}
}

// newLogExporter builds the OTLP log record exporter for the resolved protocol.
func newLogExporter(ctx context.Context, cfg SignalConfig) (sdklog.Exporter, error) {
	switch cfg.Protocol {
	case ProtocolGRPC:
		opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		return otlploggrpc.New(ctx, opts...)
	case ProtocolHTTP:
		return otlploghttp.New(ctx, otlploghttp.WithEndpointURL(cfg.Endpoint))
	default:
		return nil, unsupportedProtocol(cfg.Protocol)
	}
}

func unsupportedProtocol(p Protocol) error {
	return fmt.Errorf("unsupported protocol %q, accepted schemes are %s", p, acceptedSchemes)
}

// newNoop returns a Telemetry that discards every signal.
func newNoop() *Telemetry {
	return &Telemetry{
		tracerProvider: tracenoop.NewTracerProvider(),
		meterProvider:  metricnoop.NewMeterProvider(),
		loggerProvider: lognoop.NewLoggerProvider(),
	}
}

// newResource describes this service to the collector.
//
// The custom attributes go into a schemaless resource so the SDK's default
// keeps owning the schema URL. resource.NewWithAttributes(semconv.SchemaURL, ...)
// would pin it to this file's semconv version, and an SDK bump moving the
// default resource to a newer one then fails the merge with
// ErrSchemaURLConflict.
func newResource(info ServiceInfo) (*resource.Resource, error) {
	attrs := make([]attribute.KeyValue, 0, 2)
	// The schemaless resource wins the merge, so only fall back to the
	// compiled-in name when the environment supplies none.
	if info.Name != "" && !serviceNameSetInEnv() {
		attrs = append(attrs, semconv.ServiceName(info.Name))
	}
	if info.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(info.Version))
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	return res, nil
}

// serviceNameSetInEnv reports whether the environment already names the
// service. Both variables resource.Default() reads count: service.name may
// arrive through OTEL_RESOURCE_ATTRIBUTES as well.
func serviceNameSetInEnv() bool {
	if os.Getenv(envServiceName) != "" {
		return true
	}

	for _, pair := range strings.Split(os.Getenv(envResourceAttrs), ",") {
		key, _, found := strings.Cut(pair, "=")
		if found && strings.TrimSpace(key) == resourceServiceNameK {
			return true
		}
	}

	return false
}
