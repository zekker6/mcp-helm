package helm_client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/repo/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/zekker6/mcp-helm/lib/helm_parser"
	"github.com/zekker6/mcp-helm/lib/logger"
)

var tmpDir = "/tmp/helm_cache"

// DefaultRepoIndexMaxAge is how long a downloaded HTTP repository index is
// reused before it is downloaded again.
const DefaultRepoIndexMaxAge = 1 * time.Hour

type ClientOption func(*clientOptions)

type clientOptions struct {
	// OCI registry options
	credentialsFile string
	plainHTTP       bool

	// Shared auth options (used for both OCI and HTTP repos)
	username string
	password string

	// HTTP repository TLS options
	certFile              string
	keyFile               string
	caFile                string
	insecureSkipTLSVerify bool
	passCredentialsAll    bool

	// HTTP repository index cache
	repoIndexMaxAge time.Duration
}

// WithCredentialsFile sets the path to a Docker-style credentials file for OCI registries.
func WithCredentialsFile(path string) ClientOption {
	return func(o *clientOptions) {
		o.credentialsFile = path
	}
}

// WithBasicAuth sets username/password for authentication.
// Works for both OCI registries and HTTP repositories.
func WithBasicAuth(username, password string) ClientOption {
	return func(o *clientOptions) {
		o.username = username
		o.password = password
	}
}

// WithPlainHTTP enables plain HTTP (no TLS) for OCI registry connections.
func WithPlainHTTP(enabled bool) ClientOption {
	return func(o *clientOptions) {
		o.plainHTTP = enabled
	}
}

// WithTLSClientConfig sets client certificate and key for mTLS authentication with HTTP repositories.
func WithTLSClientConfig(certFile, keyFile string) ClientOption {
	return func(o *clientOptions) {
		o.certFile = certFile
		o.keyFile = keyFile
	}
}

// WithCAFile sets the CA certificate file for verifying HTTP repository server certificates.
func WithCAFile(caFile string) ClientOption {
	return func(o *clientOptions) {
		o.caFile = caFile
	}
}

// WithInsecureSkipTLSVerify disables TLS certificate verification for HTTP repositories.
func WithInsecureSkipTLSVerify(skip bool) ClientOption {
	return func(o *clientOptions) {
		o.insecureSkipTLSVerify = skip
	}
}

// WithPassCredentialsAll enables passing credentials to all domains when following redirects.
func WithPassCredentialsAll(pass bool) ClientOption {
	return func(o *clientOptions) {
		o.passCredentialsAll = pass
	}
}

// WithRepoIndexMaxAge sets how long a downloaded HTTP repository index is reused
// before it is downloaded again. Zero downloads the index on every request.
func WithRepoIndexMaxAge(maxAge time.Duration) ClientOption {
	return func(o *clientOptions) {
		o.repoIndexMaxAge = maxAge
	}
}

type cachedRepo struct {
	repo    *repo.ChartRepository
	fetched time.Time
}

type HelmClient struct {
	settings *cli.EnvSettings

	// registryClient is the default OCI registry client. When basic auth is
	// configured (and no explicit credentials file resolves a credential for
	// the target host) it carries those static credentials.
	registryClient *registry.Client
	// registryClientCreds is non-nil only when both basic auth and an explicit
	// credentials file are configured. It is used for OCI hosts the credentials
	// file resolves a credential for, so docker-config lookups take precedence
	// over static basic auth.
	registryClientCreds *registry.Client
	// credStore resolves credentials from the explicit credentials file using
	// the same store and semantics as the OCI registry client. Non-nil only in
	// the combined basic-auth + credentials-file mode; used to route per host.
	credStore credentials.Store

	routeMu    sync.Mutex
	routeCache map[string]*registry.Client

	options *clientOptions

	reposMu sync.Mutex
	repos   map[string]*cachedRepo
}

// NewClient creates a new HelmClient with optional configuration.
// It supports both HTTP Helm repositories and OCI registries.
//
// Authentication for OCI registries can be configured via:
//   - WithCredentialsFile: path to a Docker-style credentials file (per-host lookup)
//   - WithBasicAuth: static username/password
//
// When both are provided, OCI requests are routed per host: registries the
// credentials file resolves a credential for use the credentials-file client,
// and every other registry falls back to the static basic-auth client. HTTP
// repositories always use the basic-auth credentials (scoped per repository in
// getRepo).
func NewClient(opts ...ClientOption) (*HelmClient, error) {
	options := &clientOptions{repoIndexMaxAge: DefaultRepoIndexMaxAge}
	for _, opt := range opts {
		opt(options)
	}

	settings := cli.New()
	settings.RepositoryCache = path.Join(tmpDir, "helm-cache")
	settings.RegistryConfig = path.Join(tmpDir, "helm-registry.conf")
	settings.RepositoryConfig = path.Join(tmpDir, "helm-repository.conf")

	baseOpts := []registry.ClientOption{
		registry.ClientOptEnableCache(true),
		registry.ClientOptHTTPClient(newRegistryHTTPClient()),
	}
	if options.plainHTTP {
		baseOpts = append(baseOpts, registry.ClientOptPlainHTTP())
	}

	hasBasicAuth := options.username != "" && options.password != ""
	hasCredsFile := options.credentialsFile != ""

	client := &HelmClient{
		settings: settings,
		options:  options,
	}

	if hasBasicAuth && hasCredsFile {
		// Both auth methods configured: route OCI requests per host. A host is
		// resolved against the credentials file using the same store the
		// registry client uses; if the file yields a credential the
		// credentials-file client is used, otherwise the request falls back to
		// the static basic-auth client.
		credStore, err := newCredStore(options.credentialsFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load registry credentials file %q: %w", options.credentialsFile, err)
		}

		credsClient, err := registry.NewClient(withRegistryOpts(baseOpts,
			registry.ClientOptCredentialsFile(options.credentialsFile))...)
		if err != nil {
			return nil, fmt.Errorf("failed to create OCI registry client (credentials file): %w", err)
		}

		basicClient, err := registry.NewClient(withRegistryOpts(baseOpts,
			registry.ClientOptCredentialsFile(settings.RegistryConfig),
			registry.ClientOptBasicAuth(options.username, options.password))...)
		if err != nil {
			return nil, fmt.Errorf("failed to create OCI registry client (basic auth): %w", err)
		}

		client.registryClient = basicClient
		client.registryClientCreds = credsClient
		client.credStore = credStore
		client.routeCache = make(map[string]*registry.Client)

		// Credentials resolved through an external credential store/helper can
		// only be detected at runtime by invoking that helper. Warn so the
		// silent fallback to basic auth is visible if the helper is unavailable.
		if store, helpers := externalCredHelpers(options.credentialsFile); store != "" || len(helpers) > 0 {
			logger.Warn("registry credentials file relies on external credential helpers; affected OCI registries require the helper binary at runtime, otherwise they fall back to -username/-password basic auth",
				zap.String("credentialsFile", options.credentialsFile),
				zap.String("credsStore", store),
				zap.Strings("credHelpers", helpers),
			)
		}

		return client, nil
	}

	// Single client: explicit credentials file if provided, otherwise the
	// default registry config, optionally augmented with static basic auth.
	credsFile := settings.RegistryConfig
	if hasCredsFile {
		credsFile = options.credentialsFile
	}
	regOpts := withRegistryOpts(baseOpts, registry.ClientOptCredentialsFile(credsFile))
	if hasBasicAuth {
		regOpts = append(regOpts, registry.ClientOptBasicAuth(options.username, options.password))
	}

	regClient, err := registry.NewClient(regOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %w", err)
	}
	client.registryClient = regClient

	return client, nil
}

// withRegistryOpts returns a fresh slice combining base and extra registry
// client options without mutating base's backing array.
func withRegistryOpts(base []registry.ClientOption, extra ...registry.ClientOption) []registry.ClientOption {
	out := make([]registry.ClientOption, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// registryClientFor returns the OCI registry client to use for repoURL. When
// both basic auth and an explicit credentials file are configured, a host that
// resolves to a credential in the credentials file uses the credentials-file
// client; everything else uses the basic-auth client. The resolution mirrors
// the registry client's own credential lookup (oras-go store + Docker Hub key
// mapping), so the routing decision matches what the chosen client will use.
func (c *HelmClient) registryClientFor(ctx context.Context, repoURL string) *registry.Client {
	if c.credStore == nil {
		return c.registryClient
	}

	host := ociRegistryHost(repoURL)

	c.routeMu.Lock()
	cl, ok := c.routeCache[host]
	c.routeMu.Unlock()
	if ok {
		return cl
	}

	// Resolve outside the lock: for a credsStore/credHelpers config this execs
	// an external helper binary, which must not block routing for other hosts.
	// A concurrent first lookup of the same host may resolve twice, but the
	// result is deterministic, so caching either is safe.
	cl = c.registryClient
	// Same key derivation helm's registry client uses for credential lookup.
	key := credentials.ServerAddressFromHostname(credentials.ServerAddressFromRegistry(host))
	if cred, err := c.credStore.Get(ctx, key); err == nil && cred != auth.EmptyCredential {
		cl = c.registryClientCreds
	}

	c.routeMu.Lock()
	c.routeCache[host] = cl
	c.routeMu.Unlock()
	return cl
}

// ociTags lists the tags of an OCI chart reference. Both callers wrap the same
// SDK call in the same span, so the span lives here rather than at either.
func (c *HelmClient) ociTags(ctx context.Context, repoURL, ref string) (tags []string, err error) {
	ctx, span := startClientSpan(ctx, spanOCITags, ociRefAttributes(repoURL, ref)...)
	defer func() { endSpan(ctx, span, err) }()

	return c.registryClientFor(ctx, repoURL).Tags(ref)
}

// ociPull pulls a chart from an OCI registry.
func (c *HelmClient) ociPull(ctx context.Context, repoURL, ref string) (result *registry.PullResult, err error) {
	ctx, span := startClientSpan(ctx, spanOCIPull, ociRefAttributes(repoURL, ref)...)
	defer func() { endSpan(ctx, span, err) }()

	return c.registryClientFor(ctx, repoURL).Pull(ref, registry.PullOptWithChart(true))
}

// newCredStore builds an oras-go credentials store from a Docker-style
// credentials file, using the same store and key semantics as the
// credentials-file registry client. Only the provided file is consulted (no
// ~/.docker fallback and no native-store auto-detection): routing stays
// deterministic and scoped to the credentials the user explicitly supplied, and
// the store it resolves is a subset of what that registry client resolves, so a
// host routed to the credentials-file client is guaranteed to resolve there.
// credsStore/credHelpers declared in the file are still honored.
func newCredStore(path string) (credentials.Store, error) {
	// Fail fast on a missing/unreadable file: the store loader silently treats
	// a missing file as empty, which would hide a misconfigured path.
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return credentials.NewStore(path, credentials.StoreOptions{})
}

// externalCredHelpers reports the credential store and the registry hosts that have
// per-registry credential helpers declared in a Docker-style credentials file.
// Credentials behind these can only be resolved by invoking the helper binary at runtime.
func externalCredHelpers(path string) (credsStore string, credHelpers []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}

	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", nil
	}

	for host := range cfg.CredHelpers {
		credHelpers = append(credHelpers, host)
	}
	sort.Strings(credHelpers)
	return cfg.CredsStore, credHelpers
}

// ociRegistryHost extracts the registry host from an OCI URL,
// e.g. "oci://registry.example.com/org/chart" -> "registry.example.com".
func ociRegistryHost(repoURL string) string {
	ref := strings.TrimPrefix(repoURL, "oci://")
	if i := strings.IndexByte(ref, '/'); i != -1 {
		ref = ref[:i]
	}
	return ref
}

func IsOCI(url string) bool {
	return registry.IsOCI(url)
}

func parseOCIReference(repoURL, chartName, version string) string {
	ref := strings.TrimPrefix(repoURL, "oci://")

	// Remove any existing tag from ref for comparison
	refWithoutTag := ref
	if idx := strings.Index(ref, ":"); idx != -1 {
		refWithoutTag = ref[:idx]
	}

	// If chartName is provided, only append if not already at the end of the URL
	if chartName != "" {
		refWithoutTag = strings.TrimSuffix(refWithoutTag, "/")
		// Check if the URL already ends with the chart name
		if !strings.HasSuffix(refWithoutTag, "/"+chartName) && refWithoutTag != chartName {
			ref = refWithoutTag + "/" + chartName
		} else {
			ref = refWithoutTag
		}
	}

	// Append version/tag if provided
	if version != "" {
		ref = ref + ":" + version
	}

	return ref
}

func ExtractChartNameFromOCI(repoURL string) string {
	ref := strings.TrimPrefix(repoURL, "oci://")
	parts := strings.Split(ref, "/")
	if len(parts) > 0 {
		// The last part is the chart name (may include :tag)
		chartName := parts[len(parts)-1]
		// Remove any tag suffix
		if idx := strings.Index(chartName, ":"); idx != -1 {
			chartName = chartName[:idx]
		}
		return chartName
	}
	return ""
}

func (c *HelmClient) getRepo(ctx context.Context, name, url string) (chartRepo *repo.ChartRepository, err error) {
	c.reposMu.Lock()
	defer c.reposMu.Unlock()

	if c.repos == nil {
		c.repos = make(map[string]*cachedRepo)
	}

	// An expired entry is replaced with a new ChartRepository rather than
	// updated in place, so callers still reading the previous IndexFile keep a
	// consistent snapshot.
	if v, exists := c.repos[name]; exists && c.options != nil && time.Since(v.fetched) < c.options.repoIndexMaxAge {
		return v.repo, nil
	}

	// Opened after the cache check: a served-from-cache repository does no
	// index fetch, and a span for it would report the fetch as free.
	_, span := startClientSpan(ctx, spanRepoIndex, repositoryAttributes(url)...)
	defer func() { endSpan(ctx, span, err) }()

	entry := &repo.Entry{
		Name: name,
		URL:  url,
	}

	// Apply authentication options if configured
	if c.options != nil {
		entry.Username = c.options.username
		entry.Password = c.options.password
		entry.CertFile = c.options.certFile
		entry.KeyFile = c.options.keyFile
		entry.CAFile = c.options.caFile
		entry.InsecureSkipTLSVerify = c.options.insecureSkipTLSVerify
		entry.PassCredentialsAll = c.options.passCredentialsAll
	}

	chartRepo, err = repo.NewChartRepository(entry, c.getters())
	if err != nil {
		return nil, fmt.Errorf("failed to create chart repository: %v", err)
	}

	indexFileLocation, err := chartRepo.DownloadIndexFile()
	if err != nil {
		return nil, fmt.Errorf("failed to download repository index: %v", err)
	}

	file, err := repo.LoadIndexFile(indexFileLocation)
	if err != nil {
		return nil, fmt.Errorf("failed to load index file: %v", err)
	}
	chartRepo.IndexFile = file
	chartRepo.IndexFile.SortEntries()

	c.repos[name] = &cachedRepo{repo: chartRepo, fetched: time.Now()}
	return chartRepo, nil
}

func (c *HelmClient) ListCharts(ctx context.Context, repoURL string) (names []string, err error) {
	ctx, op := startOperation(ctx, operationListCharts, repoURL)
	defer func() { op.end(ctx, err) }()

	if IsOCI(repoURL) {
		// For OCI, each repository contains a single chart
		// Return the chart name extracted from the URL
		chartName := ExtractChartNameFromOCI(repoURL)
		if chartName == "" {
			return nil, fmt.Errorf("invalid OCI reference: cannot extract chart name from %s", repoURL)
		}

		op.setAttributes(attribute.Int(attrHelmChartCount, 1))

		return []string{chartName}, nil
	}

	helmRepo, err := c.getRepo(ctx, repoURL, repoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to add repository: %v", err)
	}

	charts := make(map[string]bool)
	for _, entry := range helmRepo.IndexFile.Entries {
		for _, version := range entry {
			if !charts[version.Name] {
				charts[version.Name] = true
				break
			}
		}
	}

	chartsList := make([]string, 0, len(charts))
	for chart := range charts {
		chartsList = append(chartsList, chart)
	}
	sort.Strings(chartsList)

	op.setAttributes(attribute.Int(attrHelmChartCount, len(chartsList)))

	return chartsList, nil
}

func (c *HelmClient) ListChartVersions(ctx context.Context, repoURL string, chart string) (versions []string, err error) {
	ctx, op := startOperation(ctx, operationListChartVersions, repoURL,
		attribute.String(attrHelmChartName, chart))
	defer func() { op.end(ctx, err) }()

	if IsOCI(repoURL) {
		ref := parseOCIReference(repoURL, chart, "")
		tags, err := c.ociTags(ctx, repoURL, ref)
		if err != nil {
			return nil, fmt.Errorf("failed to list tags for OCI chart %s: %v", ref, err)
		}

		op.setAttributes(attribute.Int(attrHelmVersionCount, len(tags)))

		// Tags are already sorted in descending semver order by Helm's registry package
		return tags, nil
	}

	helmRepo, err := c.getRepo(ctx, repoURL, repoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to add repository: %v", err)
	}

	versions = make([]string, 0)
	for k, v := range helmRepo.IndexFile.Entries {
		if k != chart {
			continue
		}

		for _, ver := range v {
			versions = append(versions, ver.Version)
		}
	}
	// Do not sort version as those were sorted in original index file

	op.setAttributes(attribute.Int(attrHelmVersionCount, len(versions)))

	return versions, nil
}

func (c *HelmClient) GetChartValues(ctx context.Context, repoURL, chartName, version string) (values string, err error) {
	ctx, op := startOperation(ctx, operationGetChartValues, repoURL,
		attribute.String(attrHelmChartName, chartName),
		attribute.String(attrHelmChartVersion, version))
	defer func() { op.end(ctx, err) }()

	loadedChart, err := c.loadChart(ctx, repoURL, chartName, version)
	if err != nil {
		return "", fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	var rawContent []byte
	for _, file := range loadedChart.Raw {
		if file.Name != "values.yaml" {
			continue
		}
		rawContent = file.Data
		break
	}

	return string(rawContent), nil
}

func (c *HelmClient) GetChartContents(ctx context.Context, repoURL, chartName, version string, recursive bool, paths []string) (contents string, err error) {
	ctx, op := startOperation(ctx, operationGetChartContents, repoURL,
		attribute.String(attrHelmChartName, chartName),
		attribute.String(attrHelmChartVersion, version),
		attribute.Bool(attrHelmRecursive, recursive))
	defer func() { op.end(ctx, err) }()

	loadedChart, err := c.loadChart(ctx, repoURL, chartName, version)
	if err != nil {
		return "", fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return "", fmt.Errorf("chart %s version %s not found", chartName, version)
	}

	_, parseSpan := startInternalSpan(ctx, spanParseContents, attribute.Bool(attrHelmRecursive, recursive))
	contents, err = helm_parser.GetChartContents(loadedChart, recursive, paths)
	endSpan(ctx, parseSpan, err)
	if err != nil {
		return "", fmt.Errorf("failed to get chart contents for %s version %s: %v", chartName, version, err)
	}
	return contents, nil
}

// loadChart fetches a chart through whichever of the two loaders the repository
// URL selects. Its span exists to record that branch as helm.repository.type.
func (c *HelmClient) loadChart(ctx context.Context, repoURL string, chartName string, version string) (chart *chartv2.Chart, err error) {
	repoType := repositoryType(repoURL)

	ctx, span := startInternalSpan(ctx, spanLoadChart,
		attribute.String(attrHelmRepositoryType, repoType),
		attribute.String(attrHelmChartName, chartName),
		attribute.String(attrHelmChartVersion, version))
	defer func() { endSpan(ctx, span, err) }()

	if repoType == repositoryTypeOCI {
		return c.loadChartFromOCI(ctx, repoURL, chartName, version)
	}

	return c.loadChartFromHTTP(ctx, repoURL, chartName, version)
}

func (c *HelmClient) loadChartFromOCI(ctx context.Context, repoURL, chartName, version string) (*chartv2.Chart, error) {
	ref := parseOCIReference(repoURL, chartName, version)

	result, err := c.ociPull(ctx, repoURL, ref)
	if err != nil {
		return nil, fmt.Errorf("failed to pull OCI chart %s: %v", ref, err)
	}

	if result.Chart == nil || len(result.Chart.Data) == 0 {
		return nil, fmt.Errorf("no chart data returned for OCI chart %s", ref)
	}

	loadedChart, err := loader.LoadArchive(bytes.NewReader(result.Chart.Data))
	if err != nil {
		return nil, fmt.Errorf("failed to load OCI chart archive %s: %v", ref, err)
	}

	v2Chart, ok := loadedChart.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("charts V3 format is not supported for OCI chart %s", ref)
	}

	return v2Chart, nil
}

func (c *HelmClient) loadChartFromHTTP(ctx context.Context, repoURL, chartName, version string) (*chartv2.Chart, error) {
	// TODO: implement caching for values file
	helmRepo, err := c.getRepo(ctx, repoURL, repoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository: %v", err)
	}

	var cv *repo.ChartVersion
	for k, v := range helmRepo.IndexFile.Entries {
		if k != chartName {
			continue
		}
		for _, ver := range v {
			if ver.Version != version {
				continue
			}
			cv = ver
			break
		}
		if cv != nil {
			break
		}
	}
	if cv == nil {
		return nil, fmt.Errorf("failed to find chart %s version %s", chartName, version)
	}

	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("no download URLs found for chart %s version %s", chartName, version)
	}

	chartURL := cv.URLs[0]
	if !strings.HasPrefix(chartURL, "http://") && !strings.HasPrefix(chartURL, "https://") {
		repoBaseURL := strings.TrimSuffix(helmRepo.Config.URL, "/")
		chartURL = fmt.Sprintf("%s/%s", repoBaseURL, strings.TrimPrefix(chartURL, "/"))
	}

	tempDir, err := os.MkdirTemp("", "helm-chart-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	chartPath := filepath.Join(tempDir, fmt.Sprintf("%s-%s", chartName, version))
	_ = os.MkdirAll(chartPath, 0o755)

	// Forward the same auth options getRepo applies to the index download.
	// The Helm SDK's ChartDownloader does not auto-discover credentials from
	// the repo.Entry, so they must be passed explicitly here; otherwise the
	// .tgz fetch from a private repository goes out unauthenticated even though
	// the index download was authenticated. Empty values are no-ops, so this is
	// safe for public repositories.
	downloadOpts := []getter.Option{
		getter.WithURL(helmRepo.Config.URL), // Pass repo URL for context if needed by getters
	}
	if c.options != nil {
		downloadOpts = append(downloadOpts,
			getter.WithBasicAuth(c.options.username, c.options.password),
			getter.WithTLSClientConfig(c.options.certFile, c.options.keyFile, c.options.caFile),
			getter.WithInsecureSkipVerifyTLS(c.options.insecureSkipTLSVerify),
			getter.WithPassCredentialsAll(c.options.passCredentialsAll),
		)
	}

	dl := downloader.ChartDownloader{
		Out:              io.Discard,
		Keyring:          "",
		Getters:          c.getters(),
		Options:          downloadOpts,
		RepositoryConfig: c.settings.RepositoryConfig,
		RepositoryCache:  c.settings.RepositoryCache,
		ContentCache:     c.settings.ContentCache,
		Verify:           downloader.VerifyNever,
	}

	_, downloadSpan := startClientSpan(ctx, spanChartDownload, chartDownloadAttributes(chartURL)...)
	chartOutputPath, _, err := dl.DownloadTo(chartURL, version, chartPath)
	endSpan(ctx, downloadSpan, err)
	if err != nil {
		return nil, fmt.Errorf("failed to download chart %s version %s from %s: %v", chartName, version, chartURL, err)
	}

	// Load the downloaded chart
	loadedChart, err := loader.Load(chartOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart from %s: %v", chartPath, err)
	}

	v2Chart, ok := loadedChart.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("charts V3 format is not supported")
	}

	return v2Chart, nil
}

func (c *HelmClient) GetChartLatestVersion(ctx context.Context, repoURL, chartName string) (latestVersion string, err error) {
	ctx, op := startOperation(ctx, operationGetLatestVersion, repoURL,
		attribute.String(attrHelmChartName, chartName))
	defer func() { op.end(ctx, err) }()

	if IsOCI(repoURL) {
		ref := parseOCIReference(repoURL, chartName, "")
		tags, err := c.ociTags(ctx, repoURL, ref)
		if err != nil {
			return "", fmt.Errorf("failed to list tags for OCI chart %s: %v", ref, err)
		}
		if len(tags) == 0 {
			return "", fmt.Errorf("no versions found for OCI chart %s", ref)
		}
		// An empty constraint skips prereleases, same as helm without --devel.
		tag, err := registry.GetTagMatchingVersionOrConstraint(tags, "")
		if err != nil {
			return "", fmt.Errorf("no stable version found for OCI chart %s: %v", ref, err)
		}

		op.setAttributes(attribute.String(attrHelmChartVersion, tag))

		return tag, nil
	}

	helmRepo, err := c.getRepo(ctx, repoURL, repoURL)
	if err != nil {
		return "", fmt.Errorf("failed to get repository: %v", err)
	}

	// An empty version skips prereleases, same as helm without --devel.
	latest, err := helmRepo.IndexFile.Get(chartName, "")
	if err != nil {
		return "", fmt.Errorf("failed to find latest stable version of chart %s in repository %s: %v", chartName, repoURL, err)
	}

	op.setAttributes(attribute.String(attrHelmChartVersion, latest.Version))

	return latest.Version, nil
}

func (c *HelmClient) GetChartLatestValues(ctx context.Context, repoURL, chartName string) (values string, err error) {
	ctx, op := startOperation(ctx, operationGetLatestValues, repoURL,
		attribute.String(attrHelmChartName, chartName))
	defer func() { op.end(ctx, err) }()

	v, err := c.GetChartLatestVersion(ctx, repoURL, chartName)
	if err != nil {
		return "", fmt.Errorf("failed to get chart %s version %s: %v", chartName, v, err)
	}

	op.setAttributes(attribute.String(attrHelmChartVersion, v))

	return c.GetChartValues(ctx, repoURL, chartName, v)
}

func (c *HelmClient) GetChartDependencies(ctx context.Context, repoURL, chartName, version string) (deps []string, err error) {
	ctx, op := startOperation(ctx, operationGetChartDependencies, repoURL,
		attribute.String(attrHelmChartName, chartName),
		attribute.String(attrHelmChartVersion, version))
	defer func() { op.end(ctx, err) }()

	loadedChart, err := c.loadChart(ctx, repoURL, chartName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return nil, fmt.Errorf("chart %s version %s not found", chartName, version)
	}

	deps, err = helm_parser.GetChartDependencies(loadedChart)
	if err != nil {
		return nil, fmt.Errorf("failed to get dependencies for chart %s version %s: %v", chartName, version, err)
	}

	op.setAttributes(attribute.Int(attrHelmDependencyCount, len(deps)))

	return deps, nil
}

func (c *HelmClient) GetChartImages(ctx context.Context, repoURL, chartName, version string, customValues map[string]any, recursive bool) (images []helm_parser.ImageReference, err error) {
	ctx, op := startOperation(ctx, operationGetChartImages, repoURL,
		attribute.String(attrHelmChartName, chartName),
		attribute.String(attrHelmChartVersion, version),
		attribute.Bool(attrHelmRecursive, recursive))
	defer func() { op.end(ctx, err) }()

	loadedChart, err := c.loadChart(ctx, repoURL, chartName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return nil, fmt.Errorf("chart %s version %s not found", chartName, version)
	}

	parseCtx, parseSpan := startInternalSpan(ctx, spanParseImages, attribute.Bool(attrHelmRecursive, recursive))
	images, err = helm_parser.GetChartImages(parseCtx, loadedChart, customValues, recursive)
	endSpan(ctx, parseSpan, err)
	if err != nil {
		return nil, fmt.Errorf("failed to extract images from chart %s version %s: %v", chartName, version, err)
	}

	op.setAttributes(attribute.Int(attrHelmImageCount, len(images)))

	return images, nil
}
