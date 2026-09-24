package helm_client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"helm.sh/helm/v4/pkg/getter"
)

// byteBudget is shared by all responses in one pull, including concurrent
// blobs, authentication, redirects and retries. It counts actual body bytes.
type byteBudget struct {
	mu        sync.Mutex
	remaining int64
	limit     int64
	exceeded  bool
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{remaining: limit, limit: limit}
}

func (b *byteBudget) limitError() error {
	return fmt.Errorf("upstream download exceeds byte limit (%d bytes)", b.limit)
}

type budgetBody struct {
	io.ReadCloser
	budget *byteBudget
}

func (r *budgetBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exceeded {
		return 0, b.limitError()
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := r.ReadCloser.Read(probe[:])
		if n != 0 {
			b.exceeded = true
			return 0, b.limitError()
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

type budgetTransport struct {
	base   http.RoundTripper
	budget *byteBudget
}

func (t *budgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// HEAD describes content without transferring it.
	if req.Method == http.MethodHead {
		return resp, nil
	}
	t.budget.mu.Lock()
	tooLarge := t.budget.exceeded || resp.ContentLength > t.budget.remaining
	if tooLarge {
		t.budget.exceeded = true
	}
	t.budget.mu.Unlock()
	if tooLarge {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("%w: closing response: %v", t.budget.limitError(), err)
		}
		return nil, t.budget.limitError()
	}
	resp.Body = &budgetBody{ReadCloser: resp.Body, budget: t.budget}
	return resp, nil
}

// httpDownloadGetter is used only by this client's repository downloads. Its
// configuration comes from clientOptions rather than Helm's opaque getter options.
type httpDownloadGetter struct {
	ctx     context.Context
	client  *http.Client
	baseURL *url.URL
	options *clientOptions
	limit   int64
}

func (c *HelmClient) newHTTPGetter(ctx context.Context, baseURL string, limit int64) (getter.Getter, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	opts := c.options
	if opts == nil {
		opts = defaultClientOptions()
	}
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		// An opaque wrapper cannot accept the repository's TLS settings.
		transport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	transport.DisableCompression = true
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.insecureSkipTLSVerify} //nolint:gosec // Explicit user option.
	if opts.caFile != "" {
		pem, err := os.ReadFile(opts.caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in CA file %q", opts.caFile)
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	if opts.certFile != "" && opts.keyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.certFile, opts.keyFile)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
	}
	client := &http.Client{Transport: transport, Timeout: getter.DefaultHTTPTimeout * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		// Go forwards Basic Auth to the same hostname even when the scheme or
		// port changes. Scope every hop to the original repository origin.
		req.Header.Del("Authorization")
		setHTTPRepositoryAuth(req, base, opts)
		return nil
	}
	return &retryGetter{Getter: &httpDownloadGetter{
		ctx: ctx, client: client, baseURL: base, options: opts, limit: limit,
	}, ctx: ctx}, nil
}

func setHTTPRepositoryAuth(req *http.Request, base *url.URL, opts *clientOptions) {
	if opts.username != "" && opts.password != "" &&
		(opts.passCredentialsAll || (req.URL.Scheme == base.Scheme && req.URL.Host == base.Host)) {
		req.SetBasicAuth(opts.username, opts.password)
	}
}

func (g *httpDownloadGetter) Get(href string, _ ...getter.Option) (result *bytes.Buffer, err error) {
	defer g.client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(g.ctx, http.MethodGet, href, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mcp-helm")
	req.Header.Set("Accept", "application/gzip,application/octet-stream")
	setHTTPRepositoryAuth(req, g.baseURL, g.options)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			result, err = nil, closeErr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch %s : %s", href, resp.Status)
	}
	budget := newByteBudget(g.limit)
	if resp.ContentLength > g.limit {
		return nil, budget.limitError()
	}
	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, &budgetBody{ReadCloser: resp.Body, budget: budget}); err != nil {
		return nil, err
	}
	return buf, nil
}
