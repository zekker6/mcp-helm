// Package telemetry owns the OpenTelemetry SDK setup for mcp-helm.
package telemetry

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Environment variables read by ConfigFromEnv. Everything else the OTLP
// exporters understand (headers, timeout, compression, export interval) is read
// by the SDK itself and is not re-implemented here.
const (
	EnvEnabled         = "OTEL_ENABLED"
	EnvEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	EnvLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

// Protocol is the OTLP wire protocol resolved from an endpoint's scheme.
type Protocol string

const (
	ProtocolGRPC Protocol = "grpc"
	ProtocolHTTP Protocol = "http"
)

// acceptedSchemes is named in every scheme-resolution error.
const acceptedSchemes = "grpc, grpcs, http, https"

// SignalConfig is the resolved exporter configuration for one OTLP signal.
//
// Endpoint is a full URL including the signal path for ProtocolHTTP, and a
// bare host:port for ProtocolGRPC.
type SignalConfig struct {
	Protocol Protocol
	Endpoint string
	Insecure bool
}

// Config is the telemetry configuration for all three signals. The zero value
// is a valid, fully disabled configuration.
type Config struct {
	Enabled bool
	Traces  SignalConfig
	Metrics SignalConfig
	Logs    SignalConfig
}

// ConfigFromEnv builds a Config from the environment.
//
// Without a true-ish OTEL_ENABLED it returns a disabled zero value and no
// error, so a stale endpoint cannot fail startup. When enabled, each signal
// resolves from its own endpoint variable or from the base one, and anything
// unresolvable is an error naming the offending variable.
func ConfigFromEnv() (Config, error) {
	if !envBool(EnvEnabled) {
		return Config{}, nil
	}

	cfg := Config{Enabled: true}
	base := strings.TrimSpace(os.Getenv(EnvEndpoint))

	signals := []struct {
		env    string
		path   string
		target *SignalConfig
	}{
		{EnvTracesEndpoint, "/v1/traces", &cfg.Traces},
		{EnvMetricsEndpoint, "/v1/metrics", &cfg.Metrics},
		{EnvLogsEndpoint, "/v1/logs", &cfg.Logs},
	}

	for _, s := range signals {
		if override := strings.TrimSpace(os.Getenv(s.env)); override != "" {
			resolved, err := resolveSignal(override, "")
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", s.env, err)
			}
			*s.target = resolved
			continue
		}

		if base == "" {
			return Config{}, fmt.Errorf("%s=true requires %s or %s to be set", EnvEnabled, EnvEndpoint, s.env)
		}

		resolved, err := resolveSignal(base, s.path)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvEndpoint, err)
		}
		*s.target = resolved
	}

	return cfg, nil
}

// resolveSignal turns an endpoint into exporter settings. signalPath is
// appended to HTTP endpoints after trimming a trailing slash; pass "" to use
// the endpoint verbatim, which is what the OTLP spec mandates for per-signal
// variables. gRPC endpoints keep only host:port - gRPC has no signal path.
func resolveSignal(raw, signalPath string) (SignalConfig, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return SignalConfig{}, fmt.Errorf("invalid URL %q: %w", raw, err)
	}

	var (
		protocol Protocol
		insecure bool
	)
	switch u.Scheme {
	case "grpc":
		protocol, insecure = ProtocolGRPC, true
	case "grpcs":
		protocol = ProtocolGRPC
	case "http":
		protocol, insecure = ProtocolHTTP, true
	case "https":
		protocol = ProtocolHTTP
	default:
		return SignalConfig{}, fmt.Errorf("invalid URL %q: unsupported scheme %q, accepted schemes are %s", raw, u.Scheme, acceptedSchemes)
	}

	if u.Host == "" {
		return SignalConfig{}, fmt.Errorf("invalid URL %q: missing host", raw)
	}

	endpoint := u.Host
	if protocol == ProtocolHTTP {
		endpoint = raw
		if signalPath != "" {
			endpoint = strings.TrimSuffix(raw, "/") + signalPath
		}
	}

	return SignalConfig{Protocol: protocol, Endpoint: endpoint, Insecure: insecure}, nil
}

func envBool(name string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && v
}
