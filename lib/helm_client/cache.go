package helm_client

import (
	"time"

	"helm.sh/helm/v4/pkg/repo/v1"
)

// cacheRepo runs under reposMu. Removal only releases the cache's reference;
// published indexes are never changed, so active readers retain their snapshot.
func (c *HelmClient) cacheRepo(name string, chartRepo *repo.ChartRepository, size int64, limits *clientOptions) {
	delete(c.repos, name)
	if size > limits.repoCacheBytes || limits.repoCacheEntries <= 0 {
		return
	}
	var total int64
	for _, entry := range c.repos {
		total += entry.size
	}
	for len(c.repos) >= limits.repoCacheEntries || total > limits.repoCacheBytes-size {
		var oldest string
		var oldestEntry *cachedRepo
		for key, entry := range c.repos {
			if oldestEntry == nil || entry.used.Before(oldestEntry.used) {
				oldest, oldestEntry = key, entry
			}
		}
		total -= oldestEntry.size
		delete(c.repos, oldest)
	}
	now := time.Now()
	c.repos[name] = &cachedRepo{repo: chartRepo, fetched: now, used: now, size: size}
}
