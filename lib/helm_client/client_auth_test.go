package helm_client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Minimal index.yaml structure for testing
type testIndexFile struct {
	APIVersion string                      `json:"apiVersion"`
	Generated  time.Time                   `json:"generated"`
	Entries    map[string][]testChartEntry `json:"entries"`
}

type testChartEntry struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	URLs    []string `json:"urls"`
}

func createTestIndex(baseURL string) []byte {
	index := testIndexFile{
		APIVersion: "v1",
		Generated:  time.Now(),
		Entries: map[string][]testChartEntry{
			"test-chart": {
				{
					Name:    "test-chart",
					Version: "1.0.0",
					URLs:    []string{baseURL + "/charts/test-chart-1.0.0.tgz"},
				},
			},
		},
	}
	data, _ := json.Marshal(index)
	return data
}

func TestBasicAuthRequired(t *testing.T) {
	const (
		validUser = "testuser"
		validPass = "testpass"
	)

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != validUser || pass != validPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(createTestIndex(serverURL))
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()
	serverURL = server.URL

	t.Run("without auth fails", func(t *testing.T) {
		client, err := NewClient()
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		_, err = client.ListCharts(server.URL)
		if err == nil {
			t.Error("expected error when accessing protected repo without auth")
		}
	})

	t.Run("with wrong credentials fails", func(t *testing.T) {
		client, err := NewClient(WithBasicAuth("wronguser", "wrongpass"))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		_, err = client.ListCharts(server.URL)
		if err == nil {
			t.Error("expected error when accessing protected repo with wrong credentials")
		}
	})

	t.Run("with correct credentials succeeds", func(t *testing.T) {
		client, err := NewClient(WithBasicAuth(validUser, validPass))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		charts, err := client.ListCharts(server.URL)
		if err != nil {
			t.Fatalf("ListCharts() error = %v", err)
		}

		if len(charts) == 0 {
			t.Error("expected at least one chart")
		}

		found := false
		for _, c := range charts {
			if c == "test-chart" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected to find 'test-chart', got %v", charts)
		}
	})
}

func TestInsecureSkipTLSVerify(t *testing.T) {
	var serverURL string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(createTestIndex(serverURL))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	serverURL = server.URL

	t.Run("without skip verify fails", func(t *testing.T) {
		client, err := NewClient()
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		_, err = client.ListCharts(server.URL)
		if err == nil {
			t.Error("expected TLS verification error with self-signed cert")
		}
	})

	t.Run("with skip verify succeeds", func(t *testing.T) {
		client, err := NewClient(WithInsecureSkipTLSVerify(true))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		charts, err := client.ListCharts(server.URL)
		if err != nil {
			t.Fatalf("ListCharts() error = %v", err)
		}

		if len(charts) == 0 {
			t.Error("expected at least one chart")
		}
	})
}

func TestCredentialsFile(t *testing.T) {
	const (
		validUser = "fileuser"
		validPass = "filepass"
	)

	// Track if auth was received
	var authReceived bool
	var receivedUser, receivedPass string

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok {
			authReceived = true
			receivedUser = user
			receivedPass = pass
		}

		if !ok || user != validUser || pass != validPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(createTestIndex(serverURL))
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()
	serverURL = server.URL

	client, err := NewClient(WithBasicAuth(validUser, validPass))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	_, err = client.ListCharts(serverURL)
	if err != nil {
		t.Fatalf("ListCharts() error = %v", err)
	}

	if !authReceived {
		t.Error("expected auth credentials to be sent to server")
	}
	if receivedUser != validUser {
		t.Errorf("expected username %q, got %q", validUser, receivedUser)
	}
	if receivedPass != validPass {
		t.Errorf("expected password %q, got %q", validPass, receivedPass)
	}
}

func TestCombinedOptions(t *testing.T) {
	const (
		validUser = "admin"
		validPass = "secret"
	)

	var serverURL string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != validUser || pass != validPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(createTestIndex(serverURL))
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()
	serverURL = server.URL

	t.Run("with auth but no skip verify fails", func(t *testing.T) {
		client, err := NewClient(WithBasicAuth(validUser, validPass))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		_, err = client.ListCharts(server.URL)
		if err == nil {
			t.Error("expected TLS verification error")
		}
	})

	t.Run("with skip verify but no auth fails", func(t *testing.T) {
		client, err := NewClient(WithInsecureSkipTLSVerify(true))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		_, err = client.ListCharts(server.URL)
		if err == nil {
			t.Error("expected auth error")
		}
	})

	t.Run("with both auth and skip verify succeeds", func(t *testing.T) {
		client, err := NewClient(
			WithBasicAuth(validUser, validPass),
			WithInsecureSkipTLSVerify(true),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		charts, err := client.ListCharts(server.URL)
		if err != nil {
			t.Fatalf("ListCharts() error = %v", err)
		}

		if len(charts) == 0 {
			t.Error("expected at least one chart")
		}
	})
}
