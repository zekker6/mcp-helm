package helm_client

import (
	"errors"
	"math"
	"os"
	"runtime/debug"
	"testing"

	"github.com/KimMachineGun/automemlimit/memlimit"
)

func TestCacheSizeForMemory(t *testing.T) {
	for _, tt := range []struct {
		name       string
		goLimit    int64
		cgroup     uint64
		cgroupErr  error
		host       uint64
		hostErr    error
		want       int64
		wantSource string
		wantErr    bool
	}{
		{name: "512 MiB container", goLimit: math.MaxInt64, cgroup: 512 << 20, host: 32 << 30, want: 32 << 20, wantSource: "cgroup"},
		{name: "1 GiB container", goLimit: math.MaxInt64, cgroup: 1 << 30, host: 32 << 30, want: 64 << 20, wantSource: "cgroup"},
		{name: "2 GiB container", goLimit: math.MaxInt64, cgroup: 2 << 30, host: 32 << 30, want: 128 << 20, wantSource: "cgroup"},
		{name: "large container capped", goLimit: math.MaxInt64, cgroup: 8 << 30, host: 32 << 30, want: MaxAutoRepoCacheBytes, wantSource: "cgroup"},
		{name: "Go limit smaller", goLimit: 128 << 20, cgroup: 1 << 30, host: 32 << 30, want: 8 << 20, wantSource: "go"},
		{name: "container smaller than Go", goLimit: 4 << 30, cgroup: 256 << 20, host: 32 << 30, want: 16 << 20, wantSource: "cgroup"},
		{name: "host smaller", goLimit: 4 << 30, cgroup: 8 << 30, host: 2 << 30, want: 128 << 20, wantSource: "host"},
		{name: "unlimited container", goLimit: math.MaxInt64, cgroupErr: memlimit.ErrNoLimit, host: 2 << 30, want: 128 << 20, wantSource: "host"},
		{name: "no cgroup", goLimit: math.MaxInt64, cgroupErr: memlimit.ErrNoCgroup, host: 2 << 30, want: 128 << 20, wantSource: "host"},
		{name: "unsupported cgroup", goLimit: math.MaxInt64, cgroupErr: memlimit.ErrCgroupsNotSupported, host: 2 << 30, want: 128 << 20, wantSource: "host"},
		{name: "no sources", goLimit: math.MaxInt64, cgroupErr: memlimit.ErrNoLimit, hostErr: memlimit.ErrNoLimit, want: DefaultRepoCacheMaxBytes},
		{name: "Go only", goLimit: 2 << 30, cgroupErr: memlimit.ErrNoLimit, hostErr: memlimit.ErrNoLimit, want: 128 << 20, wantSource: "go"},
		{name: "unreadable controller", goLimit: math.MaxInt64, cgroupErr: os.ErrPermission, host: 32 << 30, want: DefaultRepoCacheMaxBytes, wantSource: "host", wantErr: true},
		{name: "partial detection honors smaller Go limit", goLimit: 128 << 20, cgroupErr: os.ErrPermission, host: 32 << 30, want: 8 << 20, wantSource: "go", wantErr: true},
		{name: "unreadable host", goLimit: math.MaxInt64, cgroup: 2 << 30, hostErr: os.ErrPermission, want: DefaultRepoCacheMaxBytes, wantSource: "cgroup", wantErr: true},
		{name: "all probes fail", goLimit: math.MaxInt64, cgroupErr: os.ErrPermission, hostErr: os.ErrPermission, want: DefaultRepoCacheMaxBytes, wantErr: true},
		{name: "zero Go limit", goLimit: 0, cgroup: 1 << 30, host: 32 << 30, want: 1, wantSource: "go"},
		{name: "zero container limit", goLimit: math.MaxInt64, cgroup: 0, host: 32 << 30, want: 1, wantSource: "cgroup"},
		{name: "tiny allocation", goLimit: math.MaxInt64, cgroup: 8, host: 32 << 30, want: 1, wantSource: "cgroup"},
		{name: "unsigned overflow avoided", goLimit: math.MaxInt64, cgroup: math.MaxUint64, host: math.MaxUint64, want: MaxAutoRepoCacheBytes, wantSource: "cgroup"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheSizeForMemory(tt.goLimit,
				func() (uint64, error) { return tt.cgroup, tt.cgroupErr },
				func() (uint64, error) { return tt.host, tt.hostErr })
			if got.bytes != tt.want || got.source != tt.wantSource || (got.err != nil) != tt.wantErr {
				t.Fatalf("cache sizing = %+v, want bytes=%d source=%q error=%t", got, tt.want, tt.wantSource, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(got.err, os.ErrPermission) {
				t.Fatalf("probe error lost: %v", got.err)
			}
		})
	}
}

func TestCacheSizingEnvPrecedence(t *testing.T) {
	clearLimitEnv(t)
	for _, tt := range []struct {
		name     string
		explicit bool
		value    string
		want     int64
		wantErr  bool
	}{
		{name: "automatic", want: 32 << 20},
		{name: "explicit smaller", explicit: true, value: "1024", want: 1024},
		{name: "explicit exceeds auto cap", explicit: true, value: "1073741824", want: 1 << 30},
		{name: "explicit invalid", explicit: true, value: "0", wantErr: true},
		{name: "explicit empty", explicit: true, value: "", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.explicit {
				t.Setenv("MCP_HELM_REPO_CACHE_MAX_BYTES", tt.value)
			}
			calls := 0
			options := defaultClientOptions()
			err := options.loadLimitEnv(func() cacheSizing {
				calls++
				return cacheSizeForMemory(math.MaxInt64, memlimit.Limit(512<<20), memlimit.Limit(32<<30))
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("configuration error = %v, want error=%t", err, tt.wantErr)
			}
			if !tt.wantErr && options.repoCacheBytes != tt.want {
				t.Fatalf("cache bytes = %d, want %d", options.repoCacheBytes, tt.want)
			}
			if tt.explicit && calls != 0 || !tt.explicit && calls != 1 {
				t.Fatalf("detection calls = %d, explicit=%t", calls, tt.explicit)
			}
			if options.repoCacheEntries != DefaultRepoCacheMaxEntries || options.indexMaxBytes != DefaultIndexMaxBytes ||
				options.chartMaxBytes != DefaultChartMaxBytes || options.ociMaxBytes != DefaultOCIMaxBytes {
				t.Fatal("cache auto-sizing changed an independent limit")
			}
		})
	}
}

func TestNewClientAutoCacheSizing(t *testing.T) {
	clearLimitEnv(t)
	previous := debug.SetMemoryLimit(128 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })
	first := newTestClient(t)
	budget := first.options.repoCacheBytes
	if budget <= 0 || budget > 8<<20 {
		t.Fatalf("cache budget %d did not respect 128 MiB Go limit", budget)
	}
	if got := debug.SetMemoryLimit(-1); got != 128<<20 {
		t.Fatalf("cache detection changed Go memory limit: %d", got)
	}
	debug.SetMemoryLimit(256 << 20)
	if first.options.repoCacheBytes != budget {
		t.Fatal("existing client budget changed after creation")
	}
}
