package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/zekker6/mcp-helm/lib/helm_client"
)

func TestExtractCommonParams(t *testing.T) {
	client, err := helm_client.NewClient()
	if err != nil {
		t.Fatalf("failed to create helm client: %v", err)
	}

	tests := []struct {
		name                 string
		arguments            map[string]any
		resolveLatestVersion bool
		wantError            bool
		wantErrorContains    string
		wantChartName        string
	}{
		{
			name: "HTTP repo with chart_name provided",
			arguments: map[string]any{
				"repository_url": "https://charts.example.com",
				"chart_name":     "mychart",
			},
			resolveLatestVersion: false,
			wantError:            false,
			wantChartName:        "mychart",
		},
		{
			name: "HTTP repo with chart_name empty",
			arguments: map[string]any{
				"repository_url": "https://charts.example.com",
				"chart_name":     "",
			},
			resolveLatestVersion: false,
			wantError:            true,
			wantErrorContains:    "chart_name is required for HTTP repositories",
		},
		{
			name: "HTTP repo with chart_name missing",
			arguments: map[string]any{
				"repository_url": "https://charts.example.com",
			},
			resolveLatestVersion: false,
			wantError:            true,
			wantErrorContains:    "chart_name is required for HTTP repositories",
		},
		{
			name: "OCI URL with chart_name provided",
			arguments: map[string]any{
				"repository_url": "oci://ghcr.io/org/charts/mychart",
				"chart_name":     "explicit-name",
			},
			resolveLatestVersion: false,
			wantError:            false,
			wantChartName:        "explicit-name",
		},
		{
			name: "OCI URL with chart_name empty - extracts from URL",
			arguments: map[string]any{
				"repository_url": "oci://ghcr.io/org/charts/mychart",
				"chart_name":     "",
			},
			resolveLatestVersion: false,
			wantError:            false,
			wantChartName:        "mychart",
		},
		{
			name: "OCI URL with chart_name missing - extracts from URL",
			arguments: map[string]any{
				"repository_url": "oci://ghcr.io/org/charts/nginx-ingress",
			},
			resolveLatestVersion: false,
			wantError:            false,
			wantChartName:        "nginx-ingress",
		},
		{
			name: "OCI URL with version tag - extracts chart name without tag",
			arguments: map[string]any{
				"repository_url": "oci://ghcr.io/org/charts/mychart:1.2.3",
				"chart_name":     "",
			},
			resolveLatestVersion: false,
			wantError:            false,
			wantChartName:        "mychart",
		},
		{
			name: "repository_url missing",
			arguments: map[string]any{
				"chart_name": "mychart",
			},
			resolveLatestVersion: false,
			wantError:            true,
			wantErrorContains:    "repository_url",
		},
		{
			name: "trims whitespace from parameters",
			arguments: map[string]any{
				"repository_url": "  https://charts.example.com  ",
				"chart_name":     "  mychart  ",
			},
			resolveLatestVersion: false,
			wantError:            false,
			wantChartName:        "mychart",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "test_tool",
					Arguments: tt.arguments,
				},
			}

			params, errResult := ExtractCommonParams(context.Background(), request, client, tt.resolveLatestVersion)

			if tt.wantError {
				if errResult == nil {
					t.Errorf("expected error, got nil")
					return
				}
				// Check error message contains expected text
				if tt.wantErrorContains != "" {
					found := false
					for _, content := range errResult.Content {
						if textContent, ok := content.(mcp.TextContent); ok {
							if contains(textContent.Text, tt.wantErrorContains) {
								found = true
								break
							}
						}
					}
					if !found {
						t.Errorf("error message should contain %q", tt.wantErrorContains)
					}
				}
				return
			}

			if errResult != nil {
				t.Errorf("unexpected error: %v", errResult)
				return
			}

			if params.ChartName != tt.wantChartName {
				t.Errorf("ChartName = %q, want %q", params.ChartName, tt.wantChartName)
			}
		})
	}
}

func TestExtractRepositoryURL(t *testing.T) {
	tests := []struct {
		name              string
		arguments         map[string]any
		wantError         bool
		wantErrorContains string
		wantURL           string
	}{
		{
			name: "valid URL",
			arguments: map[string]any{
				"repository_url": "https://charts.example.com",
			},
			wantError: false,
			wantURL:   "https://charts.example.com",
		},
		{
			name: "trims whitespace",
			arguments: map[string]any{
				"repository_url": "  https://charts.example.com  ",
			},
			wantError: false,
			wantURL:   "https://charts.example.com",
		},
		{
			name:              "missing URL",
			arguments:         map[string]any{},
			wantError:         true,
			wantErrorContains: "repository_url",
		},
		{
			name: "OCI URL",
			arguments: map[string]any{
				"repository_url": "oci://ghcr.io/org/charts/mychart",
			},
			wantError: false,
			wantURL:   "oci://ghcr.io/org/charts/mychart",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "test_tool",
					Arguments: tt.arguments,
				},
			}

			url, errResult := ExtractRepositoryURL(request)

			if tt.wantError {
				if errResult == nil {
					t.Errorf("expected error, got nil")
					return
				}
				if tt.wantErrorContains != "" {
					found := false
					for _, content := range errResult.Content {
						if textContent, ok := content.(mcp.TextContent); ok {
							if contains(textContent.Text, tt.wantErrorContains) {
								found = true
								break
							}
						}
					}
					if !found {
						t.Errorf("error message should contain %q", tt.wantErrorContains)
					}
				}
				return
			}

			if errResult != nil {
				t.Errorf("unexpected error: %v", errResult)
				return
			}

			if url != tt.wantURL {
				t.Errorf("URL = %q, want %q", url, tt.wantURL)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsString(s, substr))
}

func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// startChartRepo serves an index.yaml holding one chart with two versions, so
// latest-version resolution has something to resolve without leaving the
// machine.
func startChartRepo(t *testing.T) string {
	t.Helper()

	const index = `apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: 1.0.0
      urls: ["charts/mychart-1.0.0.tgz"]
    - name: mychart
      version: 2.0.0
      urls: ["charts/mychart-2.0.0.tgz"]
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.yaml" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write([]byte(index))
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// TestExtractCommonParamsResolvesLatestVersionUnderCallerSpan covers the reason
// ctx is a parameter: with context.Background() the resolution would start its
// own root trace, and a tool call omitting chart_version would lose the Helm
// work from its own.
func TestExtractCommonParamsResolvesLatestVersionUnderCallerSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	client, err := helm_client.NewClient()
	if err != nil {
		t.Fatalf("failed to create helm client: %v", err)
	}

	ctx, parent := provider.Tracer("test").Start(context.Background(), "tool.probe")

	request := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "test_tool",
			Arguments: map[string]any{
				"repository_url": startChartRepo(t),
				"chart_name":     "mychart",
			},
		},
	}

	params, errResult := ExtractCommonParams(ctx, request, client, true)
	parent.End()

	if errResult != nil {
		t.Fatalf("ExtractCommonParams returned an error result: %+v", errResult)
	}
	if params.ChartVersion != "2.0.0" {
		t.Errorf("ChartVersion = %q, want the latest %q", params.ChartVersion, "2.0.0")
	}

	var found bool
	for _, span := range recorder.Ended() {
		if span.Name() != "helm.get_latest_version" {
			continue
		}

		found = true
		if span.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("helm.get_latest_version parent = %s, want the caller span %s",
				span.Parent().SpanID(), parent.SpanContext().SpanID())
		}
	}
	if !found {
		t.Fatal("no helm.get_latest_version span recorded")
	}
}
