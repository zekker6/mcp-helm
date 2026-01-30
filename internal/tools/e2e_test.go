package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	testRepoURL   = "https://prometheus-community.github.io/helm-charts"
	testChartName = "prometheus"

	testOCIRepoURL   = "oci://ghcr.io/victoriametrics/helm-charts/victoria-logs-single"
	testOCIChartName = "victoria-logs-single"
)

func getBinaryPath(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	path := "../../tmp/mcp-helm"
	absPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("failed to get absolute path of mcp-helm binary: %v", err)
	}
	if _, err := os.Stat(absPath); err == nil {
		return absPath
	}

	t.Fatal("mcp-helm binary not found. Run 'task build' first or use 'task test:e2e'")
	return ""
}

func newTestClient(t *testing.T) *client.Client {
	t.Helper()

	binaryPath := getBinaryPath(t)
	c, err := client.NewStdioMCPClient(binaryPath, nil, "-mode", "stdio")
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}

	t.Cleanup(func() {
		_ = c.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "mcp-helm-e2e-test",
				Version: "1.0.0",
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to initialize MCP client: %v", err)
	}

	return c
}

func TestE2E_ListRepositoryCharts(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_repository_charts",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	if !strings.Contains(content, testChartName) {
		t.Errorf("expected result to contain %q, got: %s", testChartName, content)
	}
}

func TestE2E_ListChartVersions(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_chart_versions",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     testChartName,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	if content == "" || content == "No versions found" {
		t.Errorf("expected versions list, got: %s", content)
	}
}

func TestE2E_GetLatestVersionOfChart(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_latest_version_of_chart",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     testChartName,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	// Version should be a semver-like string (e.g., "18.1.0")
	if content == "" || !strings.Contains(content, ".") {
		t.Errorf("expected semver version, got: %s", content)
	}
}

func TestE2E_GetChartValues(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_values",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     testChartName,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var values map[string]any
	if err := json.Unmarshal([]byte(content), &values); err != nil {
		if content == "" {
			t.Error("expected non-empty values content")
		}
	}
}

func TestE2E_GetChartDependencies(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_dependencies",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     testChartName,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var deps []any
	if err := json.Unmarshal([]byte(content), &deps); err != nil {
		t.Errorf("expected valid JSON array, got error: %v\ncontent: %s", err, truncate(content, 500))
	}
}

func TestE2E_GetChartContents(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_contents",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     testChartName,
				"recursive":      false,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var contents any
	if err := json.Unmarshal([]byte(content), &contents); err != nil {
		t.Errorf("expected valid JSON, got error: %v\ncontent: %s", err, truncate(content, 500))
	}
}

func TestE2E_GetChartImages(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_images",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     testChartName,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var imagesResult struct {
		Chart      string `json:"chart"`
		Version    string `json:"version"`
		ImageCount int    `json:"imageCount"`
		Images     []any  `json:"images"`
	}
	if err := json.Unmarshal([]byte(content), &imagesResult); err != nil {
		t.Errorf("expected valid JSON result, got error: %v\ncontent: %s", err, truncate(content, 500))
	}
	if imagesResult.Chart != testChartName {
		t.Errorf("expected chart name %q, got %q", testChartName, imagesResult.Chart)
	}
}

func TestE2E_RequiredParameterValidation(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "list_repository_charts",
			Arguments: map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	if !result.IsError {
		t.Errorf("expected error for missing required parameter, got success: %v", result.Content)
	}
}

func TestE2E_OCI_ListRepositoryCharts(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_repository_charts",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	if !strings.Contains(content, testOCIChartName) {
		t.Errorf("expected result to contain %q, got: %s", testOCIChartName, content)
	}
}

func TestE2E_OCI_ListChartVersions(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_chart_versions",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
				"chart_name":     "", // Should be extracted from URL
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	if content == "" || content == "No versions found" {
		t.Errorf("expected versions list, got: %s", content)
	}
	if !strings.Contains(content, ".") {
		t.Errorf("expected semver versions, got: %s", content)
	}
}

func TestE2E_OCI_GetLatestVersionOfChart(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_latest_version_of_chart",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
				"chart_name":     "",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	if content == "" || !strings.Contains(content, ".") {
		t.Errorf("expected semver version, got: %s", content)
	}
}

func TestE2E_OCI_GetChartValues(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_values",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
				"chart_name":     "",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var values map[string]any
	if err := json.Unmarshal([]byte(content), &values); err != nil {
		if content == "" {
			t.Error("expected non-empty values content")
		}
	}
}

func TestE2E_OCI_GetChartContents(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_contents",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
				"chart_name":     "",
				"recursive":      false,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var contents any
	if err := json.Unmarshal([]byte(content), &contents); err != nil {
		t.Errorf("expected valid JSON, got error: %v\ncontent: %s", err, truncate(content, 500))
	}
}

func TestE2E_OCI_GetChartImages(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "get_chart_images",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
				"chart_name":     "",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	var imagesResult struct {
		Chart      string `json:"chart"`
		Version    string `json:"version"`
		ImageCount int    `json:"imageCount"`
		Images     []any  `json:"images"`
	}
	if err := json.Unmarshal([]byte(content), &imagesResult); err != nil {
		t.Errorf("expected valid JSON result, got error: %v\ncontent: %s", err, truncate(content, 500))
	}
	if imagesResult.Chart != testOCIChartName {
		t.Errorf("expected chart name %q, got %q", testOCIChartName, imagesResult.Chart)
	}
}

func TestE2E_OCI_WithExplicitChartName(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_chart_versions",
			Arguments: map[string]any{
				"repository_url": testOCIRepoURL,
				"chart_name":     testOCIChartName, // Explicit name
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}

	content := getTextContent(t, result)
	if content == "" || content == "No versions found" {
		t.Errorf("expected versions list, got: %s", content)
	}
}

func TestE2E_HTTPRepo_RequiresChartName(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "list_chart_versions",
			Arguments: map[string]any{
				"repository_url": testRepoURL,
				"chart_name":     "",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	if !result.IsError {
		t.Errorf("expected error for empty chart_name with HTTP repo, got success: %v", result.Content)
	}
}

func getTextContent(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("expected non-empty content")
	}

	for _, c := range result.Content {
		if textContent, ok := c.(mcp.TextContent); ok {
			return textContent.Text
		}
	}

	t.Fatalf("expected TextContent, got: %T", result.Content[0])
	return ""
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
