package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

type httpLimitTestCase struct {
	name  string
	setup func(*http.Server, *server.MCPServer, bool) (postPath, streamPath string)
}

func httpLimitTestCases() []httpLimitTestCase {
	return []httpLimitTestCase{
		{name: "http", setup: func(srv *http.Server, s *server.MCPServer, enabled bool) (string, string) {
			newStreamableHTTPTransport(srv, s, enabled)
			return streamableEndpointPath, streamableEndpointPath
		}},
		{name: "sse", setup: func(srv *http.Server, s *server.MCPServer, enabled bool) (string, string) {
			transport := newSSETransport(srv, s, enabled)
			return transport.CompleteMessagePath(), transport.CompleteSsePath()
		}},
	}
}

func TestHTTPServerBudgets(t *testing.T) {
	srv := newHTTPServer()
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("unbounded server timeouts: header=%s read=%s idle=%s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("write timeout %s would end long-lived streams", srv.WriteTimeout)
	}
}

func TestHTTPTransportBodyLimits(t *testing.T) {
	for _, tc := range httpLimitTestCases() {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/telemetry=%t", tc.name, enabled), func(t *testing.T) {
				configured := &http.Server{}
				path, streamPath := tc.setup(configured, newTracedServer(t, false, nil, nil), enabled)
				httpSrv := httptest.NewServer(configured.Handler)
				defer httpSrv.Close()

				for _, body := range []struct {
					name string
					data string
					want int
				}{
					{name: "valid", data: initializeMessage, want: http.StatusOK},
					{name: "known oversized", data: strings.Repeat("x", int(maxHTTPBodyBytes+1)), want: http.StatusRequestEntityTooLarge},
				} {
					t.Run(body.name, func(t *testing.T) {
						target := httpSrv.URL + path
						if tc.name == "sse" && body.want == http.StatusOK {
							stream := getStream(t, httpSrv, streamPath)
							defer func() { _ = stream.Body.Close() }()
							reader := bufio.NewReader(stream.Body)
							if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "event: endpoint" {
								t.Fatalf("SSE endpoint event = %q, err = %v", line, err)
							}
							line, err := reader.ReadString('\n')
							if err != nil || !strings.HasPrefix(line, "data: ") {
								t.Fatalf("SSE endpoint URL = %q, err = %v", line, err)
							}
							target = httpSrv.URL + strings.TrimSpace(strings.TrimPrefix(line, "data: "))
							body.want = http.StatusAccepted
						}
						req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(body.data))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Accept", "application/json, text/event-stream")
						resp, err := httpSrv.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = resp.Body.Close() }()
						if resp.StatusCode != body.want {
							payload, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
							t.Errorf("status %d, want %d: %s", resp.StatusCode, body.want, payload)
						}
					})
				}

				// A chunked upload has no advertised size. The middleware must still
				// stop at the limit, rather than trusting Content-Length alone.
				oversized := strings.NewReader(strings.Repeat("x", int(maxHTTPBodyBytes+1)))
				req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpSrv.URL+path, io.NopCloser(oversized))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := httpSrv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusRequestEntityTooLarge {
					t.Errorf("chunked status %d, want 413", resp.StatusCode)
				}
			})
		}
	}
}

func TestHTTPReadDeadlinesAndSSE(t *testing.T) {
	for _, tc := range httpLimitTestCases() {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/telemetry=%t", tc.name, enabled), func(t *testing.T) {
				if tc.name == "sse" {
					withFlag(t, sseKeepAliveInterval, 300*time.Millisecond)
				} else {
					withFlag(t, heartbeatInterval, 300*time.Millisecond)
				}
				configured := newHTTPServer()
				configured.ReadHeaderTimeout = 100 * time.Millisecond
				configured.ReadTimeout = 200 * time.Millisecond
				configured.IdleTimeout = 300 * time.Millisecond
				postPath, streamPath := tc.setup(configured, newTracedServer(t, false, nil, nil), enabled)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = configured.Close() })
				go func() { _ = configured.Serve(listener) }()

				for _, partial := range []string{
					"POST " + postPath + " HTTP/1.1\r\nHost: localhost\r\n",                                       // incomplete header
					fmt.Sprintf("POST %s HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1000\r\n\r\nx", postPath), // stalled body
				} {
					conn, err := net.Dial("tcp", listener.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					if _, err := io.WriteString(conn, partial); err != nil {
						t.Fatal(err)
					}
					_, err = io.ReadAll(conn)
					if closeErr := conn.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					if err != nil {
						t.Fatalf("stalled request did not terminate: %v", err)
					}
				}

				client := &http.Client{Timeout: time.Second}
				req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
					"http://"+listener.Addr().String()+streamPath, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Accept", "text/event-stream")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if tc.name == "sse" {
					readUntil(t, resp.Body, "event: endpoint")
				}
				// The first ping arrives after the 200 ms request-read deadline.
				readUntil(t, resp.Body, `"method":"ping"`)
			})
		}
	}
}
