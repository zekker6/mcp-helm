package helm_client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/registry"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/zekker6/mcp-helm/lib/logger"
)

// retryPolicy keeps the backoff of retry.DefaultPolicy, which helm's registry
// client applies to OCI requests, but also retries connection-level failures
// (reset, refused, unexpected EOF) that DefaultPolicy returns immediately.
var retryPolicy retry.Policy = &retry.GenericPolicy{
	Retryable: isRetryable,
	Backoff:   retry.DefaultBackoff,
	MinWait:   200 * time.Millisecond,
	MaxWait:   3 * time.Second,
	MaxRetry:  5,
}

var transientErrors = []error{
	io.EOF,
	io.ErrUnexpectedEOF,
	syscall.ECONNRESET,
	syscall.ECONNREFUSED,
	syscall.ECONNABORTED,
	syscall.EPIPE,
	syscall.ETIMEDOUT,
	syscall.EHOSTUNREACH,
	syscall.ENETUNREACH,
}

func isRetryable(resp *http.Response, err error) (bool, error) {
	if err != nil {
		return isTransientNetError(err), nil
	}
	return isRetryableStatus(resp.StatusCode), nil
}

func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// isTransientNetError reports whether err is a network failure likely to
// succeed on retry. Connection errors are matched by errno rather than by
// *net.OpError, because TLS alerts (e.g. a rejected client certificate) are
// wrapped in *net.OpError too. Timeouts are retried only when raised by the
// network layer: an http.Client.Timeout means the whole request budget was
// already spent.
func isTransientNetError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return true
	}

	for _, target := range transientErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// getterStatusCode extracts the HTTP status code from an HTTPGetter error,
// which exposes it only as text: "failed to fetch <url> : <status>".
func getterStatusCode(err error) int {
	msg := err.Error()
	i := strings.LastIndex(msg, " : ")
	if !strings.HasPrefix(msg, "failed to fetch ") || i < 0 {
		return 0
	}
	code, _, _ := strings.Cut(msg[i+len(" : "):], " ")
	n, convErr := strconv.Atoi(code)
	if convErr != nil {
		return 0
	}
	return n
}

// retryGetter retries the complete download, including transient body-read
// failures. Size-limit errors are not transient and are returned immediately.
type retryGetter struct {
	getter.Getter
	ctx context.Context
}

func (g *retryGetter) Get(href string, options ...getter.Option) (*bytes.Buffer, error) {
	for attempt := 0; ; attempt++ {
		if err := g.ctx.Err(); err != nil {
			return nil, err
		}
		buf, err := g.Getter.Get(href, options...)
		if err := g.ctx.Err(); err != nil {
			return nil, err
		}
		if err == nil {
			return buf, nil
		}

		var resp *http.Response
		respErr := err
		if code := getterStatusCode(err); code != 0 {
			resp, respErr = &http.Response{StatusCode: code}, nil
		}

		wait, policyErr := retryPolicy.Retry(attempt, resp, respErr)
		if policyErr != nil || wait < 0 {
			return nil, err
		}

		logger.Warn("retrying chart repository request after transient failure",
			zap.Int("attempt", attempt+1),
			zap.Duration("backoff", wait),
			zap.Error(err),
		)
		timer := time.NewTimer(wait)
		select {
		case <-g.ctx.Done():
			timer.Stop()
			return nil, g.ctx.Err()
		case <-timer.C:
		}
	}
}

// requestContext carries ORAS auth scopes and the earliest deadline. RoundTrip
// combines cancellation from both parents for the lifetime of the response body.
type requestContext struct {
	context.Context
	request context.Context
}

func (c requestContext) Deadline() (time.Time, bool) {
	deadline, ok := c.Context.Deadline()
	if requestDeadline, requestOK := c.request.Deadline(); requestOK && (!ok || requestDeadline.Before(deadline)) {
		return requestDeadline, true
	}
	return deadline, ok
}

func (c requestContext) Value(key any) any {
	if value := c.request.Value(key); value != nil {
		return value
	}
	return c.Context.Value(key)
}

type operationTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t operationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(requestContext{Context: t.ctx, request: req.Context()})
	stop := context.AfterFunc(req.Context(), func() { cancel(context.Cause(req.Context())) })
	cleanup := func() {
		stop()
		cancel(context.Canceled)
	}
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil || resp.Body == nil || resp.Body == http.NoBody {
		cleanup()
		return resp, err
	}
	resp.Body = &operationBody{ReadCloser: resp.Body, cleanup: cleanup}
	return resp, nil
}

type operationBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *operationBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cleanup()
	}
	return n, err
}

func (b *operationBody) Close() error {
	defer b.cleanup()
	return b.ReadCloser.Close()
}

// newRegistryHTTPClient preserves Helm's retry transport. Pull clients count
// response bytes below retries so failed attempts also spend the pull budget.
func newRegistryHTTPClient(ctx context.Context, budget *byteBudget) (*http.Client, func()) {
	transport := registry.NewTransport(false)
	transport.Policy = func() retry.Policy { return retryPolicy }
	base := transport.Base
	closeIdle := func() {
		if closer, ok := base.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
	if budget != nil {
		transport.Base = &budgetTransport{base: base, budget: budget}
	}
	return &http.Client{Transport: operationTransport{ctx: ctx, base: transport}}, closeIdle
}
