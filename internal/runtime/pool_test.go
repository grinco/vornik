package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestWarmPool_StopIsIdempotent(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, DefaultPoolConfig())
	pool.Start()
	pool.Stop(context.Background())
	pool.Stop(context.Background())
}

func TestWarmPool_CanRestartAfterStop(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, DefaultPoolConfig())
	pool.Start()
	pool.Stop(context.Background())
	pool.Start()
	pool.Stop(context.Background())
}

func TestWarmPool_ReleaseUnhealthyDecrementsPoolSize(t *testing.T) {
	reg := prometheus.NewRegistry()
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, DefaultPoolConfig(),
		WithPoolPrometheusRegistry(reg),
	)
	key := PoolKey{ProjectID: "p1", Role: "coder", Image: "img"}
	entry := &PoolEntry{
		ContainerID: "c1",
		Key:         key,
		InputDir:    t.TempDir(),
	}

	pool.mu.Lock()
	pool.entries[entry.ContainerID] = entry
	pool.byKey[key] = []*PoolEntry{entry}
	pool.metrics.PoolSize.WithLabelValues(key.ProjectID, key.Role).Inc()
	pool.mu.Unlock()

	pool.Release(entry, false)
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(pool.metrics.PoolSize.WithLabelValues(key.ProjectID, key.Role)) == 0
	}, time.Second, 20*time.Millisecond)
}

// TestWarmPool_ReleaseDuringStopDoesNotSpawnGoroutine pins the
// 2026-05-29 audit fix: Release MUST NOT spawn a teardown goroutine
// when the pool is in the stopped state. Pre-fix Release could
// race Stop's wg.Wait by calling wg.Add(1) after the WaitGroup had
// already drained, leaving an untracked teardown goroutine
// outliving Stop's return. New contract: Release in stopped state
// surrenders the entry to Stop's bulk iteration (just marks idle).
func TestWarmPool_ReleaseDuringStopDoesNotSpawnGoroutine(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, DefaultPoolConfig())
	key := PoolKey{ProjectID: "p1", Role: "coder", Image: "img"}
	entry := &PoolEntry{
		ContainerID: "c1",
		Key:         key,
		InputDir:    t.TempDir(),
		InUse:       true,
	}

	pool.mu.Lock()
	pool.entries[entry.ContainerID] = entry
	pool.byKey[key] = []*PoolEntry{entry}
	pool.stopped = true // simulate post-Stop drain window
	pool.mu.Unlock()

	// Release on unhealthy entry while stopped: must NOT remove
	// from the map and must NOT spawn a teardown goroutine.
	pool.Release(entry, false)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if _, present := pool.entries[entry.ContainerID]; !present {
		t.Fatal("Release during drain must leave entry in map for Stop's iteration")
	}
	if entry.InUse {
		t.Fatal("Release during drain must mark entry idle so Stop's sweep picks it up")
	}
}
