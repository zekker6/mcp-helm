package telemetry

import (
	"strings"
	"testing"
)

// clearEnv unsets every variable ConfigFromEnv reads so a developer's own OTEL_*
// settings cannot leak into a test case.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvEnabled,
		EnvEndpoint,
		EnvTracesEndpoint,
		EnvMetricsEndpoint,
		EnvLogsEndpoint,
	} {
		t.Setenv(name, "")
	}
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	clearEnv(t)
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func TestConfigFromEnvDisabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled string
	}{
		{name: "unset", enabled: ""},
		{name: "false", enabled: "false"},
		{name: "zero", enabled: "0"},
		{name: "uppercase false", enabled: "FALSE"},
		{name: "not a bool", enabled: "yes"},
		{name: "empty after trim", enabled: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Endpoints are deliberately invalid: with telemetry off they must
			// not be looked at, let alone rejected.
			setEnv(t, map[string]string{
				EnvEnabled:        tt.enabled,
				EnvEndpoint:       "ftp://nowhere",
				EnvTracesEndpoint: "://broken",
			})

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v, want nil", err)
			}
			if cfg != (Config{}) {
				t.Errorf("ConfigFromEnv() = %+v, want zero Config", cfg)
			}
		})
	}
}

func TestConfigFromEnvEnabledTruthy(t *testing.T) {
	for _, enabled := range []string{"true", "TRUE", "True", "1", "t", " true "} {
		t.Run(enabled, func(t *testing.T) {
			setEnv(t, map[string]string{
				EnvEnabled:  enabled,
				EnvEndpoint: "http://collector:4318",
			})

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v, want nil", err)
			}
			if !cfg.Enabled {
				t.Errorf("Enabled = false, want true for %q", enabled)
			}
		})
	}
}

func TestConfigFromEnvScheme(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		protocol Protocol
		insecure bool
		traces   string
		metrics  string
		logs     string
	}{
		{
			name:     "grpc is plaintext gRPC on host:port",
			base:     "grpc://collector:4317",
			protocol: ProtocolGRPC,
			insecure: true,
			traces:   "collector:4317",
			metrics:  "collector:4317",
			logs:     "collector:4317",
		},
		{
			name:     "grpcs is TLS gRPC on host:port",
			base:     "grpcs://otel.example.com:4317",
			protocol: ProtocolGRPC,
			traces:   "otel.example.com:4317",
			metrics:  "otel.example.com:4317",
			logs:     "otel.example.com:4317",
		},
		{
			name:     "http is plaintext OTLP/HTTP with signal paths",
			base:     "http://collector:4318",
			protocol: ProtocolHTTP,
			insecure: true,
			traces:   "http://collector:4318/v1/traces",
			metrics:  "http://collector:4318/v1/metrics",
			logs:     "http://collector:4318/v1/logs",
		},
		{
			name:     "https is TLS OTLP/HTTP with signal paths",
			base:     "https://otel.example.com",
			protocol: ProtocolHTTP,
			traces:   "https://otel.example.com/v1/traces",
			metrics:  "https://otel.example.com/v1/metrics",
			logs:     "https://otel.example.com/v1/logs",
		},
		{
			name:     "gRPC drops the path",
			base:     "grpc://collector:4317/ignored",
			protocol: ProtocolGRPC,
			insecure: true,
			traces:   "collector:4317",
			metrics:  "collector:4317",
			logs:     "collector:4317",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, map[string]string{
				EnvEnabled:  "true",
				EnvEndpoint: tt.base,
			})

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v, want nil", err)
			}

			want := map[string]struct {
				signal   SignalConfig
				endpoint string
			}{
				"traces":  {cfg.Traces, tt.traces},
				"metrics": {cfg.Metrics, tt.metrics},
				"logs":    {cfg.Logs, tt.logs},
			}
			for name, got := range want {
				if got.signal.Protocol != tt.protocol {
					t.Errorf("%s protocol = %q, want %q", name, got.signal.Protocol, tt.protocol)
				}
				if got.signal.Insecure != tt.insecure {
					t.Errorf("%s insecure = %v, want %v", name, got.signal.Insecure, tt.insecure)
				}
				if got.signal.Endpoint != got.endpoint {
					t.Errorf("%s endpoint = %q, want %q", name, got.signal.Endpoint, got.endpoint)
				}
			}
		})
	}
}

func TestConfigFromEnvBasePathAppending(t *testing.T) {
	tests := []struct {
		name string
		base string
	}{
		{name: "no trailing slash", base: "http://collector:4318"},
		{name: "trailing slash", base: "http://collector:4318/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, map[string]string{
				EnvEnabled:  "true",
				EnvEndpoint: tt.base,
			})

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v, want nil", err)
			}

			for name, got := range map[string]SignalConfig{
				"traces":  cfg.Traces,
				"metrics": cfg.Metrics,
				"logs":    cfg.Logs,
			} {
				want := "http://collector:4318/v1/" + name
				if got.Endpoint != want {
					t.Errorf("%s endpoint = %q, want %q", name, got.Endpoint, want)
				}
				if strings.Contains(got.Endpoint, "//v1/") {
					t.Errorf("%s endpoint = %q, has a doubled slash", name, got.Endpoint)
				}
			}
		})
	}
}

func TestConfigFromEnvPerSignalOverride(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		traces  SignalConfig
		metrics SignalConfig
		logs    SignalConfig
	}{
		{
			name: "override beats base",
			env: map[string]string{
				EnvEndpoint:       "http://collector:4318",
				EnvTracesEndpoint: "https://traces.example.com/custom/path",
			},
			traces:  SignalConfig{Protocol: ProtocolHTTP, Endpoint: "https://traces.example.com/custom/path"},
			metrics: SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://collector:4318/v1/metrics", Insecure: true},
			logs:    SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://collector:4318/v1/logs", Insecure: true},
		},
		{
			name: "mixed protocols: traces gRPC, metrics HTTP",
			env: map[string]string{
				EnvEndpoint:        "http://collector:4318",
				EnvTracesEndpoint:  "grpc://collector:4317",
				EnvMetricsEndpoint: "https://metrics.example.com/v1/metrics",
			},
			traces:  SignalConfig{Protocol: ProtocolGRPC, Endpoint: "collector:4317", Insecure: true},
			metrics: SignalConfig{Protocol: ProtocolHTTP, Endpoint: "https://metrics.example.com/v1/metrics"},
			logs:    SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://collector:4318/v1/logs", Insecure: true},
		},
		{
			name: "all three overrides, no base",
			env: map[string]string{
				EnvTracesEndpoint:  "grpcs://traces.example.com:4317",
				EnvMetricsEndpoint: "grpc://metrics.example.com:4317",
				EnvLogsEndpoint:    "http://logs.example.com:4318/v1/logs",
			},
			traces:  SignalConfig{Protocol: ProtocolGRPC, Endpoint: "traces.example.com:4317"},
			metrics: SignalConfig{Protocol: ProtocolGRPC, Endpoint: "metrics.example.com:4317", Insecure: true},
			logs:    SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://logs.example.com:4318/v1/logs", Insecure: true},
		},
		{
			name: "per-signal endpoint is used verbatim, trailing slash kept",
			env: map[string]string{
				EnvEndpoint:     "http://collector:4318",
				EnvLogsEndpoint: "http://logs.example.com:4318/",
			},
			traces:  SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://collector:4318/v1/traces", Insecure: true},
			metrics: SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://collector:4318/v1/metrics", Insecure: true},
			logs:    SignalConfig{Protocol: ProtocolHTTP, Endpoint: "http://logs.example.com:4318/", Insecure: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{EnvEnabled: "true"}
			for k, v := range tt.env {
				env[k] = v
			}
			setEnv(t, env)

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v, want nil", err)
			}
			if cfg.Traces != tt.traces {
				t.Errorf("traces = %+v, want %+v", cfg.Traces, tt.traces)
			}
			if cfg.Metrics != tt.metrics {
				t.Errorf("metrics = %+v, want %+v", cfg.Metrics, tt.metrics)
			}
			if cfg.Logs != tt.logs {
				t.Errorf("logs = %+v, want %+v", cfg.Logs, tt.logs)
			}
		})
	}
}

func TestConfigFromEnvErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{
			name:    "enabled with no endpoint at all",
			env:     map[string]string{},
			wantErr: []string{EnvEnabled, EnvEndpoint, EnvTracesEndpoint},
		},
		{
			name: "no base and one signal endpoint missing",
			env: map[string]string{
				EnvTracesEndpoint:  "http://collector:4318/v1/traces",
				EnvMetricsEndpoint: "http://collector:4318/v1/metrics",
			},
			wantErr: []string{EnvLogsEndpoint},
		},
		{
			name:    "unknown scheme on the base endpoint",
			env:     map[string]string{EnvEndpoint: "tcp://collector:4317"},
			wantErr: []string{EnvEndpoint, "unsupported scheme", acceptedSchemes},
		},
		{
			name:    "missing scheme on the base endpoint",
			env:     map[string]string{EnvEndpoint: "collector:4318"},
			wantErr: []string{EnvEndpoint, "unsupported scheme", acceptedSchemes},
		},
		{
			name: "unknown scheme on a per-signal endpoint",
			env: map[string]string{
				EnvEndpoint:        "http://collector:4318",
				EnvMetricsEndpoint: "ftp://collector:4318",
			},
			wantErr: []string{EnvMetricsEndpoint, "unsupported scheme"},
		},
		{
			name:    "unparseable base endpoint",
			env:     map[string]string{EnvEndpoint: "http://collector:4318/\x7f"},
			wantErr: []string{EnvEndpoint, "invalid URL"},
		},
		{
			name: "unparseable per-signal endpoint",
			env: map[string]string{
				EnvTracesEndpoint: "http://[::1",
			},
			wantErr: []string{EnvTracesEndpoint, "invalid URL"},
		},
		{
			name:    "scheme with no host",
			env:     map[string]string{EnvEndpoint: "grpc://"},
			wantErr: []string{EnvEndpoint, "missing host"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{EnvEnabled: "true"}
			for k, v := range tt.env {
				env[k] = v
			}
			setEnv(t, env)

			cfg, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv() = %+v, want error", cfg)
			}
			if cfg != (Config{}) {
				t.Errorf("ConfigFromEnv() = %+v on error, want zero Config", cfg)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
