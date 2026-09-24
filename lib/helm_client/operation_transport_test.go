package helm_client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type operationRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f operationRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOperationTransportRejectsCanceledContexts(t *testing.T) {
	for _, source := range []string{"request", "operation"} {
		t.Run(source, func(t *testing.T) {
			operation, cancelOperation := context.WithCancel(t.Context())
			defer cancelOperation()
			request, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			if source == "request" {
				cancelRequest()
			} else {
				cancelOperation()
			}
			transport := operationTransport{ctx: operation, base: operationRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				t.Error("canceled request reached the underlying transport")
				return nil, context.Canceled
			})}
			req, err := http.NewRequestWithContext(request, http.MethodGet, "https://example.invalid", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transport.RoundTrip(req); !errors.Is(err, context.Canceled) {
				t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestOperationTransportCancelsHeadersAndBody(t *testing.T) {
	for _, source := range []string{"request", "operation"} {
		for _, phase := range []string{"headers", "body"} {
			t.Run(source+"/"+phase, func(t *testing.T) {
				started := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if phase == "body" {
						_, _ = w.Write([]byte("prefix"))
						w.(http.Flusher).Flush()
					}
					close(started)
					<-r.Context().Done()
				}))
				defer server.Close()
				operation, cancelOperation := context.WithCancel(t.Context())
				defer cancelOperation()
				request, cancelRequest := context.WithCancel(t.Context())
				defer cancelRequest()
				base := &http.Transport{}
				defer base.CloseIdleConnections()
				client := &http.Client{Transport: operationTransport{ctx: operation, base: base}}
				req, err := http.NewRequestWithContext(request, http.MethodGet, server.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				bodyReady := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					resp, err := client.Do(req)
					close(bodyReady)
					if err == nil {
						_, err = io.ReadAll(resp.Body)
						err = errors.Join(err, resp.Body.Close())
					}
					done <- err
				}()
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("request did not reach the server")
				}
				if phase == "body" {
					select {
					case <-bodyReady:
					case <-time.After(3 * time.Second):
						t.Fatal("response headers did not arrive")
					}
				}
				if source == "request" {
					cancelRequest()
				} else {
					cancelOperation()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("download error = %v, want context.Canceled", err)
					}
				case <-time.After(time.Second):
					t.Fatal("download ignored cancellation")
				}
			})
		}
	}
}

func TestOperationTransportContextLifetime(t *testing.T) {
	type key string
	for _, finish := range []string{"error", "eof", "close"} {
		t.Run(finish, func(t *testing.T) {
			operation := context.WithValue(t.Context(), key("operation"), "operation-value")
			request := context.WithValue(t.Context(), key("scope"), "request-scope")
			deadline := time.Now().Add(time.Minute)
			request, cancel := context.WithDeadline(request, deadline)
			defer cancel()
			var captured context.Context
			failure := errors.New("transport failure")
			transport := operationTransport{ctx: operation, base: operationRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
				captured = r.Context()
				if finish == "error" {
					return nil, failure
				}
				return &http.Response{Body: io.NopCloser(strings.NewReader("payload"))}, nil
			})}
			req, err := http.NewRequestWithContext(request, http.MethodGet, "https://example.invalid", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := transport.RoundTrip(req)
			if captured == nil {
				t.Fatal("underlying transport was not called")
			}
			if got, ok := captured.Deadline(); !ok || !got.Equal(deadline) {
				t.Errorf("deadline = %v, %v; want request deadline %v", got, ok, deadline)
			}
			if captured.Value(key("scope")) != "request-scope" || captured.Value(key("operation")) != "operation-value" {
				t.Error("merged context lost request or operation values")
			}
			if finish == "error" {
				if !errors.Is(err, failure) {
					t.Fatalf("RoundTrip error = %v, want transport failure", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if captured.Err() != nil {
					t.Fatal("request canceled before the response body was consumed")
				}
				if finish == "eof" {
					data, err := io.ReadAll(resp.Body)
					if err != nil || string(data) != "payload" {
						t.Fatalf("body = %q, error = %v", data, err)
					}
					if captured.Err() == nil {
						t.Error("request context not released at EOF")
					}
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if captured.Err() == nil {
				t.Error("request context not released after transport completion")
			}
			if request.Err() != nil || operation.Err() != nil {
				t.Error("transport canceled a parent context")
			}
		})
	}
}
