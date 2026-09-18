package helm_client

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// scopeName is the scope for both the tracer and the meter. Both come from
	// the OTel globals rather than a ClientOption: the globals are no-ops until
	// telemetry.Setup registers real providers, so an uninstrumented binary
	// pays nothing and no call site branches on whether telemetry is enabled.
	scopeName = "helm_client"

	// attrNamespace prefixes every name this package defines. Nothing here has
	// a registry entry, and unprefixed names are reserved for the
	// specification, so custom names carry the owner's reverse domain.
	attrNamespace = "cloud.zekker.helm."

	operationDurationInstrument = attrNamespace + "operation.duration"
)

const (
	attrHelmOperation      = attrNamespace + "operation"
	attrHelmRepositoryType = attrNamespace + "repository.type"
	attrHelmRepositoryURL  = attrNamespace + "repository.url"
	attrHelmChartName      = attrNamespace + "chart.name"
	attrHelmChartVersion   = attrNamespace + "chart.version"
	attrHelmRecursive      = attrNamespace + "recursive"
	attrHelmOCIRef         = attrNamespace + "oci.ref"
)

// Result counts, span-only: the number of charts a repository holds is
// unbounded and would give the histogram one series per repository size.
const (
	attrHelmChartCount      = attrNamespace + "chart.count"
	attrHelmVersionCount    = attrNamespace + "version.count"
	attrHelmDependencyCount = attrNamespace + "dependency.count"
	attrHelmImageCount      = attrNamespace + "image.count"
)

// One per exported method. The value is what the operation metric attribute
// carries, and prefixed with "helm." it is the span name.
const (
	operationListCharts           = "list_charts"
	operationListChartVersions    = "list_chart_versions"
	operationGetLatestVersion     = "get_latest_version"
	operationGetLatestValues      = "get_latest_values"
	operationGetChartValues       = "get_chart_values"
	operationGetChartContents     = "get_chart_contents"
	operationGetChartDependencies = "get_chart_dependencies"
	operationGetChartImages       = "get_chart_images"
)

// Inner spans, named in full unlike the operations above: none records a
// histogram point, so there is no metric value to double as a span name.
const (
	spanLoadChart     = "helm.load_chart"
	spanOCIPull       = "helm.oci.pull"
	spanOCITags       = "helm.oci.tags"
	spanRepoIndex     = "helm.repo.index"
	spanChartDownload = "helm.chart.download"
	spanParseImages   = "helm.parse.images"
	spanParseContents = "helm.parse.contents"
)

const (
	repositoryTypeOCI  = "oci"
	repositoryTypeHTTP = "http"
)

// Built at package init against the global delegate, since telemetry.Setup only
// runs once run() is under way. The delegate rewires its instruments when the
// real provider is registered, and never rejects one, so the error is dropped.
var operationDuration, _ = otel.GetMeterProvider().Meter(scopeName).Float64Histogram(
	operationDurationInstrument,
	metric.WithUnit("s"),
	metric.WithDescription("Duration of Helm repository and chart operations."),
)

// startSpan reads the global provider per call rather than caching it: the
// lookup is trivial next to the I/O these spans wrap, and it stays correct when
// the global provider is replaced more than once, which is what tests do.
func startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.GetTracerProvider().Tracer(scopeName).Start(ctx, name, opts...)
}

// startInternalSpan opens a span around work this process does itself.
func startInternalSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return startSpan(ctx, name, trace.WithAttributes(attrs...))
}

// startClientSpan opens a span around a call that leaves the process: an OCI
// registry request, a repository index fetch or a chart download.
func startClientSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return startSpan(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
}

// endSpan marks span failed when err is non-nil and ends it. Inner spans record
// no histogram point; the exported method they run under records one for the
// whole call.
//
// ctx must descend from startOperation, whose credential it uses to redact the
// exported message. The error returned to the caller is untouched.
func endSpan(ctx context.Context, span trace.Span, err error) {
	if err != nil {
		// Derived before the redaction below can replace err: the substitute is
		// a plain errors.errorString and would report that as the type.
		errorType := semconv.ErrorType(err)

		if msg := redactCredential(ctx, err.Error()); msg != err.Error() {
			// Only substituted when the message actually carried the
			// credential, so exception.type stays the real error type for
			// every other failure.
			err = errors.New(msg)
		}

		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(errorType)
		span.RecordError(err)
	}

	span.End()
}

// credentialKey carries the userinfo of the repository URL an operation was
// started with.
type credentialKey struct{}

// withCredential remembers the credential embedded in repoURL so every span
// under the operation can keep it out of what it exports. sanitizeURL covers
// only the values this package formats; Helm's own errors quote the reference
// verbatim and reach the collector through the span status and exception event.
func withCredential(ctx context.Context, repoURL string) context.Context {
	userinfo := userinfoOf(repoURL)
	if userinfo == "" {
		return ctx
	}

	return context.WithValue(ctx, credentialKey{}, userinfo)
}

// redactCredential removes the operation's credential from msg.
func redactCredential(ctx context.Context, msg string) string {
	userinfo, ok := ctx.Value(credentialKey{}).(string)
	if !ok {
		return msg
	}

	return strings.ReplaceAll(msg, userinfo, "")
}

// userinfoOf returns the "user[:password]@" prefix embedded in raw. It works on
// the raw text rather than through url.Parse so the result is a literal
// substring of every reference derived from raw, making redaction a replace.
func userinfoOf(raw string) string {
	_, authority := splitScheme(raw)

	first, _, _ := strings.Cut(authority, "/")
	if at := strings.LastIndex(first, "@"); at != -1 {
		return first[:at+1]
	}

	return ""
}

// splitScheme separates a "scheme://" prefix from the rest of raw. A reference
// carrying none - which is what parseOCIReference produces - reports an empty
// scheme and is returned unchanged.
func splitScheme(raw string) (scheme, rest string) {
	if i := strings.Index(raw, "://"); i != -1 {
		return raw[:i], raw[i+len("://"):]
	}

	return "", raw
}

// serverAttributes names the host a client span talks to, from either a
// repository URL or a bare OCI reference. server.port is reported only when
// known: explicit in the authority, or implied by the scheme. A bare reference
// has neither, and a registry behind one may not be on 443.
func serverAttributes(raw string) []attribute.KeyValue {
	scheme, authority := splitScheme(raw)
	authority, _, _ = strings.Cut(authority, "/")
	if at := strings.LastIndex(authority, "@"); at != -1 {
		authority = authority[at+1:]
	}

	host, port := splitHostPort(authority)
	if host == "" {
		return nil
	}

	attrs := []attribute.KeyValue{semconv.ServerAddress(host)}
	if port == 0 {
		port = defaultPort(scheme)
	}
	if port != 0 {
		attrs = append(attrs, semconv.ServerPort(port))
	}

	return attrs
}

// splitHostPort separates an authority into its host and port, reporting a zero
// port when it carries none. An IPv6 literal loses the brackets that only exist
// to delimit it from the port.
func splitHostPort(authority string) (string, int) {
	host, portText, err := net.SplitHostPort(authority)
	if err != nil {
		return strings.Trim(authority, "[]"), 0
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return host, 0
	}

	return host, port
}

func defaultPort(scheme string) int {
	switch scheme {
	case "http":
		return 80
	case "https", "oci":
		return 443
	default:
		return 0
	}
}

// chartDownloadAttributes describes the .tgz fetch. The chart URL is the exact
// resource downloaded, so it is reported as url.full rather than under this
// package's own namespace.
func chartDownloadAttributes(chartURL string) []attribute.KeyValue {
	return append(serverAttributes(chartURL), semconv.URLFull(sanitizeURL(chartURL)))
}

// repositoryAttributes describes an index fetch. The repository URL is not
// url.full: the Helm getter resolves the index path itself, so the URL actually
// requested is not the one this package holds.
func repositoryAttributes(repoURL string) []attribute.KeyValue {
	return append(serverAttributes(repoURL), attribute.String(attrHelmRepositoryURL, sanitizeURL(repoURL)))
}

// ociRefAttributes describes a registry call. The host comes from the
// repository URL, which still carries the oci:// scheme the reference has had
// stripped.
func ociRefAttributes(repoURL, ref string) []attribute.KeyValue {
	return append(serverAttributes(repoURL), attribute.String(attrHelmOCIRef, sanitizeURL(ref)))
}

// recordOperation records one exported method call. Repository URLs, chart
// names and versions stay on the span; on the instrument they would blow up its
// cardinality.
func recordOperation(ctx context.Context, operation, repositoryType string, start time.Time, err error) {
	attrs := append([]attribute.KeyValue{
		attribute.String(attrHelmOperation, operation),
		attribute.String(attrHelmRepositoryType, repositoryType),
	}, errorAttributes(err)...)

	operationDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

// errorAttributes describes a failure, and nothing at all on success. The
// conventions make the absence of error.type the success signal, which also
// keeps a constant dimension off every successful series.
func errorAttributes(err error) []attribute.KeyValue {
	if err == nil {
		return nil
	}

	return []attribute.KeyValue{semconv.ErrorType(err)}
}

// repositoryType reports which of the two loaders a repository URL selects.
func repositoryType(repoURL string) string {
	if IsOCI(repoURL) {
		return repositoryTypeOCI
	}

	return repositoryTypeHTTP
}

// sanitizeURL strips user:pass@ userinfo so a URL can be exported. It also
// guards this package's error strings: endSpan copies the message into the span
// status and the exception event, so an interpolated URL reaches the collector
// on the same path as an attribute.
func sanitizeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		// Unparseable. Everything before an @ may be a credential, so drop it.
		if i := strings.LastIndex(raw, "@"); i != -1 {
			return raw[i+1:]
		}

		return raw
	}

	if parsed.User != nil {
		parsed.User = nil

		return parsed.String()
	}

	// url.Parse only fills User for a URL with a // authority, so a bare OCI
	// reference keeps its credentials verbatim.
	if parsed.Host == "" {
		return stripLeadingUserinfo(raw)
	}

	return raw
}

// stripLeadingUserinfo drops a user[:password]@ prefix from the first segment
// of an authority-less reference. Later segments are left alone: an @ in a path
// is not a credential.
func stripLeadingUserinfo(raw string) string {
	first, rest, hasRest := strings.Cut(raw, "/")

	at := strings.LastIndex(first, "@")
	if at == -1 {
		return raw
	}

	first = first[at+1:]
	if !hasRest {
		return first
	}

	return first + "/" + rest
}

// operation ties an exported method's span to its histogram point, so a method
// cannot mark its span failed while its metric reports success.
type operation struct {
	span           trace.Span
	name           string
	repositoryType string
	start          time.Time
}

// startOperation opens the span for an exported method and returns the context
// carrying it. attrs carries whatever else the method knows up front.
func startOperation(ctx context.Context, name, repoURL string, attrs ...attribute.KeyValue) (context.Context, *operation) {
	repoType := repositoryType(repoURL)

	ctx = withCredential(ctx, repoURL)

	ctx, span := startInternalSpan(ctx, "helm."+name, append(
		[]attribute.KeyValue{
			attribute.String(attrHelmRepositoryURL, sanitizeURL(repoURL)),
			attribute.String(attrHelmRepositoryType, repoType),
		},
		attrs...,
	)...)

	return ctx, &operation{
		span:           span,
		name:           name,
		repositoryType: repoType,
		start:          time.Now(),
	}
}

// setAttributes adds attributes only known once the operation has run, such as
// the result counts or a resolved chart version.
func (o *operation) setAttributes(attrs ...attribute.KeyValue) {
	o.span.SetAttributes(attrs...)
}

// end records the histogram point, marks the span on failure and ends it. Call
// it from a deferred closure over the method's named error result so every
// return path reports the same error to both signals. ctx must be the
// span-carrying context from startOperation, so the point can carry an exemplar.
func (o *operation) end(ctx context.Context, err error) {
	recordOperation(ctx, o.name, o.repositoryType, o.start, err)
	endSpan(ctx, o.span, err)
}
