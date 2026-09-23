package helm_client

import (
	"errors"
	"fmt"
	"math"
	"runtime/debug"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"go.uber.org/zap"

	"github.com/zekker6/mcp-helm/lib/logger"
)

const (
	// MaxAutoRepoCacheBytes caps automatic sizing; explicit env overrides are not capped.
	MaxAutoRepoCacheBytes  int64 = 256 << 20
	repoCacheMemoryDivisor       = 16
)

type cacheSizing struct {
	bytes       int64
	memoryBytes uint64
	source      string
	err         error
}

func detectCacheSizing() cacheSizing {
	// A negative argument reads the current Go limit without changing it.
	return cacheSizeForMemory(debug.SetMemoryLimit(-1), memlimit.FromCgroup, memlimit.FromSystem)
}

func cacheSizeForMemory(goLimit int64, cgroup, system memlimit.Provider) cacheSizing {
	selected := cacheSizing{bytes: DefaultRepoCacheMaxBytes}
	consider := func(limit uint64, source string) {
		if selected.source == "" || limit < selected.memoryBytes {
			selected.memoryBytes, selected.source = limit, source
		}
	}
	if goLimit >= 0 && goLimit < math.MaxInt64 {
		consider(uint64(goLimit), "go")
	}
	for _, probe := range []struct {
		name string
		read memlimit.Provider
	}{
		{"cgroup", cgroup},
		{"host", system},
	} {
		limit, err := probe.read()
		switch {
		case err == nil:
			consider(limit, probe.name)
		case errors.Is(err, memlimit.ErrNoLimit), errors.Is(err, memlimit.ErrNoCgroup), errors.Is(err, memlimit.ErrCgroupsNotSupported):
			// No limit or an unsupported controller is not a detection failure.
		default:
			selected.err = errors.Join(selected.err, fmt.Errorf("detect %s memory: %w", probe.name, err))
		}
	}
	if selected.source != "" {
		// Divide and cap as uint64 before converting, including very large host limits.
		selected.bytes = int64(max(1, min(selected.memoryBytes/repoCacheMemoryDivisor, uint64(MaxAutoRepoCacheBytes))))
	}
	if selected.err != nil {
		// A hidden or unreadable controller must not cause host RAM to inflate
		// the cache. Known smaller limits still apply when detection is partial.
		selected.bytes = min(selected.bytes, DefaultRepoCacheMaxBytes)
	}
	return selected
}

func (s cacheSizing) log(entries int) {
	mode := "auto"
	if s.source == "" || s.err != nil {
		mode = "fallback"
	}
	fields := []zap.Field{
		zap.String("cache_sizing", mode),
		zap.Int("cache_max_entries", entries),
		zap.Int64("cache_max_bytes", s.bytes),
	}
	if s.source != "" {
		fields = append(fields, zap.String("memory_limit_source", s.source), zap.Uint64("memory_limit_bytes", s.memoryBytes))
	}
	if mode == "fallback" {
		if s.err != nil {
			fields = append(fields, zap.Error(s.err))
		}
		logger.Warn("repository cache memory detection incomplete; using conservative budget", fields...)
		return
	}
	logger.Info("repository cache sized from memory allocation", fields...)
}
