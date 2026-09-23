package helm_client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPDownloadCustomTLS(t *testing.T) {
	archive := testChartArchive(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate missing", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/index.yaml" {
			_, _ = w.Write(createTestIndex(""))
			return
		}
		_, _ = w.Write(archive)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	cert := server.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(WithCAFile(certFile), WithTLSClientConfig(certFile, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.GetChartValues(context.Background(), server.URL, "test-chart", "1.0.0")
	if err != nil || values != "replicas: 1\n" {
		t.Fatalf("custom CA and mTLS: values=%q err=%v", values, err)
	}
}

func TestHTTPDownloadExactLimits(t *testing.T) {
	archive := testChartArchive(t)
	index := createTestIndex("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		if r.URL.Path == "/index.yaml" {
			_, _ = w.Write(index)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	client := newTestClient(t)
	client.options.indexMaxBytes = int64(len(index))
	client.options.chartMaxBytes = int64(len(archive))
	values, err := client.GetChartValues(context.Background(), server.URL, "test-chart", "1.0.0")
	if err != nil || values != "replicas: 1\n" {
		t.Fatalf("exact byte limits: values=%q err=%v", values, err)
	}
}
