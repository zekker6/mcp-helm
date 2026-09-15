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
	if errors.Is(err, context.Canceled) {
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

// retryGetter retries transient failures of a helm getter. HTTPGetter accepts
// only an *http.Transport, so a retrying RoundTripper cannot be injected and
// each Get call is retried as a whole instead.
type retryGetter struct {
	getter.Getter
}

func (g *retryGetter) Get(href string, options ...getter.Option) (*bytes.Buffer, error) {
	for attempt := 0; ; attempt++ {
		buf, err := g.Getter.Get(href, options...)
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
		time.Sleep(wait)
	}
}

// getters returns helm's getter providers with HTTP(S) downloads wrapped in
// retryGetter.
func (c *HelmClient) getters() getter.Providers {
	providers := getter.All(c.settings)
	for i, p := range providers {
		if !p.Provides("http") && !p.Provides("https") {
			continue
		}
		newGetter := p.New
		providers[i].New = func(options ...getter.Option) (getter.Getter, error) {
			g, err := newGetter(options...)
			if err != nil {
				return nil, err
			}
			return &retryGetter{Getter: g}, nil
		}
	}
	return providers
}

// newRegistryHTTPClient builds the same client helm's registry package uses by
// default, with retryPolicy in place of retry.DefaultPolicy. The transport must
// stay a *retry.Transport over an *http.Transport so the registry client can
// still apply TLS settings to it.
func newRegistryHTTPClient() *http.Client {
	transport := registry.NewTransport(false)
	transport.Policy = func() retry.Policy { return retryPolicy }
	return &http.Client{Transport: transport}
}
