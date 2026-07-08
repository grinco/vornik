package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestSyncProjects_ConnectsConcurrently is the 2026-07-08 watcher-wedge Layer-2
// regression. SyncProjects runs inside the config-reload activator (initMCP),
// which holds the reloader's reloadMu. Pre-fix it dialled servers SERIALLY,
// each bounded at 30s — so N slow/offline servers stalled the whole reload
// cycle for the SUM of their timeouts (an offline pagedrop alone held it 30s
// every reload). Dialling concurrently caps the wall-clock at the slowest
// SINGLE dial.
func TestSyncProjects_ConnectsConcurrently(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()

	const perConnect = 150 * time.Millisecond
	var calls int32
	connectFn = func(ctx context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-time.After(perConnect):
		case <-ctx.Done():
		}
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())
	desired := map[string][]ServerConfig{
		"p1": {{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}},
	}

	start := time.Now()
	mgr.SyncProjects(context.Background(), desired)
	elapsed := time.Since(start)

	require.Equal(t, int32(4), atomic.LoadInt32(&calls))
	require.Equal(t, 4, mgr.ServerCount(), "all four servers must connect")
	// Serial would be ~4×150ms=600ms; concurrent ~150ms. A generous ceiling
	// well under the serial sum proves the dials overlapped.
	require.Less(t, elapsed, 400*time.Millisecond,
		"connects must run concurrently — a serial sum would stall the reload activator")
}

// TestSyncProjects_FailingServerDoesNotDropHealthy — a server whose dial errors
// (e.g. the real "mcp server pagedrop: command is empty") must not prevent a
// healthy server in the same batch from connecting; the failure is logged +
// skipped.
func TestSyncProjects_FailingServerDoesNotDropHealthy(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()

	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if cfg.Name == "broken" {
			return nil, context.Canceled // instant dial failure, like a config error
		}
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())
	mgr.SyncProjects(context.Background(), map[string][]ServerConfig{
		"p1": {{Name: "broken"}, {Name: "healthy"}},
	})

	require.Equal(t, 1, mgr.ServerCount(), "the healthy server connects despite the broken one")
	require.NotEmpty(t, mgr.Tools("p1"), "healthy server's tools must be available")
}

// TestSyncProjects_HangingDialDoesNotStallBatch is the core robustness
// guarantee behind the 2026-07-08 fix: a dial that IGNORES its context and
// blocks forever must not hold SyncProjects — and therefore the reload
// activator and the reloader's reloadMu — past the overall budget. The batch
// proceeds with whatever connected; the pathological straggler is abandoned.
func TestSyncProjects_HangingDialDoesNotStallBatch(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()

	// connectsDone lets the test JOIN both dial goroutines before the deferred
	// connectFn restore, so the abandoned goroutine's read of the global
	// connectFn can't race the restore (a test-only concern — prod never
	// reassigns connectFn).
	var connectsDone sync.WaitGroup
	connectsDone.Add(2)
	blockForever := make(chan struct{})
	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		defer connectsDone.Done()
		if cfg.Name == "wedged" {
			<-blockForever // ignore ctx entirely — the pathological dial
			return nil, context.Canceled
		}
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		mgr.SyncProjects(ctx, map[string][]ServerConfig{
			"p1": {{Name: "wedged"}, {Name: "healthy"}},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SyncProjects blocked on a ctx-ignoring dial — the reload activator would wedge (2026-07-08 regression)")
	}
	require.Equal(t, 1, mgr.ServerCount(), "healthy server connects; the wedged dial is abandoned, not awaited")

	close(blockForever) // release the wedged dial
	connectsDone.Wait() // join both goroutines before the deferred connectFn restore
}

// TestSyncProjects_BlockingDisplacedCloseDoesNotStall is the ACTUAL 2026-07-08
// activator-hang regression. Tearing down a displaced (old-catalog) client can
// block — Client.Close() for a stdio server does Process.Kill + cmd.Wait, and a
// subprocess that won't reap (or an SSE stream that won't terminate) hangs it.
// SyncProjects runs inside the config-reload activator holding reloadMu, so a
// synchronous displaced Close() there wedged the whole reload cycle (initMCP
// never returned; live reloads stopped applying until a restart). The closes
// must be detached so a slow Close can't hold up the reload.
func TestSyncProjects_BlockingDisplacedCloseDoesNotStall(t *testing.T) {
	origConnect, origClose := connectFn, closeFn
	defer func() { connectFn, closeFn = origConnect, origClose }()

	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}
	blockClose := make(chan struct{})
	var closeDone sync.WaitGroup
	closeDone.Add(1)
	closeFn = func(_ *Client) error {
		<-blockClose // the displaced Close hangs until released
		closeDone.Done()
		return nil
	}

	mgr := NewManager(zerolog.Nop())
	// Seed an old catalog that the sync will displace — its Close will hang.
	mgr.mu.Lock()
	mgr.clients["p1"] = map[string]*Client{"old": newFakeClient(ServerConfig{Name: "old"}, nil)}
	mgr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		mgr.SyncProjects(context.Background(), map[string][]ServerConfig{"p1": {{Name: "new"}}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SyncProjects blocked on a displaced client's Close() — the reload activator would wedge (2026-07-08 root cause)")
	}
	require.Equal(t, 1, mgr.ServerCount(), "new catalog is live even while the displaced Close still hangs")

	close(blockClose) // release the hung displaced Close
	closeDone.Wait()  // join the detached close goroutine before the deferred closeFn restore
}
