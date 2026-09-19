package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const envMetricTemporality = "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE"

type metricReceiver struct {
	colmetricspb.UnimplementedMetricsServiceServer
	requests chan *colmetricspb.ExportMetricsServiceRequest
}

func (r *metricReceiver) Export(_ context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	r.requests <- req
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

func newMetricReceiver(t *testing.T, protocol Protocol) (SignalConfig, *metricReceiver) {
	t.Helper()
	receiver := &metricReceiver{requests: make(chan *colmetricspb.ExportMetricsServiceRequest, 8)}
	if protocol == ProtocolGRPC {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		colmetricspb.RegisterMetricsServiceServer(srv, receiver)
		t.Cleanup(srv.Stop)
		go func() {
			if err := srv.Serve(listener); err != nil {
				t.Errorf("serve gRPC: %v", err)
			}
		}()
		return SignalConfig{Protocol: protocol, Endpoint: listener.Addr().String(), Insecure: true}, receiver
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read export: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Errorf("decode export: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		receiver.requests <- &req
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return SignalConfig{Protocol: protocol, Endpoint: srv.URL + "/v1/metrics", Insecure: true}, receiver
}

func TestMetricExportAggregationAndTemporality(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolHTTP, ProtocolGRPC} {
		for _, tc := range []struct {
			name        string
			aggregation string
			temporality string
			exponential bool
			delta       bool
		}{
			{name: "defaults", exponential: true},
			{name: "explicit cumulative", aggregation: "explicit_bucket_histogram", temporality: "cumulative"},
			{name: "default aggregation delta", temporality: "delta", exponential: true, delta: true},
			{name: "explicit delta", aggregation: "explicit_bucket_histogram", temporality: "delta", delta: true},
			{name: "exponential lowmemory", aggregation: "base2_exponential_bucket_histogram", temporality: "lowmemory", exponential: true, delta: true},
		} {
			t.Run(string(protocol)+"/"+tc.name, func(t *testing.T) {
				t.Setenv(envHistogramAggregation, tc.aggregation)
				t.Setenv(envMetricTemporality, tc.temporality)
				t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")
				cfg, receiver := newMetricReceiver(t, protocol)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				exporter, err := newMetricExporter(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Hour))
				provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := provider.Shutdown(ctx); err != nil {
						t.Errorf("shutdown: %v", err)
					}
				})
				meter := provider.Meter("test")
				histogram, err := meter.Float64Histogram(operationDurationInstrument,
					metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(DurationBucketBoundaries...))
				if err != nil {
					t.Fatal(err)
				}
				counter, err := meter.Int64Counter("mcp_helm.test.calls")
				if err != nil {
					t.Fatal(err)
				}
				for round := range 2 {
					histogram.Record(ctx, float64(round+1))
					counter.Add(ctx, 1)
					if err := provider.ForceFlush(ctx); err != nil {
						t.Fatalf("flush: %v", err)
					}
					var req *colmetricspb.ExportMetricsServiceRequest
					select {
					case req = <-receiver.requests:
					case <-ctx.Done():
						t.Fatal("no metric export received")
					}
					metrics := make(map[string]*metricspb.Metric)
					for _, resource := range req.GetResourceMetrics() {
						for _, scope := range resource.GetScopeMetrics() {
							for _, m := range scope.GetMetrics() {
								metrics[m.GetName()] = m
							}
						}
					}
					wantTemporality := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
					wantCount, wantSum := uint64(round+1), float64(1+2*round)
					if tc.delta {
						wantTemporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
						wantCount, wantSum = 1, float64(round+1)
					}
					m := metrics[operationDurationInstrument]
					if m.GetUnit() != "s" {
						t.Errorf("histogram unit = %q, want s", m.GetUnit())
					}
					var count uint64
					var sum float64
					var temporality metricspb.AggregationTemporality
					if tc.exponential {
						h := m.GetExponentialHistogram()
						if h == nil || len(h.GetDataPoints()) != 1 {
							t.Fatalf("expected exponential histogram, got %v", m)
						}
						count, sum, temporality = h.DataPoints[0].GetCount(), h.DataPoints[0].GetSum(), h.GetAggregationTemporality()
					} else {
						h := m.GetHistogram()
						if h == nil || len(h.GetDataPoints()) != 1 {
							t.Fatalf("expected explicit histogram, got %v", m)
						}
						count, sum, temporality = h.DataPoints[0].GetCount(), h.DataPoints[0].GetSum(), h.GetAggregationTemporality()
					}
					if count != wantCount || sum != wantSum || temporality != wantTemporality {
						t.Errorf("round %d: histogram = (%d, %v, %v), want (%d, %v, %v)", round, count, sum, temporality, wantCount, wantSum, wantTemporality)
					}
					s := metrics["mcp_helm.test.calls"].GetSum()
					if s == nil || len(s.GetDataPoints()) != 1 || !s.GetIsMonotonic() {
						t.Fatalf("expected monotonic counter sum, got %v", s)
					}
					if s.GetAggregationTemporality() != wantTemporality || s.DataPoints[0].GetAsInt() != int64(wantCount) {
						t.Errorf("counter = %v, want count %d with %v", s, wantCount, wantTemporality)
					}
				}
			})
		}
	}
}
