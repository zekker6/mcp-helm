package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type logRequest struct {
	request    *collogspb.ExportLogsServiceRequest
	header     metadata.MD
	clientCert bool
}

type logReceiver struct {
	collogspb.UnimplementedLogsServiceServer
	requests chan logRequest
}

func (r *logReceiver) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	header, _ := metadata.FromIncomingContext(ctx)
	p, _ := peer.FromContext(ctx)
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	r.requests <- logRequest{request: req, header: header, clientCert: ok && len(info.State.PeerCertificates) > 0}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func newLogReceiver(t *testing.T, secure, requireClient bool) (string, *logReceiver) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	receiver := &logReceiver{requests: make(chan logRequest, 1)}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "")
	var opts []grpc.ServerOption
	if secure {
		fixture := httptest.NewTLSServer(nil)
		defer fixture.Close()
		cert := fixture.TLS.Certificates[0]
		path := filepath.Join(t.TempDir(), "collector.pem")
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", path)
		serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if requireClient {
			serverTLS.ClientAuth = tls.RequireAnyClientCert
			key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(t.TempDir(), "client.key")
			if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE", path)
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY", keyPath)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	srv := grpc.NewServer(opts...)
	collogspb.RegisterLogsServiceServer(srv, receiver)
	t.Cleanup(srv.Stop)
	go func() {
		if err := srv.Serve(listener); err != nil {
			t.Errorf("serve gRPC: %v", err)
		}
	}()
	return listener.Addr().String(), receiver
}

func TestGRPCLogTransportHonorsResolvedSecurity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		scheme        string
		secure        bool
		requireClient bool
		override      bool
		logsCA        bool
		wantExport    bool
	}{
		{name: "base grpcs to TLS", scheme: "grpcs://", secure: true, wantExport: true},
		{name: "logs grpcs override to TLS", scheme: "grpcs://", secure: true, override: true, wantExport: true},
		{name: "logs grpcs with signal CA", scheme: "grpcs://", secure: true, logsCA: true, wantExport: true},
		{name: "logs grpcs with mTLS", scheme: "grpcs://", secure: true, requireClient: true, wantExport: true},
		{name: "base grpcs refuses plaintext", scheme: "grpcs://"},
		{name: "logs grpcs override refuses plaintext", scheme: "grpcs://", override: true},
		{name: "grpc still allows plaintext", scheme: "grpc://", wantExport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, receiver := newLogReceiver(t, tc.secure, tc.requireClient)
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE", "")
			if tc.logsCA {
				t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE", os.Getenv("OTEL_EXPORTER_OTLP_CERTIFICATE"))
				t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "")
			}
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20test-token")
			if tc.override {
				t.Setenv(EnvEndpoint, "http://127.0.0.1:1")
				t.Setenv(EnvLogsEndpoint, tc.scheme+endpoint)
			} else {
				t.Setenv(EnvEndpoint, tc.scheme+endpoint)
				t.Setenv(EnvLogsEndpoint, "")
			}
			t.Setenv(EnvEnabled, "true")
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			exporter, err := newLogExporter(ctx, cfg.Logs)
			if err != nil {
				t.Fatal(err)
			}
			provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
			defer func() {
				if err := provider.Shutdown(ctx); err != nil && tc.wantExport {
					t.Errorf("shutdown log provider: %v", err)
				}
			}()
			var record otellog.Record
			record.SetBody(attribute.StringValue("tls-transport-test"))
			provider.Logger("test").Emit(ctx, record)
			if tc.wantExport {
				select {
				case got := <-receiver.requests:
					if len(got.request.GetResourceLogs()) == 0 || len(got.header.Get("authorization")) == 0 {
						t.Errorf("missing log or authorization header: %v", got)
					}
					if tc.requireClient && !got.clientCert {
						t.Error("mTLS receiver did not receive a client certificate")
					}
				case <-ctx.Done():
					t.Fatal("no log export received")
				}
			} else {
				select {
				case got := <-receiver.requests:
					t.Fatalf("plaintext receiver got credentials and logs: %v", got)
				default:
				}
			}
		})
	}
}
