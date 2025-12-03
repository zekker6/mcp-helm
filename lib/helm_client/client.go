package helm_client

import (
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
	"helm.sh/helm/v4/pkg/repo/v1"

	"github.com/zekker6/mcp-helm/lib/helm_parser"
)

var (
	tmpDir = "/tmp/helm_cache"
)

type HelmClient struct {
	settings *cli.EnvSettings

	reposMu sync.Mutex
	repos   map[string]*repo.ChartRepository
}

func NewClient() *HelmClient {
	settings := cli.New()
	settings.RepositoryCache = path.Join(tmpDir, "helm-cache")
	settings.RegistryConfig = path.Join(tmpDir, "helm-registry.conf")
	settings.RepositoryConfig = path.Join(tmpDir, "helm-repository.conf")

	return &HelmClient{
		settings: settings,
	}
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

	requestedRepo, err := repo.NewChartRepository(&repo.Entry{
		Name: name,
		URL:  url,
	}, getter.All(c.settings))
	if err != nil {
		return nil, fmt.Errorf("failed to create chartv2 repository: %v", err)
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
	// todo: sanitize repoURL url to create a name

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
		return "", fmt.Errorf("failed to load chartv2 %s version %s: %v", chartName, version, err)
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
		return "", fmt.Errorf("failed to load chartv2 %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return "", fmt.Errorf("chartv2 %s version %s not found", chartName, version)
	}

	contents, err := helm_parser.GetChartContents(loadedChart, recursive)
	if err != nil {
		return "", fmt.Errorf("failed to get chartv2 contents for %s version %s: %v", chartName, version, err)
	}
	return contents, nil
}

func (c *HelmClient) loadChart(repoURL string, chartName string, version string) (*chartv2.Chart, error) {
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
		return nil, fmt.Errorf("failed to find chartv2 %s version %s", chartName, version)
	}

	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("no download URLs found for chartv2 %s version %s", chartName, version)
	}

	chartURL := cv.URLs[0]
	if !strings.HasPrefix(chartURL, "http://") && !strings.HasPrefix(chartURL, "https://") {
		repoBaseURL := strings.TrimSuffix(helmRepo.Config.URL, "/")
		chartURL = fmt.Sprintf("%s/%s", repoBaseURL, strings.TrimPrefix(chartURL, "/"))
	}

	tempDir, err := os.MkdirTemp("", "helm-chartv2-")
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
		return nil, fmt.Errorf("failed to download chartv2 %s version %s from %s: %v", chartName, version, chartURL, err)
	}

	// Load the downloaded chartv2
	loadedChart, err := loader.Load(chartOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chartv2 from %s: %v", chartPath, err)
	}

	v2Chart, ok := loadedChart.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("charts V3 format is not supported")
	}

	return v2Chart, nil
}

func (c *HelmClient) GetChartLatestVersion(repoURL, chartName string) (string, error) {
	helmRepo, err := c.getRepo(repoURL, repoURL)
	if err != nil {
		return "", fmt.Errorf("failed to get repository: %v", err)
	}

	chartVersions, ok := helmRepo.IndexFile.Entries[chartName]
	if !ok || len(chartVersions) == 0 {
		return "", fmt.Errorf("chartv2 %s not found in repository %s", chartName, repoURL)
	}

	// IndexFile.SortEntries() sorts versions in descending order, so the first one is the latest.
	latestVersion := chartVersions[0].Version
	return latestVersion, nil
}

func (c *HelmClient) GetChartLatestValues(repoURL, chartName string) (string, error) {
	v, err := c.GetChartLatestVersion(repoURL, chartName)
	if err != nil {
		return "", fmt.Errorf("failed to get chartv2 %s version %s: %v", chartName, v, err)
	}

	return c.GetChartValues(repoURL, chartName, v)
}

func (c *HelmClient) GetChartDependencies(repoURL, chartName, version string) ([]string, error) {
	loadedChart, err := c.loadChart(repoURL, chartName, version)
	if err != nil {
		return nil, fmt.Errorf("failed to load chartv2 %s version %s: %v", chartName, version, err)
	}

	if loadedChart == nil {
		return nil, fmt.Errorf("chartv2 %s version %s not found", chartName, version)
	}

	deps, err := helm_parser.GetChartDependencies(loadedChart)
	if err != nil {
		return nil, fmt.Errorf("failed to get dependencies for chartv2 %s version %s: %v", chartName, version, err)
	}
	return deps, nil
}
