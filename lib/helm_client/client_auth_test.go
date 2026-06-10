package helm_client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func writeDockerConfig(t *testing.T, hosts ...string) string {
	t.Helper()

	auths := make(map[string]map[string]string, len(hosts))
	for _, h := range hosts {
		auths[h] = map[string]string{"auth": "dXNlcjpwYXNz"} // user:pass
	}
	return writeDockerConfigRaw(t, map[string]any{"auths": auths})
}

func writeDockerConfigRaw(t *testing.T, cfg map[string]any) string {
	t.Helper()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal docker config: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	return path
}

func TestOCIRegistryRouting(t *testing.T) {
	t.Run("resolvable host uses creds client, others fall back to basic auth", func(t *testing.T) {
		// Docker Hub is stored under its canonical key, as `docker login` writes it.
		cfgPath := writeDockerConfig(t, "registry.example.com", "https://index.docker.io/v1/")

		client, err := NewClient(
			WithBasicAuth("user", "pass"),
			WithCredentialsFile(cfgPath),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		if client.registryClientCreds == nil {
			t.Fatal("expected a dedicated credentials-file registry client when both auth methods are set")
		}
		if client.registryClient == nil {
			t.Fatal("expected a basic-auth registry client")
		}

		cases := []struct {
			repoURL  string
			wantCred bool
		}{
			{"oci://registry.example.com/org/chart", true},
			{"oci://docker.io/library/mysql", true},            // canonical Docker Hub key resolves
			{"oci://registry-1.docker.io/library/redis", true}, // maps to the same canonical key
			{"oci://ghcr.io/org/chart", false},                 // not in creds file -> basic auth
		}
		for _, tc := range cases {
			got := client.registryClientFor(tc.repoURL)
			if tc.wantCred && got != client.registryClientCreds {
				t.Errorf("%s: expected credentials-file client", tc.repoURL)
			}
			if !tc.wantCred && got != client.registryClient {
				t.Errorf("%s: expected basic-auth fallback client", tc.repoURL)
			}
		}
	})

	t.Run("non-canonical docker.io key does not falsely route to creds client", func(t *testing.T) {
		// A bare "docker.io" auths key is not the key ORAS resolves for Docker
		// Hub (it looks up https://index.docker.io/v1/), so it must not suppress
		// the basic-auth fallback. Regression guard for issue #135 finding 2.
		cfgPath := writeDockerConfig(t, "docker.io")

		client, err := NewClient(
			WithBasicAuth("user", "pass"),
			WithCredentialsFile(cfgPath),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		if got := client.registryClientFor("oci://docker.io/library/mysql"); got != client.registryClient {
			t.Error("expected basic-auth fallback for a non-resolvable bare docker.io key")
		}
	})

	t.Run("credsStore config falls back gracefully when helper is unavailable", func(t *testing.T) {
		// A credsStore config can only be resolved by invoking its helper binary.
		// When that binary is absent, routing must fall back to basic auth rather
		// than error. Regression guard for issue #135 finding 1.
		cfgPath := writeDockerConfigRaw(t, map[string]any{"credsStore": "mcphelmtestnohelper"})

		client, err := NewClient(
			WithBasicAuth("user", "pass"),
			WithCredentialsFile(cfgPath),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		if got := client.registryClientFor("oci://registry.invalid.test/org/chart"); got != client.registryClient {
			t.Error("expected basic-auth fallback when the credential helper is unavailable")
		}
	})

	t.Run("only basic auth: single client for all hosts", func(t *testing.T) {
		client, err := NewClient(WithBasicAuth("user", "pass"))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if client.registryClientCreds != nil {
			t.Error("did not expect a separate credentials-file client without -registry-credentials")
		}
		if client.registryClientFor("oci://any.example.com/org/chart") != client.registryClient {
			t.Error("expected the single registry client for all hosts")
		}
	})

	t.Run("only creds file: single client for all hosts", func(t *testing.T) {
		cfgPath := writeDockerConfig(t, "registry.example.com")

		client, err := NewClient(WithCredentialsFile(cfgPath))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if client.registryClientCreds != nil {
			t.Error("did not expect routing without basic auth")
		}
		if client.registryClientFor("oci://registry.example.com/org/chart") != client.registryClient {
			t.Error("expected the single registry client for all hosts")
		}
	})
}

func TestOCIRegistryHost(t *testing.T) {
	cases := map[string]string{
		"oci://registry.example.com/org/chart":      "registry.example.com",
		"oci://registry.example.com:5000/org/chart": "registry.example.com:5000",
		"oci://docker.io/library/mysql":             "docker.io",
		"oci://ghcr.io/org/chart":                   "ghcr.io",
	}
	for in, want := range cases {
		if got := ociRegistryHost(in); got != want {
			t.Errorf("ociRegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExternalCredHelpers(t *testing.T) {
	cfgPath := writeDockerConfigRaw(t, map[string]any{
		"credsStore":  "osxkeychain",
		"credHelpers": map[string]string{"gcr.io": "gcloud", "public.ecr.aws": "ecr-login"},
	})

	store, helpers := externalCredHelpers(cfgPath)
	if store != "osxkeychain" {
		t.Errorf("credsStore = %q, want osxkeychain", store)
	}
	want := []string{"gcr.io", "public.ecr.aws"}
	if len(helpers) != len(want) || helpers[0] != want[0] || helpers[1] != want[1] {
		t.Errorf("credHelpers = %v, want %v", helpers, want)
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
