package helm_client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// errorTypeErrorString is the error.type semconv derives from every failure
// this package produces: the Helm SDK and this package both build errors with
// fmt.Errorf("...%v"), which leaves errors.errorString as the concrete type.
const errorTypeErrorString = "*errors.errorString"

var (
	meterReaderOnce sync.Once
	meterReader     *sdkmetric.ManualReader
)

// globalMeterReader installs a manual-reader meter provider as the OTel global,
// once for the whole test binary: otel.SetMeterProvider rewires the delegating
// global's instruments only on its first call (internal/global/state.go
// delegateMeterOnce). Tests share the reader and filter by operation.
func globalMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	meterReaderOnce.Do(func() {
		meterReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(meterReader)))
	})

	return meterReader
}

// operationPoint returns the single data point recorded for operation, failing
// the test when the instrument or the operation is missing.
func operationPoint(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	operation string,
) metricdata.HistogramDataPoint[float64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != operationDurationInstrument {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
			}

			for _, point := range histogram.DataPoints {
				if value, found := point.Attributes.Value(attrHelmOperation); found && value.AsString() == operation {
					return point
				}
			}
		}
	}

	t.Fatalf("no %s data point for operation %q", operationDurationInstrument, operation)

	return metricdata.HistogramDataPoint[float64]{}
}

// assertAttributes fails unless attrs holds exactly want.
func assertAttributes(t *testing.T, attrs attribute.Set, want map[attribute.Key]string) {
	t.Helper()

	if got := attrs.Len(); got != len(want) {
		t.Errorf("attribute count = %d, want %d (%v)", got, len(want), attrs.ToSlice())
	}

	for key, wantValue := range want {
		value, found := attrs.Value(key)
		if !found {
			t.Errorf("attribute %q missing from %v", key, attrs.ToSlice())

			continue
		}

		// String rather than AsString: server.port is an int, and AsString
		// reports an empty string for every non-string value.
		if value.String() != wantValue {
			t.Errorf("attribute %q = %q, want %q", key, value.String(), wantValue)
		}
	}
}

// TestOperationDurationRecordsAfterProviderRegistration pins the no-ClientOption
// design: operationDuration is built at package init against the no-op global,
// and must still record once telemetry.Setup registers the real provider.
func TestOperationDurationRecordsAfterProviderRegistration(t *testing.T) {
	reader := globalMeterReader(t)
	const operation = "probe.records_after_registration"

	recordOperation(context.Background(), operation, repositoryTypeHTTP, time.Now().Add(-1500*time.Millisecond), nil)

	point := operationPoint(t, reader, operation)
	if point.Count != 1 {
		t.Errorf("count = %d, want 1", point.Count)
	}

	if point.Sum < 1.5 {
		t.Errorf("sum = %v seconds, want at least 1.5", point.Sum)
	}

	assertAttributes(t, point.Attributes, map[attribute.Key]string{
		attrHelmOperation:      operation,
		attrHelmRepositoryType: repositoryTypeHTTP,
	})

	if _, found := point.Attributes.Value(semconv.ErrorTypeKey); found {
		t.Errorf("attributes = %v, want no error.type on a successful call", point.Attributes.ToSlice())
	}
}

// TestRecordOperationAttributes covers both outcomes and asserts the instrument
// carries nothing beyond the bounded attributes. Success is the absence of
// error.type.
func TestRecordOperationAttributes(t *testing.T) {
	reader := globalMeterReader(t)

	tests := []struct {
		name           string
		operation      string
		repositoryType string
		err            error
		wantErrorType  string
	}{
		{
			name:           "success over http",
			operation:      "probe.http_ok",
			repositoryType: repositoryTypeHTTP,
		},
		{
			name:           "failure over oci",
			operation:      "probe.oci_error",
			repositoryType: repositoryTypeOCI,
			err:            errors.New("pull failed"),
			wantErrorType:  errorTypeErrorString,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recordOperation(context.Background(), tc.operation, tc.repositoryType, time.Now(), tc.err)

			want := map[attribute.Key]string{
				attrHelmOperation:      tc.operation,
				attrHelmRepositoryType: tc.repositoryType,
			}
			if tc.wantErrorType != "" {
				want[semconv.ErrorTypeKey] = tc.wantErrorType
			}

			attrs := operationPoint(t, reader, tc.operation).Attributes
			assertAttributes(t, attrs, want)

			if got := attrs.Len(); got != len(want) {
				t.Errorf("attribute count = %d, want %d (%v)", got, len(want), attrs.ToSlice())
			}
		})
	}
}

func TestErrorAttributes(t *testing.T) {
	if got := errorAttributes(nil); got != nil {
		t.Errorf("errorAttributes(nil) = %v, want none", got)
	}

	got := errorAttributes(errors.New("boom"))
	if len(got) != 1 || got[0].Key != semconv.ErrorTypeKey {
		t.Fatalf("errorAttributes(err) = %v, want a single error.type attribute", got)
	}
	if want := errorTypeErrorString; got[0].Value.AsString() != want {
		t.Errorf("error.type = %q, want %q", got[0].Value.AsString(), want)
	}
}

func TestRepositoryType(t *testing.T) {
	tests := map[string]string{
		"oci://registry.example.com/org/chart": repositoryTypeOCI,
		"https://charts.example.com":           repositoryTypeHTTP,
		"http://charts.example.com":            repositoryTypeHTTP,
	}

	for repoURL, want := range tests {
		if got := repositoryType(repoURL); got != want {
			t.Errorf("repositoryType(%q) = %q, want %q", repoURL, got, want)
		}
	}
}

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "http credentials are stripped",
			raw:  "https://user:s3cret@charts.example.com/stable/index.yaml",
			want: "https://charts.example.com/stable/index.yaml",
		},
		{
			name: "oci credentials are stripped",
			raw:  "oci://user:s3cret@registry.example.com/org/chart",
			want: "oci://registry.example.com/org/chart",
		},
		{
			name: "username without password is stripped",
			raw:  "https://user@charts.example.com/stable",
			want: "https://charts.example.com/stable",
		},
		{
			name: "url without credentials is untouched",
			raw:  "https://charts.example.com/stable/nginx-1.2.3.tgz",
			want: "https://charts.example.com/stable/nginx-1.2.3.tgz",
		},
		{
			// What parseOCIReference produces: no oci:// prefix, so url.Parse
			// finds no authority and the credentials survive the bare form.
			name: "bare oci reference credentials are stripped",
			raw:  "user:s3cret@registry.example.com/org/chart:1.2.3",
			want: "registry.example.com/org/chart:1.2.3",
		},
		{
			name: "bare oci reference username is stripped",
			raw:  "user@registry.example.com/org/chart",
			want: "registry.example.com/org/chart",
		},
		{
			name: "bare oci reference without credentials is untouched",
			raw:  "registry.example.com/org/chart:1.2.3",
			want: "registry.example.com/org/chart:1.2.3",
		},
		{
			name: "an at sign in a path is not a credential",
			raw:  "https://charts.example.com/stable/nginx@sha256",
			want: "https://charts.example.com/stable/nginx@sha256",
		},
		{
			name: "empty url",
			raw:  "",
			want: "",
		},
		{
			name: "unparseable url with credentials drops everything before the at sign",
			raw:  "://user:s3cret@charts.example.com/stable",
			want: "charts.example.com/stable",
		},
		{
			name: "unparseable url without credentials is untouched",
			raw:  "https://[::1",
			want: "https://[::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeURL(tc.raw)
			if got != tc.want {
				t.Errorf("sanitizeURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}

			if strings.Contains(got, "s3cret") {
				t.Errorf("sanitizeURL(%q) leaked credentials: %q", tc.raw, got)
			}
		})
	}
}

// TestStartSpanUsesGlobalTracerProvider covers the other half of the sourcing
// decision: the tracer is read from the global per call, so a provider
// registered after this package loaded is the one recording.
func TestStartSpanUsesGlobalTracerProvider(t *testing.T) {
	recorder := spanRecorder(t)

	ctx, parent := startSpan(context.Background(), "helm.parent")
	_, child := startSpan(ctx, "helm.child")
	child.End()
	parent.End()

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}

	if spans[0].Name() != "helm.child" || spans[1].Name() != "helm.parent" {
		t.Fatalf("span names = %q, %q, want helm.child, helm.parent", spans[0].Name(), spans[1].Name())
	}

	if spans[0].Parent().SpanID() != spans[1].SpanContext().SpanID() {
		t.Error("helm.child is not a child of helm.parent")
	}

	if got := spans[0].InstrumentationScope().Name; got != scopeName {
		t.Errorf("instrumentation scope = %q, want %q", got, scopeName)
	}
}

// spanRecorder installs a recording tracer provider as the OTel global for one
// test. The tracer provider, unlike the meter provider, can be swapped
// repeatedly: startSpan reads it per call.
func spanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	return recorder
}

// findSpan returns the single ended span with the given name.
func findSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	var found sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() != name {
			continue
		}

		if found != nil {
			t.Fatalf("recorded more than one %q span", name)
		}

		found = span
	}

	if found == nil {
		t.Fatalf("no %q span recorded", name)
	}

	return found
}

// latestSpan returns the most recently ended span with the given name.
func latestSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	var found sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == name {
			found = span
		}
	}

	if found == nil {
		t.Fatalf("no %q span recorded", name)
	}

	return found
}

// operationPoints returns every data point recorded for operation.
func operationPoints(
	t *testing.T,
	reader *sdkmetric.ManualReader,
	operation string,
) []metricdata.HistogramDataPoint[float64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	var points []metricdata.HistogramDataPoint[float64]
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != operationDurationInstrument {
				continue
			}

			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
			}

			for _, point := range histogram.DataPoints {
				if value, found := point.Attributes.Value(attrHelmOperation); found && value.AsString() == operation {
					points = append(points, point)
				}
			}
		}
	}

	if len(points) == 0 {
		t.Fatalf("no %s data point for operation %q", operationDurationInstrument, operation)
	}

	return points
}

const (
	// telemetryRepoURL fails inside repo.NewChartRepository, since no Helm
	// getter handles ftp, so nothing touches the network. Its embedded
	// credential is what the sanitized repository attribute is checked against.
	telemetryRepoURL          = "ftp://user:s3cret@charts.invalid/stable"
	telemetryRepoURLSanitized = "ftp://charts.invalid/stable"

	telemetryChartName    = "telemetry-probe-chart"
	telemetryChartVersion = "9.9.9"
)

// TestGetChartValuesSpan covers what startOperation wires up: the span joins the
// caller's trace, carries the chart identifiers, and reports the failure on both
// the status and an exception event.
func TestGetChartValuesSpan(t *testing.T) {
	recorder := spanRecorder(t)
	client := newTestClient(t)

	ctx, caller := otel.GetTracerProvider().Tracer("caller").Start(context.Background(), "caller")
	_, err := client.GetChartValues(ctx, telemetryRepoURL, telemetryChartName, telemetryChartVersion)
	caller.End()

	if err == nil {
		t.Fatal("GetChartValues() on an unhandled scheme returned no error")
	}

	span := findSpan(t, recorder, "helm."+operationGetChartValues)

	if span.Parent().SpanID() != caller.SpanContext().SpanID() {
		t.Error("helm.get_chart_values is not a child of the caller's span")
	}

	assertAttributes(t, attribute.NewSet(span.Attributes()...), map[attribute.Key]string{
		attrHelmRepositoryURL:  telemetryRepoURLSanitized,
		attrHelmRepositoryType: repositoryTypeHTTP,
		attrHelmChartName:      telemetryChartName,
		attrHelmChartVersion:   telemetryChartVersion,
		semconv.ErrorTypeKey:   errorTypeErrorString,
	})

	if got := span.Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want %v", got, codes.Error)
	}

	if span.Status().Description != err.Error() {
		t.Errorf("span status description = %q, want %q", span.Status().Description, err.Error())
	}

	var recorded bool
	for _, event := range span.Events() {
		if event.Name == semconv.ExceptionEventName {
			recorded = true
		}
	}

	if !recorded {
		t.Errorf("span recorded no exception event, has %v", span.Events())
	}
}

// TestGetChartValuesMetricAttributes pins the cardinality rule: the chart name,
// the chart version and the repository URL are span-only, and every point the
// operation records carries the bounded attributes and nothing else.
func TestGetChartValuesMetricAttributes(t *testing.T) {
	reader := globalMeterReader(t)
	client := newTestClient(t)

	if _, err := client.GetChartValues(
		context.Background(),
		telemetryRepoURL,
		telemetryChartName,
		telemetryChartVersion,
	); err == nil {
		t.Fatal("GetChartValues() on an unhandled scheme returned no error")
	}

	// The reader is shared by the test binary, so the call above is identified
	// by its full attribute set rather than by being the only point.
	want := attribute.NewSet(
		attribute.String(attrHelmOperation, operationGetChartValues),
		attribute.String(attrHelmRepositoryType, repositoryTypeHTTP),
		semconv.ErrorTypeKey.String(errorTypeErrorString),
	)

	forbidden := []string{telemetryChartName, telemetryChartVersion, "charts.invalid", "s3cret"}
	requiredKeys := []attribute.Key{attrHelmOperation, attrHelmRepositoryType}
	// error.type is the only optional one: a successful call omits it.
	boundedKeys := append([]attribute.Key{semconv.ErrorTypeKey}, requiredKeys...)

	var found bool
	for _, point := range operationPoints(t, reader, operationGetChartValues) {
		if point.Attributes.Equals(&want) {
			found = true
		}

		// Every point the operation records, whichever test produced it,
		// carries the bounded attributes and nothing else.
		for _, key := range requiredKeys {
			if _, ok := point.Attributes.Value(key); !ok {
				t.Errorf("attribute %q missing from %v", key, point.Attributes.ToSlice())
			}
		}
		for _, attr := range point.Attributes.ToSlice() {
			if !slices.Contains(boundedKeys, attr.Key) {
				t.Errorf("unexpected attribute %q on %v", attr.Key, point.Attributes.ToSlice())
			}
		}

		for _, attr := range point.Attributes.ToSlice() {
			for _, value := range forbidden {
				if strings.Contains(attr.Value.String(), value) {
					t.Errorf("metric attribute %q leaks %q: %q", attr.Key, value, attr.Value.String())
				}
			}
		}
	}

	if !found {
		t.Errorf("no data point with attributes %v", want.Encoded(attribute.DefaultEncoder()))
	}
}

const (
	// The pair produces an OCI reference whose tag starts with a dot. Helm's
	// registry client parses before it connects, so the pull fails structurally
	// rather than waiting on a name that must not resolve.
	telemetryOCIRepoURL    = "oci://registry.invalid/org/telemetry-probe-chart"
	telemetryOCIBadVersion = ".invalid-tag"
	telemetryOCIRef        = telemetryOCIRegistry + "/org/telemetry-probe-chart:" + telemetryOCIBadVersion
	telemetryOCIRegistry   = "registry.invalid"
)

// TestLoadChartOCISpanChain pins the OCI half of the inner-span tree: the
// operation span parents helm.load_chart, which parents the single client span
// its branch makes.
func TestLoadChartOCISpanChain(t *testing.T) {
	recorder := spanRecorder(t)
	client := newTestClient(t)

	// The chart name lives in the URL for OCI, as it does for every other
	// caller of an oci:// repository.
	if _, err := client.GetChartValues(
		context.Background(),
		telemetryOCIRepoURL,
		"",
		telemetryOCIBadVersion,
	); err == nil {
		t.Fatal("GetChartValues() with an invalid OCI tag returned no error")
	}

	values := findSpan(t, recorder, "helm."+operationGetChartValues)
	load := findSpan(t, recorder, spanLoadChart)
	pull := findSpan(t, recorder, spanOCIPull)

	if load.Parent().SpanID() != values.SpanContext().SpanID() {
		t.Errorf("%s is not a child of helm.%s", spanLoadChart, operationGetChartValues)
	}

	if pull.Parent().SpanID() != load.SpanContext().SpanID() {
		t.Errorf("%s is not a child of %s", spanOCIPull, spanLoadChart)
	}

	assertAttributes(t, attribute.NewSet(load.Attributes()...), map[attribute.Key]string{
		attrHelmRepositoryType: repositoryTypeOCI,
		attrHelmChartName:      "",
		attrHelmChartVersion:   telemetryOCIBadVersion,
		semconv.ErrorTypeKey:   errorTypeErrorString,
	})

	assertAttributes(t, attribute.NewSet(pull.Attributes()...), map[attribute.Key]string{
		attrHelmOCIRef:           telemetryOCIRef,
		semconv.ServerAddressKey: telemetryOCIRegistry,
		semconv.ServerPortKey:    "443",
		semconv.ErrorTypeKey:     errorTypeErrorString,
	})

	if got := pull.SpanKind(); got != trace.SpanKindClient {
		t.Errorf("%s kind = %v, want %v", spanOCIPull, got, trace.SpanKindClient)
	}

	for _, span := range []sdktrace.ReadOnlySpan{pull, load} {
		if got := span.Status().Code; got != codes.Error {
			t.Errorf("%s status = %v, want %v", span.Name(), got, codes.Error)
		}
	}
}

// TestListChartVersionsOCITagsSpan covers the other OCI client span, which no
// chart load reaches.
func TestListChartVersionsOCITagsSpan(t *testing.T) {
	recorder := spanRecorder(t)
	client := newTestClient(t)

	// The tag rides in the repository URL here, as it does for a pinned oci://
	// repository, and carries the same invalid value into the reference parser.
	if _, err := client.ListChartVersions(
		context.Background(),
		telemetryOCIRepoURL+":"+telemetryOCIBadVersion,
		"",
	); err == nil {
		t.Fatal("ListChartVersions() with an invalid OCI reference returned no error")
	}

	versions := findSpan(t, recorder, "helm."+operationListChartVersions)
	tags := findSpan(t, recorder, spanOCITags)

	if tags.Parent().SpanID() != versions.SpanContext().SpanID() {
		t.Errorf("%s is not a child of helm.%s", spanOCITags, operationListChartVersions)
	}

	assertAttributes(t, attribute.NewSet(tags.Attributes()...), map[attribute.Key]string{
		attrHelmOCIRef:           telemetryOCIRef,
		semconv.ServerAddressKey: telemetryOCIRegistry,
		semconv.ServerPortKey:    "443",
		semconv.ErrorTypeKey:     errorTypeErrorString,
	})

	if got := tags.SpanKind(); got != trace.SpanKindClient {
		t.Errorf("%s kind = %v, want %v", spanOCITags, got, trace.SpanKindClient)
	}

	if got := tags.Status().Code; got != codes.Error {
		t.Errorf("%s status = %v, want %v", spanOCITags, got, codes.Error)
	}
}

// TestLoadChartHTTPSpanOrder pins the HTTP half: the index fetch is a separate
// span that has finished before the archive download starts, which is what
// makes a slow index distinguishable from a slow download in a trace.
func TestLoadChartHTTPSpanOrder(t *testing.T) {
	recorder := spanRecorder(t)

	// The index resolves, the archive it points at does not: the download span
	// is reached and fails without leaving the test's own server.
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = w.Write(createTestIndex(serverURL))

			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()
	serverURL = server.URL

	client := newTestClient(t)

	if _, err := client.GetChartValues(context.Background(), server.URL, "test-chart", "1.0.0"); err == nil {
		t.Fatal("GetChartValues() with a missing chart archive returned no error")
	}

	load := findSpan(t, recorder, spanLoadChart)
	index := findSpan(t, recorder, spanRepoIndex)
	download := findSpan(t, recorder, spanChartDownload)

	if index.Parent().SpanID() != load.SpanContext().SpanID() {
		t.Errorf("%s is not a child of %s", spanRepoIndex, spanLoadChart)
	}

	if download.Parent().SpanID() != load.SpanContext().SpanID() {
		t.Errorf("%s is not a child of %s", spanChartDownload, spanLoadChart)
	}

	if !index.EndTime().Before(download.StartTime()) {
		t.Errorf(
			"%s ended at %v, want before %s started at %v",
			spanRepoIndex, index.EndTime(), spanChartDownload, download.StartTime(),
		)
	}

	parsedURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}

	assertAttributes(t, attribute.NewSet(index.Attributes()...), map[attribute.Key]string{
		attrHelmRepositoryURL:    server.URL,
		semconv.ServerAddressKey: parsedURL.Hostname(),
		semconv.ServerPortKey:    parsedURL.Port(),
	})

	assertAttributes(t, attribute.NewSet(download.Attributes()...), map[attribute.Key]string{
		semconv.URLFullKey:       server.URL + "/charts/test-chart-1.0.0.tgz",
		semconv.ServerAddressKey: parsedURL.Hostname(),
		semconv.ServerPortKey:    parsedURL.Port(),
		semconv.ErrorTypeKey:     errorTypeErrorString,
	})

	if got := index.Status().Code; got != codes.Unset {
		t.Errorf("%s status = %v, want %v on a served index", spanRepoIndex, got, codes.Unset)
	}

	if got := download.Status().Code; got != codes.Error {
		t.Errorf("%s status = %v, want %v", spanChartDownload, got, codes.Error)
	}

	for _, span := range []sdktrace.ReadOnlySpan{index, download} {
		if got := span.SpanKind(); got != trace.SpanKindClient {
			t.Errorf("%s kind = %v, want %v", span.Name(), got, trace.SpanKindClient)
		}
	}
}

// TestSpanAttributesNeverCarryCredentials sweeps every attribute of every span,
// not the one each test above names: sanitizing happens per call site, so a new
// attribute carrying a URL is the leak a targeted assertion cannot see.
func TestSpanAttributesNeverCarryCredentials(t *testing.T) {
	const secret = "s3cret"

	tests := []struct {
		name string
		call func(*HelmClient) error
	}{
		{
			// An HTTP repository: the credentials ride in the repository URL,
			// which reaches the operation span through startOperation.
			name: "http repository url",
			call: func(client *HelmClient) error {
				_, err := client.GetChartValues(
					context.Background(),
					telemetryRepoURL,
					telemetryChartName,
					telemetryChartVersion,
				)

				return err
			},
		},
		{
			// An OCI reference: parseOCIReference strips the oci:// prefix, so
			// what reaches helm.oci.ref has no authority left for url.Parse to
			// recognise the userinfo in.
			name: "bare oci reference",
			call: func(client *HelmClient) error {
				_, err := client.ListChartVersions(
					context.Background(),
					"oci://user:"+secret+"@registry.invalid/org/telemetry-probe-chart:"+telemetryOCIBadVersion,
					"",
				)

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := spanRecorder(t)

			if err := tt.call(newTestClient(t)); err == nil {
				t.Fatal("expected the call to fail without leaving the machine")
			}

			spans := recorder.Ended()
			if len(spans) == 0 {
				t.Fatal("no spans recorded")
			}

			for _, span := range spans {
				for _, attr := range span.Attributes() {
					if strings.Contains(attr.Value.String(), secret) {
						t.Errorf("span %q attribute %q leaks credentials: %q",
							span.Name(), attr.Key, attr.Value.String())
					}
				}

				// The error message travels to the collector too: endSpan
				// copies it into the span status and into the exception event.
				if strings.Contains(span.Status().Description, secret) {
					t.Errorf("span %q status leaks credentials: %q",
						span.Name(), span.Status().Description)
				}

				for _, event := range span.Events() {
					for _, attr := range event.Attributes {
						if strings.Contains(attr.Value.String(), secret) {
							t.Errorf("span %q event %q attribute %q leaks credentials: %q",
								span.Name(), event.Name, attr.Key, attr.Value.String())
						}
					}
				}
			}
		})
	}
}

// TestSuccessfulOperationsRecordResultAttributes drives the operations against a
// repository serving a real chart archive. Every other test here fails before a
// chart is parsed, so the result counts, the absent error.type and the
// helm.parse.* spans have no other coverage.
func TestSuccessfulOperationsRecordResultAttributes(t *testing.T) {
	reader := globalMeterReader(t)
	recorder := spanRecorder(t)

	repoURL, _ := startHTTPChartRepo(t, false, buildMatrixChartTGZ(t))
	client := newTestClient(t)
	ctx := context.Background()

	tests := []struct {
		operation string
		call      func() error
		want      map[attribute.Key]string
	}{
		{
			operation: operationListCharts,
			call: func() error {
				_, err := client.ListCharts(ctx, repoURL)

				return err
			},
			want: map[attribute.Key]string{attrHelmChartCount: "1"},
		},
		{
			operation: operationListChartVersions,
			call: func() error {
				_, err := client.ListChartVersions(ctx, repoURL, matrixChart)

				return err
			},
			want: map[attribute.Key]string{attrHelmVersionCount: "1"},
		},
		{
			operation: operationGetLatestVersion,
			call: func() error {
				_, err := client.GetChartLatestVersion(ctx, repoURL, matrixChart)

				return err
			},
			want: map[attribute.Key]string{attrHelmChartVersion: matrixVersion},
		},
		{
			operation: operationGetLatestValues,
			call: func() error {
				_, err := client.GetChartLatestValues(ctx, repoURL, matrixChart)

				return err
			},
			want: map[attribute.Key]string{attrHelmChartVersion: matrixVersion},
		},
		{
			operation: operationGetChartContents,
			call: func() error {
				_, err := client.GetChartContents(ctx, repoURL, matrixChart, matrixVersion, true, nil)

				return err
			},
			want: map[attribute.Key]string{attrHelmRecursive: "true"},
		},
		{
			operation: operationGetChartDependencies,
			call: func() error {
				_, err := client.GetChartDependencies(ctx, repoURL, matrixChart, matrixVersion)

				return err
			},
			want: map[attribute.Key]string{attrHelmDependencyCount: "0"},
		},
		{
			operation: operationGetChartImages,
			call: func() error {
				_, err := client.GetChartImages(ctx, repoURL, matrixChart, matrixVersion, nil, false)

				return err
			},
			want: map[attribute.Key]string{attrHelmImageCount: "0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.operation, func(t *testing.T) {
			if err := tt.call(); err != nil {
				t.Fatalf("%s: %v", tt.operation, err)
			}

			// The most recent one: get_latest_values runs get_latest_version
			// and get_chart_values underneath itself, so a name is not unique
			// across the whole table.
			span := latestSpan(t, recorder, "helm."+tt.operation)
			attrs := attribute.NewSet(span.Attributes()...)
			for key, want := range tt.want {
				value, found := attrs.Value(key)
				if !found {
					t.Errorf("span %q has no %q attribute, has %v", span.Name(), key, span.Attributes())

					continue
				}
				if value.String() != want {
					t.Errorf("span %q attribute %q = %q, want %q", span.Name(), key, value.String(), want)
				}
			}

			if got := span.Status().Code; got != codes.Unset {
				t.Errorf("span %q status = %v, want %v on success", span.Name(), got, codes.Unset)
			}

			wantPoint := attribute.NewSet(
				attribute.String(attrHelmOperation, tt.operation),
				attribute.String(attrHelmRepositoryType, repositoryTypeHTTP),
			)
			var found bool
			for _, point := range operationPoints(t, reader, tt.operation) {
				if point.Attributes.Equals(&wantPoint) {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s point with attributes %v",
					operationDurationInstrument, wantPoint.Encoded(attribute.DefaultEncoder()))
			}
		})
	}

	// The parse spans are only reached once a chart has actually been loaded,
	// so they have no coverage from the failing-path tests.
	for _, name := range []string{spanParseContents, spanParseImages} {
		var parents int
		for _, span := range recorder.Ended() {
			if span.Name() == name {
				parents++
				if !span.Parent().SpanID().IsValid() {
					t.Errorf("%s has no parent span", name)
				}
			}
		}
		if parents != 1 {
			t.Errorf("recorded %d %s spans, want 1", parents, name)
		}
	}

	// getRepo caches the repository and opens helm.repo.index only on a miss,
	// so the eight calls share one fetch. A span per call would report a cached
	// repository as a free fetch and skew the index latency towards zero.
	var indexSpans int
	for _, span := range recorder.Ended() {
		if span.Name() == spanRepoIndex {
			indexSpans++
		}
	}
	if indexSpans != 1 {
		t.Errorf("recorded %d %s spans across %d operations, want 1",
			indexSpans, spanRepoIndex, len(tests))
	}
}

func TestUserinfoOf(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "no credential", raw: "https://charts.example.com/stable", want: ""},
		{name: "url userinfo", raw: "https://user:pass@charts.example.com/stable", want: "user:pass@"},
		{name: "user only", raw: "https://user@charts.example.com", want: "user@"},
		{name: "oci url", raw: "oci://user:pass@registry.example.com/org/chart", want: "user:pass@"},
		{name: "bare oci reference", raw: "user:pass@registry.example.com/org/chart:1.0.0", want: "user:pass@"},
		{name: "at in the path only", raw: "https://charts.example.com/stable/a@b.tgz", want: ""},
		{name: "empty", raw: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userinfoOf(tt.raw); got != tt.want {
				t.Errorf("userinfoOf(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestRedactCredentialLeavesUnrelatedText covers the other half of endSpan's
// redaction: a message from an operation whose URL carried no credential, and a
// message that simply does not mention one, must reach the collector unchanged.
func TestRedactCredentialLeavesUnrelatedText(t *testing.T) {
	const msg = `invalid reference: invalid registry "registry.invalid"`

	if got := redactCredential(context.Background(), msg); got != msg {
		t.Errorf("redactCredential without a credential in context = %q, want %q", got, msg)
	}

	ctx := withCredential(context.Background(), "https://charts.example.com")
	if got := redactCredential(ctx, msg); got != msg {
		t.Errorf("redactCredential for a credential-free URL = %q, want %q", got, msg)
	}

	ctx = withCredential(context.Background(), "https://user:s3cret@charts.example.com")
	want := `failed to reach charts.example.com`
	if got := redactCredential(ctx, `failed to reach user:s3cret@charts.example.com`); got != want {
		t.Errorf("redactCredential = %q, want %q", got, want)
	}
}
