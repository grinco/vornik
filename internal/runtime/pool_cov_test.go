package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// poolCovManager returns a Manager backed by a fake podman that succeeds on
// `run` (echoing a container ID) and emits a running inspect record. This is
// enough for StartWarm and the WaitForTaskDone liveness probe.
func poolCovManager(t *testing.T, containerID string) *Manager {
	t.Helper()
	return &Manager{
		podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
case "$1" in
  run)     echo "`+containerID+`"; exit 0 ;;
  inspect) echo '[{"Id":"`+containerID+`","Name":"/w","Image":"img","State":{"Status":"running","ExitCode":0},"Config":{"Labels":{}}}]'; exit 0 ;;
  stop|rm) exit 0 ;;
  *)       exit 1 ;;
esac
`),
		logger: zerolog.Nop(),
	}
}

func TestAcquire_HitMissAndStopped(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, DefaultPoolConfig())
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}

	// Empty pool: cold start (miss).
	if got := pool.Acquire(key); got != nil {
		t.Fatalf("Acquire on empty pool = %v, want nil", got)
	}

	// Seed one idle entry: Acquire returns it and marks it InUse.
	entry := &PoolEntry{ContainerID: "c1", Key: key}
	pool.mu.Lock()
	pool.byKey[key] = []*PoolEntry{entry}
	pool.mu.Unlock()

	got := pool.Acquire(key)
	if got != entry {
		t.Fatalf("Acquire = %v, want seeded entry", got)
	}
	if !got.InUse {
		t.Error("acquired entry must be marked InUse")
	}
	if got.ReuseCount != 1 {
		t.Errorf("ReuseCount = %d, want 1", got.ReuseCount)
	}

	// Now the only entry is InUse: a second Acquire misses.
	if again := pool.Acquire(key); again != nil {
		t.Fatalf("Acquire with all-busy pool = %v, want nil", again)
	}

	// Stopped pool always returns nil.
	pool.mu.Lock()
	pool.stopped = true
	pool.mu.Unlock()
	if got := pool.Acquire(key); got != nil {
		t.Fatalf("Acquire on stopped pool = %v, want nil", got)
	}
}

func TestStartWarm_StoppedAndLimitReached(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, PoolConfig{MaxPerRole: 1})
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}

	pool.mu.Lock()
	pool.stopped = true
	pool.mu.Unlock()
	if _, err := pool.StartWarm(context.Background(), key, nil); err == nil {
		t.Fatal("StartWarm on stopped pool must error")
	}

	// Reset and fill the single slot, then expect a limit error.
	pool.mu.Lock()
	pool.stopped = false
	pool.byKey[key] = []*PoolEntry{{Key: key}}
	pool.mu.Unlock()
	_, err := pool.StartWarm(context.Background(), key, nil)
	if err == nil {
		t.Fatal("StartWarm past MaxPerRole must error")
	}
}

func TestStartWarm_SuccessRegistersEntryAndMergesEnv(t *testing.T) {
	mgr := poolCovManager(t, "warm-abc")
	pool := NewWarmPool(mgr, PoolConfig{MaxPerRole: 2},
		WithPoolEnvVars(map[string]string{"BASE": "1"}),
	)
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}

	entry, err := pool.StartWarm(context.Background(), key, map[string]string{"OVERRIDE": "2"})
	if err != nil {
		t.Fatalf("StartWarm() error = %v", err)
	}
	if entry.ContainerID != "warm-abc" {
		t.Fatalf("ContainerID = %q, want warm-abc", entry.ContainerID)
	}
	if !entry.InUse {
		t.Error("fresh warm entry must be InUse")
	}
	// Scratch dirs created on host.
	for _, d := range []string{entry.InputDir, entry.OutputDir, entry.WorkspaceDir} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("expected scratch dir %q, err=%v", d, err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(entry.InputDir)) })

	// Entry registered under both maps; placeholder replaced.
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.entries["warm-abc"] != entry {
		t.Error("entry not registered in entries map")
	}
	if len(pool.byKey[key]) != 1 || pool.byKey[key][0] != entry {
		t.Errorf("byKey not updated with real entry: %+v", pool.byKey[key])
	}
}

func TestStartWarm_StartContainerFailureReleasesSlot(t *testing.T) {
	// Fake podman fails every `run`, so StartContainer returns an error and
	// StartWarm must remove the reserved placeholder slot.
	mgr := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
case "$1" in
  run) echo "boom" >&2; exit 125 ;;
  *)   exit 1 ;;
esac
`), logger: zerolog.Nop()}
	pool := NewWarmPool(mgr, PoolConfig{MaxPerRole: 1})
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}

	if _, err := pool.StartWarm(context.Background(), key, nil); err == nil {
		t.Fatal("StartWarm must error when StartContainer fails")
	}
	// Slot released — a subsequent StartWarm shouldn't hit the limit error.
	pool.mu.Lock()
	n := len(pool.byKey[key])
	pool.mu.Unlock()
	if n != 0 {
		t.Fatalf("placeholder slot not released: byKey len = %d", n)
	}
}

func TestSize_CountsEntriesPerKey(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, DefaultPoolConfig())
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}
	other := PoolKey{ProjectID: "p", Role: "tester", Image: "img"}

	if pool.Size(key) != 0 {
		t.Fatal("empty pool size should be 0")
	}
	pool.mu.Lock()
	pool.byKey[key] = []*PoolEntry{{Key: key}, {Key: key}}
	pool.mu.Unlock()
	if got := pool.Size(key); got != 2 {
		t.Errorf("Size(key) = %d, want 2", got)
	}
	if got := pool.Size(other); got != 0 {
		t.Errorf("Size(other) = %d, want 0", got)
	}
}

func TestWaitForTaskDone_ReturnsResultWhenDone(t *testing.T) {
	out := t.TempDir()
	entry := &PoolEntry{OutputDir: out}
	// Pre-write result + done so the first doneTicker tick succeeds. The
	// 250ms ticker means we need a comfortably larger timeout.
	if err := os.WriteFile(filepath.Join(out, "result.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, ".done"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := &WarmPool{logger: zerolog.Nop()} // nil manager: liveness probe skipped
	data, err := pool.WaitForTaskDone(context.Background(), entry, 2*time.Second)
	if err != nil {
		t.Fatalf("WaitForTaskDone() error = %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("result = %q, want the written payload", string(data))
	}
}

func TestWaitForTaskDone_DoneButResultMissing(t *testing.T) {
	out := t.TempDir()
	entry := &PoolEntry{OutputDir: out}
	// .done present but no result.json -> error.
	if err := os.WriteFile(filepath.Join(out, ".done"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := &WarmPool{logger: zerolog.Nop()}
	_, err := pool.WaitForTaskDone(context.Background(), entry, 2*time.Second)
	if err == nil {
		t.Fatal("expected error when .done present but result.json missing")
	}
}

func TestWaitForTaskDone_TimesOut(t *testing.T) {
	entry := &PoolEntry{OutputDir: t.TempDir()}
	pool := &WarmPool{logger: zerolog.Nop()} // nil manager skips liveness

	start := time.Now()
	_, err := pool.WaitForTaskDone(context.Background(), entry, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Errorf("returned too early (%s); timeout not honored", time.Since(start))
	}
}

func TestEvictIdle_RemovesStaleIdleEntries(t *testing.T) {
	mgr := poolCovManager(t, "warm-evict")
	pool := NewWarmPool(mgr, PoolConfig{IdleTimeout: time.Millisecond, MaxPerRole: 2})
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}

	dir := t.TempDir()
	entry := &PoolEntry{
		ContainerID: "warm-evict",
		Key:         key,
		InUse:       false,
		LastUsedAt:  time.Now().Add(-time.Hour), // long idle -> evictable
		InputDir:    filepath.Join(dir, "input"),
	}
	if err := os.MkdirAll(entry.InputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.entries[entry.ContainerID] = entry
	pool.byKey[key] = []*PoolEntry{entry}
	pool.mu.Unlock()

	pool.evictIdle()

	pool.mu.Lock()
	_, present := pool.entries[entry.ContainerID]
	n := len(pool.byKey[key])
	pool.mu.Unlock()
	if present || n != 0 {
		t.Fatalf("idle entry not removed from maps (present=%v, byKey=%d)", present, n)
	}
	// Drain the teardown goroutine evictIdle spawned (tracked in p.wg).
	if !waitGroupWithContext(context.Background(), &pool.wg) {
		t.Fatal("teardown goroutine did not finish")
	}
}

func TestEvictIdle_KeepsBusyAndFreshEntries(t *testing.T) {
	pool := NewWarmPool(&Manager{podmanPath: "/bin/false", logger: zerolog.Nop()}, PoolConfig{IdleTimeout: time.Hour, MaxPerRole: 2})
	key := PoolKey{ProjectID: "p", Role: "coder", Image: "img"}

	busy := &PoolEntry{ContainerID: "busy", Key: key, InUse: true, LastUsedAt: time.Now().Add(-time.Hour)}
	fresh := &PoolEntry{ContainerID: "fresh", Key: key, InUse: false, LastUsedAt: time.Now()}
	pool.mu.Lock()
	pool.entries["busy"] = busy
	pool.entries["fresh"] = fresh
	pool.byKey[key] = []*PoolEntry{busy, fresh}
	pool.mu.Unlock()

	pool.evictIdle()

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.entries) != 2 {
		t.Fatalf("evictIdle removed entries it should keep: %d remain", len(pool.entries))
	}
}
