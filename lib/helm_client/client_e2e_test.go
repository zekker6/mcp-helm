package helm_client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/registry"
)

// End-to-end auth matrix: every combination of repository type (HTTP / OCI) and
// access (anonymous / basic-auth-required) is exercised against a real
// in-process server, through the same code paths the MCP tools use.
//
// Each cell drives two distinct code paths:
//   - the index/tags path  (ListChartVersions)
//   - the chart-binary path (GetChartValues -> loadChart* -> download/pull)
//
// The HTTP + auth-required cell is the regression guard for the report in
// report.md: the index download is authenticated (getRepo forwards credentials
// onto the repo.Entry) but the subsequent .tgz download in loadChartFromHTTP
// was sent anonymously, so a private repo returns 404/401 only on the binary
// fetch while listing versions still works.

const (
	matrixUser    = "matrix-user"
	matrixPass    = "matrix-pass"
	matrixChart   = "test-chart"
	matrixVersion = "1.0.0"
	matrixMarker  = "hello-from-matrix"
)

// buildMatrixChartTGZ writes a minimal chart to disk and packages it with
// Helm's own loader/saver, producing a real chart archive usable both for HTTP
// serving and OCI seeding (so the parsing on the read side is genuine).
func buildMatrixChartTGZ(t *testing.T) []byte {
	t.Helper()

	chartDir := filepath.Join(t.TempDir(), matrixChart)
	if err := os.MkdirAll(filepath.Join(chartDir, "templates"), 0o755); err != nil {
		t.Fatalf("mkdir chart dir: %v", err)
	}

	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: " + matrixChart + "\nversion: " + matrixVersion +
			"\nappVersion: \"1.0\"\ndescription: matrix test chart\n",
		"values.yaml": "replicaCount: 1\nmessage: " + matrixMarker + "\n",
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
			"  name: {{ .Release.Name }}-cm\ndata:\n  message: {{ .Values.message | quote }}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(chartDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	loaded, err := loader.LoadDir(chartDir)
	if err != nil {
		t.Fatalf("load chart dir: %v", err)
	}
	chart, ok := loaded.(*chartv2.Chart)
	if !ok {
		t.Fatalf("expected *chartv2.Chart, got %T", loaded)
	}

	tgzPath, err := chartutil.Save(chart, t.TempDir())
	if err != nil {
		t.Fatalf("save chart: %v", err)
	}
	data, err := os.ReadFile(tgzPath)
	if err != nil {
		t.Fatalf("read chart tgz: %v", err)
	}
	return data
}

// httpRepoRecorder captures whether the chart .tgz was fetched and whether that
// fetch carried valid credentials. The credential flag is the smoking gun for
// the reported bug: with the bug present the .tgz arrives unauthenticated.
type httpRepoRecorder struct {
	mu        sync.Mutex
	tgzHit    bool
	tgzAuthed bool
}

// startHTTPChartRepo serves index.yaml and the chart .tgz, optionally behind
// HTTP basic auth, and records how the .tgz was requested.
func startHTTPChartRepo(t *testing.T, requireAuth bool, tgz []byte) (string, *httpRepoRecorder) {
	t.Helper()

	rec := &httpRepoRecorder{}
	tgzPath := "/charts/" + matrixChart + "-" + matrixVersion + ".tgz"

	var serverURL string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		authed := ok && u == matrixUser && p == matrixPass

		if requireAuth && !authed {
			w.Header().Set("WWW-Authenticate", `Basic realm="charts"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/index.yaml":
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(createTestIndex(serverURL))
		case tgzPath:
			rec.mu.Lock()
			rec.tgzHit = true
			rec.tgzAuthed = authed
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tgz)
		default:
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	serverURL = server.URL
	return server.URL, rec
}

// ociArtifact is a single Helm chart packaged as an OCI artifact (config +
// chart layer + manifest), addressable by digest and by tag. It is everything a
// minimal in-process registry needs to answer a chart pull and a tag list.
type ociArtifact struct {
	repoName     string // path component of the reference, e.g. "charts/test-chart"
	tag          string
	manifestDgst digest.Digest
	blobs        map[digest.Digest][]byte
}

// buildOCIArtifact assembles the OCI blobs for the chart using Helm's chart
// media types, mirroring what `helm push` would store, without pulling in a
// full registry server implementation.
func buildOCIArtifact(t *testing.T, repoName, tag string, tgz []byte) *ociArtifact {
	t.Helper()

	art := &ociArtifact{repoName: repoName, tag: tag, blobs: map[digest.Digest][]byte{}}
	add := func(mediaType string, data []byte) ocispec.Descriptor {
		desc := content.NewDescriptorFromBytes(mediaType, data)
		art.blobs[desc.Digest] = data
		return desc
	}

	configDesc := add(registry.ConfigMediaType,
		[]byte(`{"name":"`+matrixChart+`","version":"`+matrixVersion+`"}`))
	layerDesc := add(registry.ChartLayerMediaType, tgz)

	manifestBytes, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		t.Fatalf("marshal OCI manifest: %v", err)
	}
	art.manifestDgst = add(ocispec.MediaTypeImageManifest, manifestBytes).Digest
	return art
}

// ServeHTTP implements the read subset of the OCI distribution spec that Helm's
// registry client exercises for Tags() and Pull(): the API-version probe, the
// tag list, manifest fetch by tag or digest, and blob fetch by digest.
func (a *ociArtifact) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case p == "/v2/" || p == "/v2":
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)

	case p == "/v2/"+a.repoName+"/tags/list":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": a.repoName, "tags": []string{a.tag}})

	case strings.HasPrefix(p, "/v2/"+a.repoName+"/manifests/"):
		ref := strings.TrimPrefix(p, "/v2/"+a.repoName+"/manifests/")
		dgst := a.manifestDgst
		if ref != a.tag && ref != a.manifestDgst.String() {
			http.Error(w, "manifest unknown", http.StatusNotFound)
			return
		}
		a.writeBlob(w, r, dgst, ocispec.MediaTypeImageManifest)

	case strings.HasPrefix(p, "/v2/"+a.repoName+"/blobs/"):
		ref := strings.TrimPrefix(p, "/v2/"+a.repoName+"/blobs/")
		a.writeBlob(w, r, digest.Digest(ref), "application/octet-stream")

	default:
		http.NotFound(w, r)
	}
}

func (a *ociArtifact) writeBlob(w http.ResponseWriter, r *http.Request, dgst digest.Digest, mediaType string) {
	data, ok := a.blobs[dgst]
	if !ok {
		http.Error(w, "blob unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}

// startOCIRegistry stands up an in-process OCI registry serving a single chart
// and returns its host:port. A non-empty user enables basic auth requiring
// exactly that user/pass (and rejecting anything else).
func startOCIRegistry(t *testing.T, user, pass string, tgz []byte) string {
	t.Helper()

	var handler http.Handler = buildOCIArtifact(t, "charts/"+matrixChart, matrixVersion, tgz)
	if user != "" {
		handler = requireBasicAuth(user, pass, handler)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// requireBasicAuth gates a handler behind static basic auth, issuing a Basic
// challenge so the ORAS-based registry client retries with credentials.
func requireBasicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestRepositoryAuthMatrix(t *testing.T) {
	tgz := buildMatrixChartTGZ(t)

	cases := []struct {
		name string
		oci  bool
		auth bool
	}{
		{name: "HTTP_Anonymous", oci: false, auth: false},
		{name: "HTTP_AuthRequired", oci: false, auth: true},
		{name: "OCI_Anonymous", oci: true, auth: false},
		{name: "OCI_AuthRequired", oci: true, auth: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				repoURL    string
				chartName  string
				clientOpts []ClientOption
				httpRec    *httpRepoRecorder
			)

			if tc.oci {
				user, pass := "", ""
				if tc.auth {
					user, pass = matrixUser, matrixPass
				}
				host := startOCIRegistry(t, user, pass, tgz)
				repoURL = "oci://" + host + "/charts/" + matrixChart
				chartName = "" // chart name is extracted from the OCI URL
				clientOpts = append(clientOpts, WithPlainHTTP(true))
			} else {
				url, rec := startHTTPChartRepo(t, tc.auth, tgz)
				repoURL, httpRec = url, rec
				chartName = matrixChart
			}
			if tc.auth {
				clientOpts = append(clientOpts, WithBasicAuth(matrixUser, matrixPass))
			}

			client, err := NewClient(clientOpts...)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			// 1) Index / tags path.
			versions, err := client.ListChartVersions(repoURL, chartName)
			if err != nil {
				t.Fatalf("ListChartVersions() error = %v", err)
			}
			if !slices.Contains(versions, matrixVersion) {
				t.Fatalf("expected version %q in %v", matrixVersion, versions)
			}

			// 2) Chart-binary download / OCI pull path. This is where the
			//    reported bug bites for HTTP + auth: the index succeeded above,
			//    but the .tgz fetch is performed without credentials.
			values, err := client.GetChartValues(repoURL, chartName, matrixVersion)
			if err != nil {
				t.Fatalf("GetChartValues() error = %v", err)
			}
			if !strings.Contains(values, matrixMarker) {
				t.Errorf("expected chart values to contain %q, got: %q", matrixMarker, values)
			}

			// For the HTTP + auth cell, assert the .tgz fetch actually carried
			// credentials. This pins the exact regression from report.md.
			if !tc.oci && tc.auth {
				httpRec.mu.Lock()
				defer httpRec.mu.Unlock()
				if !httpRec.tgzHit {
					t.Fatal("chart .tgz was never requested")
				}
				if !httpRec.tgzAuthed {
					t.Error("chart .tgz download was sent without credentials (loadChartFromHTTP dropped auth)")
				}
			}
		})
	}
}

// TestCombinedCredentialsRouting exercises the combined auth mode end-to-end:
// both static basic auth (-username/-password-file) and a Docker credentials
// file (-registry-credentials) are configured at once. OCI requests must route
// per host: a registry the credentials file resolves a credential for uses that
// identity, and every other registry falls back to the static basic auth.
//
// Unlike TestOCIRegistryRouting (which only asserts which client object is
// selected), this drives real authenticated pulls against two registries that
// each accept ONLY their expected identity, so a mis-route fails the auth.
func TestCombinedCredentialsRouting(t *testing.T) {
	tgz := buildMatrixChartTGZ(t)

	// Identity stored in the Docker credentials file. writeDockerConfig encodes
	// the fixed pair "user:pass", so the covered registry must accept exactly
	// that and reject the basic-auth identity.
	const credsFileUser, credsFilePass = "user", "pass"
	// Static basic-auth identity, used as the fallback for hosts the
	// credentials file does not resolve.
	const fallbackUser, fallbackPass = "fallback-user", "fallback-pass"

	// Registry the credentials file covers: accepts only the creds-file identity.
	coveredHost := startOCIRegistry(t, credsFileUser, credsFilePass, tgz)
	// Registry the credentials file does NOT list: reachable only via the
	// basic-auth fallback, so it accepts only the basic-auth identity.
	fallbackHost := startOCIRegistry(t, fallbackUser, fallbackPass, tgz)

	// Credentials file resolves a credential for the covered host only.
	cfgPath := writeDockerConfig(t, coveredHost)

	client, err := NewClient(
		WithBasicAuth(fallbackUser, fallbackPass),
		WithCredentialsFile(cfgPath),
		WithPlainHTTP(true),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.registryClientCreds == nil {
		t.Fatal("expected a dedicated credentials-file registry client when both auth methods are set")
	}

	// Covered host: must authenticate with the credentials-file identity.
	// A mis-route to basic auth would send fallback-user/pass and get rejected.
	t.Run("covered host uses credentials file", func(t *testing.T) {
		repoURL := "oci://" + coveredHost + "/charts/" + matrixChart

		versions, err := client.ListChartVersions(repoURL, "")
		if err != nil {
			t.Fatalf("ListChartVersions() error = %v (credentials-file identity should have been used)", err)
		}
		if !slices.Contains(versions, matrixVersion) {
			t.Fatalf("expected version %q in %v", matrixVersion, versions)
		}

		values, err := client.GetChartValues(repoURL, "", matrixVersion)
		if err != nil {
			t.Fatalf("GetChartValues() error = %v", err)
		}
		if !strings.Contains(values, matrixMarker) {
			t.Errorf("expected chart values to contain %q, got: %q", matrixMarker, values)
		}
	})

	// Uncovered host: must fall back to the static basic-auth identity.
	// A mis-route to the credentials-file client would send no credential and
	// get rejected.
	t.Run("uncovered host falls back to basic auth", func(t *testing.T) {
		repoURL := "oci://" + fallbackHost + "/charts/" + matrixChart

		versions, err := client.ListChartVersions(repoURL, "")
		if err != nil {
			t.Fatalf("ListChartVersions() error = %v (basic-auth fallback should have been used)", err)
		}
		if !slices.Contains(versions, matrixVersion) {
			t.Fatalf("expected version %q in %v", matrixVersion, versions)
		}

		values, err := client.GetChartValues(repoURL, "", matrixVersion)
		if err != nil {
			t.Fatalf("GetChartValues() error = %v", err)
		}
		if !strings.Contains(values, matrixMarker) {
			t.Errorf("expected chart values to contain %q, got: %q", matrixMarker, values)
		}
	})
}
