package helm_client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPArchiveCredentialsStayOnRepositoryOrigin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		redirect   bool
		sameOrigin bool
		crossHost  bool
		repoTLS    bool
		forwardAll bool
		wantAuth   bool
	}{
		{name: "same origin archive", sameOrigin: true, wantAuth: true},
		{name: "same origin HTTPS archive", sameOrigin: true, repoTLS: true, wantAuth: true},
		{name: "same origin redirect", sameOrigin: true, redirect: true, wantAuth: true},
		{name: "absolute cross port"},
		{name: "absolute cross host", crossHost: true},
		{name: "HTTPS to HTTP archive", repoTLS: true},
		{name: "redirect cross port", redirect: true},
		{name: "redirect cross host", redirect: true, crossHost: true},
		{name: "HTTPS to HTTP redirect", redirect: true, repoTLS: true},
		{name: "explicit cross port archive", forwardAll: true, wantAuth: true},
		{name: "explicit cross port forwarding", redirect: true, forwardAll: true, wantAuth: true},
		{name: "explicit HTTPS to HTTP forwarding", redirect: true, repoTLS: true, forwardAll: true, wantAuth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := testChartArchive(t)
			authAtArchive := make(chan bool, 1)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _, hasAuth := r.BasicAuth()
				authAtArchive <- hasAuth
				_, _ = w.Write(archive)
			}))
			defer remote.Close()

			var repoURL string
			target := remote.URL
			if tc.crossHost {
				target = strings.Replace(target, "127.0.0.1", "localhost", 1)
			}
			repo := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "user" || password != "secret" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/index.yaml":
					indexURL := target
					if tc.redirect || tc.sameOrigin {
						indexURL = repoURL
					}
					_, _ = w.Write(createTestIndex(indexURL))
				case "/charts/test-chart-1.0.0.tgz":
					if tc.redirect {
						http.Redirect(w, r, target+"/final.tgz", http.StatusFound)
					} else {
						authAtArchive <- true
						_, _ = w.Write(archive)
					}
				case "/final.tgz":
					authAtArchive <- true
					_, _ = w.Write(archive)
				default:
					http.NotFound(w, r)
				}
			}))
			if tc.repoTLS {
				repo.StartTLS()
			} else {
				repo.Start()
			}
			defer repo.Close()
			repoURL = repo.URL
			if tc.sameOrigin {
				target = repoURL
			}

			opts := []ClientOption{WithBasicAuth("user", "secret")}
			if tc.repoTLS {
				opts = append(opts, WithInsecureSkipTLSVerify(true))
			}
			if tc.forwardAll {
				opts = append(opts, WithPassCredentialsAll(true))
			}
			client, err := NewClient(opts...)
			if err != nil {
				t.Fatal(err)
			}
			values, err := client.GetChartValues(context.Background(), repo.URL, "test-chart", "1.0.0")
			if err != nil || values != "replicas: 1\n" {
				t.Fatalf("archive values = %q, error = %v", values, err)
			}
			select {
			case got := <-authAtArchive:
				if got != tc.wantAuth {
					t.Errorf("archive received credentials = %t, want %t", got, tc.wantAuth)
				}
			default:
				t.Fatal("archive was not requested")
			}
		})
	}
}

func TestHTTPGetterStopsAfterTenRedirects(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	defer server.Close()

	client, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	getter, err := client.newHTTPGetter(context.Background(), server.URL, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getter.Get(server.URL); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("redirect loop error = %v", err)
	}
	if got := count.Load(); got != 10 {
		t.Errorf("made %d requests before redirect limit, want 10", got)
	}
}
