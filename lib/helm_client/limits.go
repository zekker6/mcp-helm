package helm_client

import (
	"fmt"
	"os"
	"strconv"

	"go.uber.org/zap"

	"github.com/zekker6/mcp-helm/lib/logger"
)

const (
	// DefaultRepoCacheMaxEntries bounds the number of retained repository indexes.
	DefaultRepoCacheMaxEntries = 16
	// DefaultRepoCacheMaxBytes is the fallback source-byte budget when memory detection fails.
	DefaultRepoCacheMaxBytes int64 = 64 << 20
	// DefaultIndexMaxBytes bounds a single HTTP repository index download.
	DefaultIndexMaxBytes int64 = 32 << 20
	// DefaultChartMaxBytes bounds a single compressed HTTP chart archive.
	DefaultChartMaxBytes int64 = 100 << 20
	// DefaultOCIMaxBytes bounds all response-body bytes consumed by one OCI pull.
	DefaultOCIMaxBytes int64 = 128 << 20
)

func defaultClientOptions() *clientOptions {
	return &clientOptions{
		repoIndexMaxAge:  DefaultRepoIndexMaxAge,
		indexMaxBytes:    DefaultIndexMaxBytes,
		chartMaxBytes:    DefaultChartMaxBytes,
		ociMaxBytes:      DefaultOCIMaxBytes,
		repoCacheBytes:   DefaultRepoCacheMaxBytes,
		repoCacheEntries: DefaultRepoCacheMaxEntries,
	}
}

func (o *clientOptions) loadLimitEnv(detectMemory func() cacheSizing) error {
	entries, err := positiveEnvInt("MCP_HELM_REPO_CACHE_MAX_ENTRIES", int64(o.repoCacheEntries), strconv.IntSize)
	if err != nil {
		return err
	}
	o.repoCacheEntries = int(entries)
	for _, setting := range []struct {
		name   string
		target *int64
	}{
		{"MCP_HELM_REPO_CACHE_MAX_BYTES", &o.repoCacheBytes},
		{"MCP_HELM_INDEX_MAX_BYTES", &o.indexMaxBytes},
		{"MCP_HELM_CHART_MAX_BYTES", &o.chartMaxBytes},
		{"MCP_HELM_OCI_MAX_BYTES", &o.ociMaxBytes},
	} {
		value, err := positiveEnvInt(setting.name, *setting.target, 64)
		if err != nil {
			return err
		}
		*setting.target = value
	}
	if _, explicit := os.LookupEnv("MCP_HELM_REPO_CACHE_MAX_BYTES"); explicit {
		logger.Info("repository cache configured from environment",
			zap.String("cache_sizing", "env"),
			zap.Int("cache_max_entries", o.repoCacheEntries),
			zap.Int64("cache_max_bytes", o.repoCacheBytes))
	} else {
		sizing := detectMemory()
		o.repoCacheBytes = sizing.bytes
		sizing.log(o.repoCacheEntries)
	}
	return nil
}

func positiveEnvInt(name string, fallback int64, bits int) (int64, error) {
	raw, exists := os.LookupEnv(name)
	if !exists {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, bits)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive decimal integer fitting in %d bits, got %q", name, bits, raw)
	}
	return value, nil
}
