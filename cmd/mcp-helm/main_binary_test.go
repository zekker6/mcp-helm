package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/zekker6/mcp-helm/lib/telemetry"
)

// subprocessEnv makes this test binary run the server instead of the tests.
// Flag parsing, the signal disposition, the exit codes and the shutdown flush
// only exist in a real process, so these tests drive one.
const subprocessEnv = "MCP_HELM_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(subprocessEnv) == "1" {
		main()
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// serverCommand re-executes this binary as the mcp-helm server. The parent's
// OTEL_ variables are dropped so a developer's own collector settings cannot
// reach the server under test.
func serverCommand(t *testing.T, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(os.Args[0], args...) //nolint:gosec // the test binary itself
	cmd.Env = []string{subprocessEnv + "=1"}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "OTEL_") || strings.HasPrefix(kv, subprocessEnv+"=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}

	return cmd
}

// exportingEnv points every signal at col and pins the wire format, so the
// collector's bodies decode as plain protobuf.
func exportingEnv(col *logCollector) map[string]string {
	return map[string]string{
		telemetry.EnvEnabled:             "true",
		telemetry.EnvEndpoint:            col.URL,
		"OTEL_EXPORTER_OTLP_COMPRESSION": "none",
	}
}

// syncBuffer collects a subprocess's stderr while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// logRecord is one line of the server's structured stderr output.
type logRecord map[string]any

// findLogRecord returns the single record whose msg is want.
func findLogRecord(t *testing.T, stderr, want string) logRecord {
	t.Helper()

	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, want) {
			continue
		}

		var record logRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record["msg"] == want {
			return record
		}
	}

	t.Fatalf("no log record with msg %q in:\n%s", want, stderr)

	return nil
}

// exitCode reports the status a finished subprocess exited with, and fails the
// test if it was killed by a signal instead.
func exitCode(t *testing.T, err error, stderr string) int {
	t.Helper()

	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run the server: %v\nstderr:\n%s", err, stderr)
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		t.Fatalf("the server was killed by %v instead of exiting: no signal handler is installed\nstderr:\n%s",
			status.Signal(), stderr)
	}

	return exitErr.ExitCode()
}

// TestBinaryLeavesTelemetryOffByDefault: with OTEL_ENABLED unset the binary
// behaves as it did before. The second case is the observable half of "no
// exporter is constructed", a reachable collector that is never contacted.
func TestBinaryLeavesTelemetryOffByDefault(t *testing.T) {
	t.Run("no otel variables", func(t *testing.T) {
		var stderr syncBuffer

		cmd := serverCommand(t, nil, "-mode", "stdio")
		// An immediately closed stdin is a clean end of session for the stdio
		// transport, so the process runs its whole lifecycle and exits.
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderr

		if code := exitCode(t, cmd.Run(), stderr.String()); code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr.String())
		}

		startup := findLogRecord(t, stderr.String(), "Starting MCP Helm server")
		if enabled, ok := startup["otelEnabled"].(bool); !ok || enabled {
			t.Errorf("otelEnabled = %v, want false", startup["otelEnabled"])
		}
		for key := range startup {
			if strings.HasPrefix(key, "otel.") {
				t.Errorf("startup line carries %q with telemetry off", key)
			}
		}

		if strings.Contains(stderr.String(), "opentelemetry sdk error") {
			t.Errorf("the sdk error handler is installed with telemetry off:\n%s", stderr.String())
		}
	})

	t.Run("endpoint set but not enabled", func(t *testing.T) {
		col := newLogCollector(t)
		var stderr syncBuffer

		// A stale endpoint left in the environment must not switch exporting
		// on, and must not fail startup either.
		cmd := serverCommand(t, map[string]string{telemetry.EnvEndpoint: col.URL}, "-mode", "stdio")
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderr

		if code := exitCode(t, cmd.Run(), stderr.String()); code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr.String())
		}

		if paths := col.paths(); len(paths) != 0 {
			t.Errorf("collector was contacted with telemetry off: %v", paths)
		}
	})
}

// TestBinaryRejectsInvalidTelemetryEndpoint covers the startup failure: an
// unusable endpoint has to stop the process with a message naming the variable
// that holds it, rather than starting a server that silently exports nothing.
func TestBinaryRejectsInvalidTelemetryEndpoint(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		named string
	}{
		{
			name: "base endpoint",
			env: map[string]string{
				telemetry.EnvEnabled:  "true",
				telemetry.EnvEndpoint: "ftp://collector:4318",
			},
			named: telemetry.EnvEndpoint,
		},
		{
			name: "per-signal endpoint",
			env: map[string]string{
				telemetry.EnvEnabled:         "true",
				telemetry.EnvEndpoint:        "http://collector:4318",
				telemetry.EnvMetricsEndpoint: "tcp://collector:4317",
			},
			named: telemetry.EnvMetricsEndpoint,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr syncBuffer

			cmd := serverCommand(t, tt.env, "-mode", "stdio")
			cmd.Stdin = strings.NewReader("")
			cmd.Stdout = io.Discard
			cmd.Stderr = &stderr

			if code := exitCode(t, cmd.Run(), stderr.String()); code == 0 {
				t.Fatalf("exit code = 0, want non-zero\nstderr:\n%s", stderr.String())
			}

			fatal := findLogRecord(t, stderr.String(), "fatal")
			message, _ := fatal["error"].(string)
			for _, want := range []string{tt.named, "unsupported scheme"} {
				if !strings.Contains(message, want) {
					t.Errorf("error %q does not mention %q", message, want)
				}
			}
		})
	}
}

// reserveAddr returns a loopback address nothing is listening on.
func reserveAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	return addr
}

// spansOf decodes every span the collector received.
func (c *logCollector) spansOf(t *testing.T) []*tracepb.Span {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	var spans []*tracepb.Span
	for _, req := range c.requests {
		if req.path != "/v1/traces" {
			continue
		}

		var export coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(req.body, &export); err != nil {
			t.Fatalf("decode a traces export: %v", err)
		}

		for _, resource := range export.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				spans = append(spans, scope.GetSpans()...)
			}
		}
	}

	return spans
}

// metricAttributes decodes every metric the collector received into its name
// and the attribute set of each of its data points.
func (c *logCollector) metricAttributes(t *testing.T) map[string][][]*commonpb.KeyValue {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	attributes := make(map[string][][]*commonpb.KeyValue)
	for _, req := range c.requests {
		if req.path != "/v1/metrics" {
			continue
		}

		var export colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(req.body, &export); err != nil {
			t.Fatalf("decode a metrics export: %v", err)
		}

		for _, resource := range export.GetResourceMetrics() {
			for _, scope := range resource.GetScopeMetrics() {
				for _, metric := range scope.GetMetrics() {
					attributes[metric.GetName()] = append(attributes[metric.GetName()], dataPointAttributes(metric)...)
				}
			}
		}
	}

	return attributes
}

// dataPointAttributes flattens the attribute sets of whichever data point type a
// metric carries. All five are walked so a metric that changes shape cannot skip
// the assertion.
func dataPointAttributes(metric *metricspb.Metric) [][]*commonpb.KeyValue {
	var sets [][]*commonpb.KeyValue

	for _, point := range metric.GetGauge().GetDataPoints() {
		sets = append(sets, point.GetAttributes())
	}
	for _, point := range metric.GetSum().GetDataPoints() {
		sets = append(sets, point.GetAttributes())
	}
	for _, point := range metric.GetHistogram().GetDataPoints() {
		sets = append(sets, point.GetAttributes())
	}
	for _, point := range metric.GetExponentialHistogram().GetDataPoints() {
		sets = append(sets, point.GetAttributes())
	}
	for _, point := range metric.GetSummary().GetDataPoints() {
		sets = append(sets, point.GetAttributes())
	}

	return sets
}

// sawPath reports whether the collector answered a request on path.
func (c *logCollector) sawPath(path string) bool {
	return slices.Contains(c.paths(), path)
}

// ancestry names the span chain from the span called name up to its root.
func ancestry(t *testing.T, spans []*tracepb.Span, name string) []string {
	t.Helper()

	byID := make(map[string]*tracepb.Span, len(spans))
	var start *tracepb.Span
	for _, span := range spans {
		byID[string(span.GetSpanId())] = span
		if span.GetName() != name {
			continue
		}
		if start != nil {
			t.Fatalf("more than one %q span was exported", name)
		}
		start = span
	}

	if start == nil {
		t.Fatalf("no %q span was exported, got %v", name, spanNamesOf(spans))
	}

	chain := []string{start.GetName()}
	for span := start; len(span.GetParentSpanId()) != 0; {
		parent, ok := byID[string(span.GetParentSpanId())]
		if !ok {
			t.Fatalf("span %q references a parent that was never exported, chain so far %v", span.GetName(), chain)
		}
		if parent.GetTraceId() == nil || string(parent.GetTraceId()) != string(start.GetTraceId()) {
			t.Fatalf("span %q is in a different trace from %q", parent.GetName(), name)
		}

		chain = append(chain, parent.GetName())
		span = parent
	}

	return chain
}

func spanNamesOf(spans []*tracepb.Span) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.GetName())
	}

	return names
}

const (
	// probeOCIRepo with probeOCIVersion produces an OCI reference whose tag
	// starts with a dot. Helm's registry client rejects it while parsing, so
	// the call reaches helm.oci.pull and fails there without leaving the
	// machine.
	probeOCIRepo    = "oci://registry.invalid/org/telemetry-probe-chart"
	probeOCIVersion = ".invalid-tag"

	// probeCredentialRepo fails inside repo.NewChartRepository - no Helm getter
	// handles the ftp scheme - so its embedded credential reaches the span
	// attributes without any network access.
	probeCredentialSecret = "s3cret"
	probeCredentialRepo   = "ftp://user:" + probeCredentialSecret + "@charts.invalid/stable"
	probeChartName        = "telemetry-probe-chart"
	probeChartVersion     = "9.9.9"
)

// TestBinaryFlushesFullTraceOnSIGTERM runs the real binary in http mode against
// a collector, drives two tool calls through it and terminates it the way a
// container runtime would. Everything asserted afterwards is what actually left
// the process: the trace shape from the inbound request down to helm.oci.pull,
// no credentials on any span attribute, no chart identifiers on any metric
// attribute, and all three signals flushed before exit.
func TestBinaryFlushesFullTraceOnSIGTERM(t *testing.T) {
	col := newLogCollector(t)
	addr := reserveAddr(t)

	var stderr syncBuffer
	cmd := serverCommand(t, exportingEnv(col), "-mode", "http", "-httpListenAddr", addr)
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitForListener(t, addr)
	callProbeTools(t, addr)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the server: %v", err)
	}

	if code := exitCode(t, cmd.Wait(), stderr.String()); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr.String())
	}

	for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if !col.sawPath(want) {
			t.Errorf("SIGTERM did not flush %s, paths seen: %v", want, col.paths())
		}
	}

	spans := col.spansOf(t)

	want := []string{
		"helm.oci.pull",
		"helm.load_chart",
		"helm.get_chart_values",
		"tool.get_chart_values",
		"tools/call get_chart_values",
		"POST " + streamableEndpointPath,
	}
	if got := ancestry(t, spans, "helm.oci.pull"); !slices.Equal(got, want) {
		t.Errorf("trace shape = %v, want %v", got, want)
	}

	for _, span := range spans {
		for _, attr := range span.GetAttributes() {
			if strings.Contains(attr.GetValue().String(), probeCredentialSecret) {
				t.Errorf("span %q attribute %q leaks credentials", span.GetName(), attr.GetKey())
			}
		}
	}

	assertMetricsCarryNoChartIdentifiers(t, col)
}

// callProbeTools completes an initialize and the two tool calls the assertions
// need: an OCI one producing the trace shape, and one whose repository URL
// carries a credential.
func callProbeTools(t *testing.T, addr string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := client.NewStreamableHttpClient("http://" + addr + streamableEndpointPath)
	if err != nil {
		t.Fatalf("build MCP client: %v", err)
	}

	if _, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "mcp-helm-binary-test", Version: "1.0.0"},
		},
	}); err != nil {
		t.Fatalf("initialize over http: %v", err)
	}

	calls := []map[string]any{
		{"repository_url": probeOCIRepo, "chart_name": "", "chart_version": probeOCIVersion},
		{"repository_url": probeCredentialRepo, "chart_name": probeChartName, "chart_version": probeChartVersion},
	}
	for _, arguments := range calls {
		result, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "get_chart_values", Arguments: arguments},
		})
		if err != nil {
			t.Fatalf("tools/call %v: %v", arguments, err)
		}
		if !result.IsError {
			t.Fatalf("expected an error result for %v, got %+v", arguments, result.Content)
		}
	}

	// Close before the signal: an open session would hold the graceful
	// shutdown open for its whole timeout.
	if err := c.Close(); err != nil {
		t.Fatalf("close the MCP client: %v", err)
	}
}

// assertMetricsCarryNoChartIdentifiers pins the cardinality rule at the wire:
// chart names, chart versions and repository URLs are span attributes, and a
// metric carrying one would mint a series per chart.
func assertMetricsCarryNoChartIdentifiers(t *testing.T, col *logCollector) {
	t.Helper()

	forbidden := []string{
		probeChartName,
		probeChartVersion,
		probeOCIVersion,
		"charts.invalid",
		"registry.invalid",
		probeCredentialSecret,
	}

	names := make(map[string]bool)
	for name, sets := range col.metricAttributes(t) {
		names[name] = true
		for _, attrs := range sets {
			for _, attr := range attrs {
				for _, value := range forbidden {
					if strings.Contains(attr.GetValue().String(), value) {
						t.Errorf("metric %s attribute %q carries %q", name, attr.GetKey(), value)
					}
				}
			}
		}
	}

	for _, want := range []string{"mcp.server.operation.duration", "mcp_helm.helm.operation.duration"} {
		if !names[want] {
			t.Errorf("%s was never exported, got %v", want, sortedKeys(names))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}

// TestBinaryExitsCleanlyOnSIGTERMInStdioMode covers the transport with no server
// to drain. serve now installs the process's only signal handler, so a SIGTERM
// has to unwind Listen and flush telemetry rather than kill the process.
func TestBinaryExitsCleanlyOnSIGTERMInStdioMode(t *testing.T) {
	col := newLogCollector(t)

	var stderr syncBuffer
	cmd := serverCommand(t, exportingEnv(col), "-mode", "stdio")
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("open stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open stdout: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// A completed request proves Listen is running, and therefore that the
	// signal handler serve installs before it is in place: signalling any
	// earlier would race the default disposition.
	if _, err := io.WriteString(stdin, initializeMessage+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	readLine(t, stdout)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the server: %v", err)
	}

	if code := exitCode(t, cmd.Wait(), stderr.String()); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr.String())
	}

	// stdin was open for the whole run - Wait closes it - so the process cannot
	// have ended on EOF: the flush only happened because the signal unwound the
	// transport.
	if !col.sawPath("/v1/logs") {
		t.Errorf("SIGTERM did not flush logs, paths seen: %v", col.paths())
	}
}

// readLine reads one line, failing the test rather than blocking forever.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()

	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)

	go func() {
		var line []byte
		buf := make([]byte, 1)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if buf[0] == '\n' {
					break
				}
				line = append(line, buf[0])
			}
			if err != nil {
				lines <- result{err: err}

				return
			}
		}
		lines <- result{line: string(line)}
	}()

	select {
	case res := <-lines:
		if res.err != nil {
			t.Fatalf("read a response line: %v", res.err)
		}

		return res.line
	case <-time.After(30 * time.Second):
		t.Fatal("no response line arrived")
	}

	return ""
}
