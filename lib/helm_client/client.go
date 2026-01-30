package helm_client

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/repo/v1"

	"github.com/zekker6/mcp-helm/lib/helm_parser"
)

var (
	tmpDir = "/tmp/helm_cache"
)

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

type HelmClient struct {
	settings       *cli.EnvSettings
	registryClient *registry.Client
	options        *clientOptions

	reposMu sync.Mutex
	repos   map[string]*repo.ChartRepository
}

// NewClient creates a new HelmClient with optional configuration.
// It supports both HTTP Helm repositories and OCI registries.
//
// Authentication for registries can be configured via:
//   - WithCredentialsFile: path to Docker-style credentials file
//   - WithBasicAuth: username/password authentication
func NewClient(opts ...ClientOption) (*HelmClient, error) {
	options := &clientOptions{}
	for _, opt := range opts {
		opt(options)
	}

	settings := cli.New()
	settings.RepositoryCache = path.Join(tmpDir, "helm-cache")
	settings.RegistryConfig = path.Join(tmpDir, "helm-registry.conf")
	settings.RepositoryConfig = path.Join(tmpDir, "helm-repository.conf")

	var regOpts []registry.ClientOption
	regOpts = append(regOpts, registry.ClientOptEnableCache(true))

	if options.credentialsFile != "" {
		regOpts = append(regOpts, registry.ClientOptCredentialsFile(options.credentialsFile))
	} else if settings.RegistryConfig != "" {
		regOpts = append(regOpts, registry.ClientOptCredentialsFile(settings.RegistryConfig))
	}

	if options.username != "" && options.password != "" {
		regOpts = append(regOpts, registry.ClientOptBasicAuth(options.username, options.password))
	}

	if options.plainHTTP {
		regOpts = append(regOpts, registry.ClientOptPlainHTTP())
	}

	regClient, err := registry.NewClient(regOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %v", err)
	}

	return &HelmClient{
		settings:       settings,
		registryClient: regClient,
		options:        options,
	}, nil
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

func (c *HelmClient) getRepo(name, url string) (*repo.ChartRepository, error) {
	c.reposMu.Lock()
	defer c.reposMu.Unlock()

	if c.repos == nil {
		c.repos = make(map[string]*repo.ChartRepository)
	}

	// todo: refresh index periodically based on last update time or a fixed interval
	if v, exists := c.repos[name]; exists {
		return v, nil
	}

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

	requestedRepo, err := repo.NewChartRepository(entry, getter.All(c.settings))
	if err != nil {
		return nil, fmt.Errorf("failed to create chart repository: %v", err)
	}

	indexFileLocation, err := requestedRepo.DownloadIndexFile()
	if err != nil {
		return nil, fmt.Errorf("failed to download repository index: %v", err)
	}

	file, err := repo.LoadIndexFile(indexFileLocation)
	if err != nil {
		return nil, fmt.Errorf("failed to load index file: %v", err)
	}
	requestedRepo.IndexFile = file
	requestedRepo.IndexFile.SortEntries()

	c.repos[name] = requestedRepo
	return requestedRepo, nil
}

func (c *HelmClient) ListCharts(repoURL string) ([]string, error) {
	if IsOCI(repoURL) {
		// For OCI, each repository contains a single chart
		// Return the chart name extracted from the URL
		chartName := ExtractChartNameFromOCI(repoURL)
		if chartName == "" {
			return nil, fmt.Errorf("invalid OCI reference: cannot extract chart name from %s", repoURL)
		}
		return []string{chartName}, nil
	}

	helmRepo, err := c.getRepo(repoURL, repoURL)
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

	return chartsList, nil
}

func (c *HelmClient) ListChartVersions(repoURL string, chart string) ([]string, error) {
	if IsOCI(repoURL) {
		ref := parseOCIReference(repoURL, chart, "")
		tags, err := c.registryClient.Tags(ref)
		if err != nil {
			return nil, fmt.Errorf("failed to list tags for OCI chart %s: %v", ref, err)
		}
		// Tags are already sorted in descending semver order by Helm's registry package
		return tags, nil
	}

	helmRepo, err := c.getRepo(repoURL, repoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to add repository: %v", err)
	}

	versions := make([]string, 0)
	for k, v := range helmRepo.IndexFile.Entries {
		if k != chart {
			continue
		}

		for _, ver := range v {
			versions = append(versions, ver.Version)
		}
	}
	// Do not sort version as those were sorted in original index file

	return versions, nil
}

func (c *HelmClient) GetChartValues(repoURL, chartName, version string) (string, error) {
	loadedChart, err := c.loadChart(repoURL, chartName, version)
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

func (c *HelmClient) GetChartContents(repoURL, chartName, version string, recursive bool) (string, error) {
	loadedChart, err := c.loadChart(repoURL, chartName, version)
	if err != nil {
		return "", fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return "", fmt.Errorf("chart %s version %s not found", chartName, version)
	}

	contents, err := helm_parser.GetChartContents(loadedChart, recursive)
	if err != nil {
		return "", fmt.Errorf("failed to get chart contents for %s version %s: %v", chartName, version, err)
	}
	return contents, nil
}

func (c *HelmClient) loadChart(repoURL string, chartName string, version string) (*chartv2.Chart, error) {
	if IsOCI(repoURL) {
		return c.loadChartFromOCI(repoURL, chartName, version)
	}

	return c.loadChartFromHTTP(repoURL, chartName, version)
}

func (c *HelmClient) loadChartFromOCI(repoURL, chartName, version string) (*chartv2.Chart, error) {
	ref := parseOCIReference(repoURL, chartName, version)

	result, err := c.registryClient.Pull(ref, registry.PullOptWithChart(true))
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

func (c *HelmClient) loadChartFromHTTP(repoURL, chartName, version string) (*chartv2.Chart, error) {
	// TODO: implement caching for values file
	helmRepo, err := c.getRepo(repoURL, repoURL)
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
	_ = os.MkdirAll(chartPath, 0755)

	dl := downloader.ChartDownloader{
		Out:     io.Discard,
		Keyring: "",
		Getters: getter.All(c.settings),
		Options: []getter.Option{
			getter.WithURL(helmRepo.Config.URL), // Pass repo URL for context if needed by getters
		},
		RepositoryConfig: c.settings.RepositoryConfig,
		RepositoryCache:  c.settings.RepositoryCache,
		ContentCache:     c.settings.ContentCache,
		Verify:           downloader.VerifyNever,
	}

	chartOutputPath, _, err := dl.DownloadTo(chartURL, version, chartPath)
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

func (c *HelmClient) GetChartLatestVersion(repoURL, chartName string) (string, error) {
	if IsOCI(repoURL) {
		ref := parseOCIReference(repoURL, chartName, "")
		tags, err := c.registryClient.Tags(ref)
		if err != nil {
			return "", fmt.Errorf("failed to list tags for OCI chart %s: %v", ref, err)
		}
		if len(tags) == 0 {
			return "", fmt.Errorf("no versions found for OCI chart %s", ref)
		}
		// Tags are sorted in descending semver order, first is latest
		return tags[0], nil
	}

	helmRepo, err := c.getRepo(repoURL, repoURL)
	if err != nil {
		return "", fmt.Errorf("failed to get repository: %v", err)
	}

	chartVersions, ok := helmRepo.IndexFile.Entries[chartName]
	if !ok || len(chartVersions) == 0 {
		return "", fmt.Errorf("chart %s not found in repository %s", chartName, repoURL)
	}

	// IndexFile.SortEntries() sorts versions in descending order, so the first one is the latest.
	latestVersion := chartVersions[0].Version
	return latestVersion, nil
}

func (c *HelmClient) GetChartLatestValues(repoURL, chartName string) (string, error) {
	v, err := c.GetChartLatestVersion(repoURL, chartName)
	if err != nil {
		return "", fmt.Errorf("failed to get chart %s version %s: %v", chartName, v, err)
	}

	return c.GetChartValues(repoURL, chartName, v)
}

func (c *HelmClient) GetChartDependencies(repoURL, chartName, version string) ([]string, error) {
	loadedChart, err := c.loadChart(repoURL, chartName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return nil, fmt.Errorf("chart %s version %s not found", chartName, version)
	}

	deps, err := helm_parser.GetChartDependencies(loadedChart)
	if err != nil {
		return nil, fmt.Errorf("failed to get dependencies for chart %s version %s: %v", chartName, version, err)
	}
	return deps, nil
}

func (c *HelmClient) GetChartImages(repoURL, chartName, version string, customValues map[string]any, recursive bool) ([]helm_parser.ImageReference, error) {
	loadedChart, err := c.loadChart(repoURL, chartName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return nil, fmt.Errorf("chart %s version %s not found", chartName, version)
	}

	images, err := helm_parser.GetChartImages(loadedChart, customValues, recursive)
	if err != nil {
		return nil, fmt.Errorf("failed to extract images from chart %s version %s: %v", chartName, version, err)
	}
	return images, nil
}
