package telemetry

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	attrServiceName    = attribute.Key("service.name")
	attrServiceVersion = attribute.Key("service.version")
)

func attrValue(t *testing.T, res *resource.Resource, key attribute.Key) (string, bool) {
	t.Helper()
	for _, kv := range res.Attributes() {
		if kv.Key == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestNewResourceIdentifiesService also pins the schema URL to the SDK's own. A
// resource built with resource.NewWithAttributes(semconv.SchemaURL, ...) carries
// this package's semconv version, and an SDK bump moving the default to a newer
// schema then fails the merge with ErrSchemaURLConflict.
func TestNewResourceIdentifiesService(t *testing.T) {
	t.Setenv(envServiceName, "")

	res, err := newResource(ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}

	name, ok := attrValue(t, res, attrServiceName)
	if !ok {
		t.Fatal("resource has no service.name attribute")
	}
	if name != "mcp-helm" {
		t.Errorf("service.name = %q, want %q", name, "mcp-helm")
	}

	version, ok := attrValue(t, res, attrServiceVersion)
	if !ok {
		t.Fatal("resource has no service.version attribute")
	}
	if version != "1.2.3" {
		t.Errorf("service.version = %q, want %q", version, "1.2.3")
	}

	if want := resource.Default().SchemaURL(); res.SchemaURL() != want {
		t.Errorf("schema URL = %q, want the SDK default %q", res.SchemaURL(), want)
	}
	if res.SchemaURL() == "" {
		t.Error("schema URL is empty, the merge dropped the SDK default")
	}
}

// TestNewResourceKeepsEnvServiceName covers OTEL_SERVICE_NAME, which
// resource.Default() reads. The schemaless resource wins the merge, so the
// compiled-in name must not be added when the environment supplies one.
func TestNewResourceKeepsEnvServiceName(t *testing.T) {
	// resource.Default() memoises its result, so resolve it before the
	// environment changes to keep this test independent of execution order.
	_ = resource.Default()
	t.Setenv(envServiceName, "from-env")

	res, err := newResource(ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}

	name, ok := attrValue(t, res, attrServiceName)
	if !ok {
		t.Fatal("resource has no service.name attribute")
	}
	if name == "mcp-helm" {
		t.Error("service.name is the compiled-in name, OTEL_SERVICE_NAME was overridden")
	}
}

// TestNewResourceKeepsServiceNameFromResourceAttributes covers the other
// variable resource.Default() reads. The OTLP specification lets service.name
// arrive through OTEL_RESOURCE_ATTRIBUTES, and the schemaless resource wins the
// merge, so the compiled-in name must not be added when it does.
func TestNewResourceKeepsServiceNameFromResourceAttributes(t *testing.T) {
	// resource.Default() memoises its result, so resolve it before the
	// environment changes to keep this test independent of execution order.
	_ = resource.Default()
	t.Setenv(envServiceName, "")
	t.Setenv(envResourceAttrs, "deployment.environment=production,service.name=from-attrs")

	res, err := newResource(ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}

	name, ok := attrValue(t, res, attrServiceName)
	if !ok {
		t.Fatal("resource has no service.name attribute")
	}
	if name == "mcp-helm" {
		t.Error("service.name is the compiled-in name, OTEL_RESOURCE_ATTRIBUTES was overridden")
	}
}

func TestServiceNameSetInEnv(t *testing.T) {
	tests := []struct {
		name       string
		serviceEnv string
		attrsEnv   string
		want       bool
	}{
		{name: "neither set"},
		{name: "service name", serviceEnv: "svc", want: true},
		{name: "resource attribute", attrsEnv: "service.name=svc", want: true},
		{name: "resource attribute among others", attrsEnv: "a=b,service.name=svc,c=d", want: true},
		{name: "unrelated resource attributes", attrsEnv: "deployment.environment=production"},
		{name: "service name only as a value", attrsEnv: "other=service.name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envServiceName, tt.serviceEnv)
			t.Setenv(envResourceAttrs, tt.attrsEnv)

			if got := serviceNameSetInEnv(); got != tt.want {
				t.Errorf("serviceNameSetInEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetupDisabledIsNoop(t *testing.T) {
	ctx := context.Background()

	tracerProvider := otel.GetTracerProvider()
	meterProvider := otel.GetMeterProvider()
	loggerProvider := logglobal.GetLoggerProvider()

	tel, err := Setup(ctx, Config{}, ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if otel.GetTracerProvider() != tracerProvider {
		t.Error("Setup registered a global tracer provider while disabled")
	}
	if otel.GetMeterProvider() != meterProvider {
		t.Error("Setup registered a global meter provider while disabled")
	}
	if logglobal.GetLoggerProvider() != loggerProvider {
		t.Error("Setup registered a global logger provider while disabled")
	}

	_, span := tel.Tracer().Start(ctx, "noop")
	if span.IsRecording() {
		t.Error("disabled telemetry produced a recording span")
	}
	span.End()

	counter, err := tel.Meter().Int64Counter("app.test.counter")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(ctx, 1)

	if tel.LoggerProvider() == nil {
		t.Fatal("LoggerProvider is nil")
	}
	tel.LoggerProvider().Logger("test")

	if err := tel.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestShutdownJoinsProviderErrors(t *testing.T) {
	first := errors.New("traces")
	second := errors.New("metrics")

	tel := newNoop()
	tel.shutdownFuncs = []func(context.Context) error{
		func(context.Context) error { return first },
		func(context.Context) error { return nil },
		func(context.Context) error { return second },
	}

	err := tel.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown returned nil, want the joined provider errors")
	}
	if !errors.Is(err, first) {
		t.Errorf("Shutdown error %v does not wrap %v", err, first)
	}
	if !errors.Is(err, second) {
		t.Errorf("Shutdown error %v does not wrap %v, an earlier failure masked it", err, second)
	}
}

// collector is a stand-in for an OTLP/HTTP receiver. It records the path of
// every request it answers: one that returns 200 for everything cannot tell a
// correctly routed export from one 404ing in production.
type collector struct {
	*httptest.Server

	mu    sync.Mutex
	paths []string
}

func newCollector(t *testing.T) *collector {
	t.Helper()

	c := &collector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)

	return c
}

func (c *collector) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.paths)
}

// emitOneOfEverything produces a single record for each signal so every
// exporter has something to flush at shutdown.
func emitOneOfEverything(t *testing.T, ctx context.Context, tel *Telemetry) {
	t.Helper()

	_, span := tel.Tracer().Start(ctx, "collector-test")
	span.End()

	histogram, err := tel.Meter().Float64Histogram("app.test.duration")
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	histogram.Record(ctx, 0.01)

	var record otellog.Record
	record.SetSeverity(otellog.SeverityInfo)
	record.SetBody(attribute.StringValue("collector-test"))
	tel.LoggerProvider().Logger("test").Emit(ctx, record)
}

// shutdown bounds the flush so a misrouted exporter fails the test by assertion
// rather than by retrying against an unreachable endpoint until the test binary
// times out.
func shutdown(t *testing.T, tel *Telemetry) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := tel.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// shutdownUnreachable tears down providers whose endpoint cannot answer. The
// flush failure is the expected outcome, so only the time it takes is bounded.
func shutdownUnreachable(t *testing.T, tel *Telemetry) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_ = tel.Shutdown(ctx)
}

// TestSetupExportsEverySignalPath: OTEL_EXPORTER_OTLP_ENDPOINT is a base URL and
// the signal path goes on top of it. Passed through verbatim, every export 404s
// without changing anything else a unit test would notice. The trailing-slash
// case is the one injected endpoints actually arrive with.
func TestSetupExportsEverySignalPath(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
	}{
		{name: "bare endpoint", suffix: ""},
		{name: "trailing slash", suffix: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := newCollector(t)

			t.Setenv(EnvEnabled, "true")
			t.Setenv(EnvEndpoint, col.URL+tt.suffix)
			t.Setenv(EnvTracesEndpoint, "")
			t.Setenv(EnvMetricsEndpoint, "")
			t.Setenv(EnvLogsEndpoint, "")

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}

			ctx := context.Background()
			tel, err := Setup(ctx, cfg, ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}

			emitOneOfEverything(t, ctx, tel)
			shutdown(t, tel)

			paths := col.seen()
			for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
				if !slices.Contains(paths, want) {
					t.Errorf("collector never received %s, got %v", want, paths)
				}
			}
			for _, got := range paths {
				if strings.HasPrefix(got, "//") {
					t.Errorf("collector received %q, the trailing slash was not trimmed", got)
				}
			}
		})
	}
}

// TestSetupIgnoresSDKEndpointEnv pins the explicit-options design: the exporters
// read OTEL_EXPORTER_OTLP_ENDPOINT themselves and do not understand grpc://, so
// an HTTP exporter must take its endpoint from the passed option.
func TestSetupIgnoresSDKEndpointEnv(t *testing.T) {
	col := newCollector(t)

	t.Setenv(EnvEndpoint, "grpc://127.0.0.1:4317")

	cfg := Config{
		Enabled: true,
		Traces:  SignalConfig{Protocol: ProtocolHTTP, Endpoint: col.URL + "/v1/traces", Insecure: true},
		Metrics: SignalConfig{Protocol: ProtocolHTTP, Endpoint: col.URL + "/v1/metrics", Insecure: true},
		Logs:    SignalConfig{Protocol: ProtocolHTTP, Endpoint: col.URL + "/v1/logs", Insecure: true},
	}

	ctx := context.Background()
	tel, err := Setup(ctx, cfg, ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	emitOneOfEverything(t, ctx, tel)
	shutdown(t, tel)

	paths := col.seen()
	for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if !slices.Contains(paths, want) {
			t.Errorf("collector never received %s, the grpc:// endpoint in %s leaked into an HTTP exporter, got %v", want, EnvEndpoint, paths)
		}
	}
}

// TestGRPCExporterConstruction covers the gRPC branch of every exporter for
// both schemes. gRPC exporters connect lazily, so construction plus shutdown is
// all a live collector would add here.
func TestGRPCExporterConstruction(t *testing.T) {
	tests := []struct {
		name string
		cfg  SignalConfig
	}{
		{name: "grpc", cfg: SignalConfig{Protocol: ProtocolGRPC, Endpoint: "collector:4317", Insecure: true}},
		{name: "grpcs", cfg: SignalConfig{Protocol: ProtocolGRPC, Endpoint: "collector:4317"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			traceExporter, err := newTraceExporter(ctx, tt.cfg)
			if err != nil {
				t.Fatalf("newTraceExporter: %v", err)
			}
			if err := traceExporter.Shutdown(ctx); err != nil {
				t.Errorf("trace exporter Shutdown: %v", err)
			}

			metricExporter, err := newMetricExporter(ctx, tt.cfg)
			if err != nil {
				t.Fatalf("newMetricExporter: %v", err)
			}
			if err := metricExporter.Shutdown(ctx); err != nil {
				t.Errorf("metric exporter Shutdown: %v", err)
			}

			logExporter, err := newLogExporter(ctx, tt.cfg)
			if err != nil {
				t.Fatalf("newLogExporter: %v", err)
			}
			if err := logExporter.Shutdown(ctx); err != nil {
				t.Errorf("log exporter Shutdown: %v", err)
			}
		})
	}
}

// TestExporterRejectsUnknownProtocol covers the branch a future signal config
// with an unset protocol would fall into.
func TestExporterRejectsUnknownProtocol(t *testing.T) {
	ctx := context.Background()
	cfg := SignalConfig{Protocol: Protocol("thrift"), Endpoint: "collector:4317"}

	if _, err := newTraceExporter(ctx, cfg); err == nil {
		t.Error("newTraceExporter accepted an unsupported protocol")
	}
	if _, err := newMetricExporter(ctx, cfg); err == nil {
		t.Error("newMetricExporter accepted an unsupported protocol")
	}
	if _, err := newLogExporter(ctx, cfg); err == nil {
		t.Error("newLogExporter accepted an unsupported protocol")
	}
}

// newTLSCollector is newCollector over TLS. Its certificate reaches the
// exporters through OTEL_EXPORTER_OTLP_CERTIFICATE, the only way to trust a
// per-test CA without adding a TLS option Setup has no production use for.
func newTLSCollector(t *testing.T) *collector {
	t.Helper()

	c := &collector{}
	c.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)

	path := filepath.Join(t.TempDir(), "collector.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate().Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write the collector certificate: %v", err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", path)

	return c
}

// TestSetupSelectsExporterPerScheme walks the four accepted schemes from the
// environment through to a constructed exporter. The HTTP schemes assert
// delivery, which is what proves https:// negotiated TLS rather than falling
// back to the plaintext exporter; the gRPC ones connect lazily, so construction
// is all there is to assert.
func TestSetupSelectsExporterPerScheme(t *testing.T) {
	tests := []struct {
		name string
		// target returns the endpoint to configure and, when the scheme is one
		// an exporter can deliver to, the collector standing behind it.
		target       func(t *testing.T) (string, *collector)
		wantProtocol Protocol
		wantInsecure bool
	}{
		{
			name:         "grpc",
			target:       func(*testing.T) (string, *collector) { return "grpc://127.0.0.1:4317", nil },
			wantProtocol: ProtocolGRPC,
			wantInsecure: true,
		},
		{
			name:         "grpcs",
			target:       func(*testing.T) (string, *collector) { return "grpcs://127.0.0.1:4317", nil },
			wantProtocol: ProtocolGRPC,
		},
		{
			name: "http",
			target: func(t *testing.T) (string, *collector) {
				col := newCollector(t)

				return col.URL, col
			},
			wantProtocol: ProtocolHTTP,
			wantInsecure: true,
		},
		{
			name: "https",
			target: func(t *testing.T) (string, *collector) {
				col := newTLSCollector(t)

				return col.URL, col
			},
			wantProtocol: ProtocolHTTP,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, col := tt.target(t)

			t.Setenv(EnvEnabled, "true")
			t.Setenv(EnvEndpoint, endpoint)
			t.Setenv(EnvTracesEndpoint, "")
			t.Setenv(EnvMetricsEndpoint, "")
			t.Setenv(EnvLogsEndpoint, "")

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}

			for name, signal := range map[string]SignalConfig{
				"traces": cfg.Traces, "metrics": cfg.Metrics, "logs": cfg.Logs,
			} {
				if signal.Protocol != tt.wantProtocol {
					t.Errorf("%s protocol = %q, want %q", name, signal.Protocol, tt.wantProtocol)
				}
				if signal.Insecure != tt.wantInsecure {
					t.Errorf("%s insecure = %v, want %v", name, signal.Insecure, tt.wantInsecure)
				}
			}

			ctx := context.Background()
			tel, err := Setup(ctx, cfg, ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}

			// Nothing is delivered over gRPC: the endpoint is a closed port.
			// The exporter existing at all is the assertion, since Setup
			// returns the error from a construction that failed.
			if col == nil {
				shutdownUnreachable(t, tel)

				return
			}

			emitOneOfEverything(t, ctx, tel)
			shutdown(t, tel)

			paths := col.seen()
			for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
				if !slices.Contains(paths, want) {
					t.Errorf("collector never received %s over %s, got %v", want, tt.name, paths)
				}
			}
		})
	}
}

// TestSetupMixedProtocols covers signals that do not share a protocol: traces
// over gRPC, metrics and logs over HTTP. Nothing in Setup ties the three
// exporters together, and this is what keeps it that way.
func TestSetupMixedProtocols(t *testing.T) {
	col := newCollector(t)

	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvEndpoint, col.URL)
	t.Setenv(EnvTracesEndpoint, "grpc://127.0.0.1:4317")
	t.Setenv(EnvMetricsEndpoint, "")
	t.Setenv(EnvLogsEndpoint, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Traces.Protocol != ProtocolGRPC {
		t.Fatalf("traces protocol = %q, want %q", cfg.Traces.Protocol, ProtocolGRPC)
	}
	if cfg.Metrics.Protocol != ProtocolHTTP || cfg.Logs.Protocol != ProtocolHTTP {
		t.Fatalf("metrics/logs protocols = %q/%q, want %q",
			cfg.Metrics.Protocol, cfg.Logs.Protocol, ProtocolHTTP)
	}

	ctx := context.Background()
	tel, err := Setup(ctx, cfg, ServiceInfo{Name: "mcp-helm", Version: "1.2.3"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// No span is emitted: the gRPC traces endpoint is a closed port, and a
	// batched span would hold the shutdown open retrying its flush.
	histogram, err := tel.Meter().Float64Histogram("app.test.duration")
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	histogram.Record(ctx, 0.01)

	var record otellog.Record
	record.SetSeverity(otellog.SeverityInfo)
	record.SetBody(attribute.StringValue("mixed-protocols"))
	tel.LoggerProvider().Logger("test").Emit(ctx, record)

	shutdown(t, tel)

	paths := col.seen()
	for _, want := range []string{"/v1/metrics", "/v1/logs"} {
		if !slices.Contains(paths, want) {
			t.Errorf("collector never received %s, got %v", want, paths)
		}
	}
	if slices.Contains(paths, "/v1/traces") {
		t.Errorf("collector received /v1/traces over HTTP, the per-signal gRPC override was ignored")
	}
}
