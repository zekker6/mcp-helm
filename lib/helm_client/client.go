package helm_client

import (
	"fmt"
	"sort"
	"sync"

	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
)

var (
	helmCacheDir = "/tmp/helm_cache"
)

type HelmClient struct {
	settings *cli.EnvSettings

	reposMu sync.Mutex
	repos   map[string]*repo.ChartRepository
}

func NewClient() *HelmClient {
	settings := cli.New()
	settings.RepositoryCache = helmCacheDir

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

	sort.Strings(versions)
	return versions, nil
}
