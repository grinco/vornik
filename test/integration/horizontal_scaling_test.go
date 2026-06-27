//go:build integration
// +build integration

package integration_test

// Integration tests for the horizontal-scaling primitives shipped
// in Slice 1 + 2 + 3 (https://docs.vornik.io).
// Each test exercises one matrix property by constructing two
// independent in-process instances of the relevant primitive
// against the same postgres database — same shape as two daemons
// running behind a load balancer.
//
// The tests are deliberately NOT process-level: spinning up two
// full vornik binaries adds harness complexity without testing
// anything the in-process pair doesn't already cover. The DB is
// the boundary across daemon instances, not the OS process.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// TestHorizontalScaling_RateLimit_TwoInstancesShareCounter proves
// the postgres rate-limit backend sums correctly across replicas.
// Without postgres counters, each in-process limiter would
// enforce its own local count → N replicas would effectively
// multiply the per-project cap by N.
//
// Procedure: two PostgresProjectLimiter instances against the
// same DB. Submit 60 events across both, alternating which
// limiter does the Record + Check. Assert: the 11th and later
// events on a 10/minute cap are blocked regardless of which
// limiter sees them.
func TestHorizontalScaling_RateLimit_TwoInstancesShareCounter(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	// Distinct project ID per test run so concurrent CI runs
	// don't see each other's counter rows.
	projectID := fmt.Sprintf("hs-rl-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM ratelimit_counters WHERE scope_key = $1`, projectID)
	})

	limiterA := ratelimit.NewPostgresProjectLimiter(db)
	limiterB := ratelimit.NewPostgresProjectLimiter(db)

	project := &registry.Project{
		ID: projectID,
		RateLimit: registry.ProjectRateLimit{
			TasksPerMinute: 10,
		},
	}

	now := time.Now()
	limiters := []*ratelimit.PostgresProjectLimiter{limiterA, limiterB}
	allowed := 0
	blocked := 0
	for i := 0; i < 30; i++ {
		l := limiters[i%2]
		d := l.Check(project, now)
		if d.Blocked {
			blocked++
			continue
		}
		l.Record(projectID, now)
		allowed++
	}

	// Cap is 10/minute. Exactly 10 events should land before the
	// counter trips. The remaining 20 alternations are blocked.
	assert.Equal(t, 10, allowed,
		"per-minute cap should fire on the 11th event regardless of which limiter sees it")
	assert.Equal(t, 20, blocked, "remaining events must all be blocked")
}

// TestHorizontalScaling_Elector_LeaderHandover proves that when
// one daemon's elector loses its TTL (e.g. crashed), a peer
// daemon claims the lease cleanly.
//
// Procedure: two Electors on the same worker_id, different
// holder_ids. Acquire on A; assert B can't acquire. Release A;
// assert B acquires immediately (no TTL wait — Slice 1's
// explicit Release path).
func TestHorizontalScaling_Elector_LeaderHandover(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	workerID := fmt.Sprintf("hs-elec-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id = $1`, workerID)
	})

	repo := postgres.NewLeaderLockRepository(db)
	const ttl = 30 * time.Second
	a := leaderelection.New(repo, workerID, "instance-A", ttl, zerolog.Nop())
	b := leaderelection.New(repo, workerID, "instance-B", ttl, zerolog.Nop())

	ctx := context.Background()

	// A acquires.
	a.BootstrapAcquire(ctx)
	require.True(t, a.IsLeader(), "instance A should hold the lease after BootstrapAcquire")

	// B tries to acquire — must fail; A holds an unexpired lease.
	b.BootstrapAcquire(ctx)
	require.False(t, b.IsLeader(),
		"instance B should NOT take an unexpired lease held by A")

	// A releases explicitly (Slice 1's drain-sequence path).
	require.NoError(t, a.Release(ctx))
	require.False(t, a.IsLeader(), "A's leader bit clears after Release")

	// B takes over on the next acquire — no TTL wait.
	b.BootstrapAcquire(ctx)
	require.True(t, b.IsLeader(),
		"instance B should claim the released lease without waiting out TTL")
}

// TestHorizontalScaling_Elector_NoDoubleLease proves that two
// electors racing for the same fresh worker_id never both hold
// it. The repo's Acquire is the serialisation point —
// postgres enforces the (worker_id) primary key, so exactly one
// INSERT lands; the loser sees the row already exists and the
// Acquire returns false.
func TestHorizontalScaling_Elector_NoDoubleLease(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	workerID := fmt.Sprintf("hs-race-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id = $1`, workerID)
	})

	repo := postgres.NewLeaderLockRepository(db)
	const ttl = 30 * time.Second

	// Spin up 8 electors all racing on the same worker_id.
	const racers = 8
	electors := make([]*leaderelection.Elector, racers)
	for i := range electors {
		electors[i] = leaderelection.New(repo, workerID,
			fmt.Sprintf("racer-%d", i), ttl, zerolog.Nop())
	}

	var wg sync.WaitGroup
	wg.Add(racers)
	for i := range electors {
		i := i
		go func() {
			defer wg.Done()
			electors[i].BootstrapAcquire(context.Background())
		}()
	}
	wg.Wait()

	// Exactly one elector should hold the lease.
	leaders := 0
	for i := range electors {
		if electors[i].IsLeader() {
			leaders++
		}
	}
	assert.Equal(t, 1, leaders, "exactly one elector must win the race")
}

// TestHorizontalScaling_ConfigReload_PeerListenerInvokesReload
// proves the end-to-end Slice 3a flow: instance A's
// Reload-success NOTIFY actually triggers a peer instance's
// ConfigReloader.Reload(). The unit tests pin the post-reload
// hook and the existing
// TestHorizontalScaling_ConfigReload_NotifyReachesPeerListener
// proves NOTIFY reaches a peer's pq.Listener; this test closes
// the last gap by wiring the listener side's payload-discrimination
// (self-broadcast suppression) + Reload invocation against a real
// reloader.
//
// Procedure: build instance A's ConfigReloader (with post-reload
// hook that fires `pg_notify($channel, $A_holderID)`) and
// instance B's ConfigReloader + listener loop. Call
// A.ConfigReloader.Reload(); wait briefly; assert B's
// loader/validator/activator each fired exactly once.
func TestHorizontalScaling_ConfigReload_PeerListenerInvokesReload(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	// Unique channel per test run so concurrent CI runs don't
	// see each other's notifications.
	channel := fmt.Sprintf("hs_reload_%d", time.Now().UnixNano())

	const aHolderID = "instance-A"
	const bHolderID = "instance-B"

	// Build instance A's reloader. The post-reload hook fires
	// NOTIFY with A's holderID as the payload (production shape
	// in container_config_broadcast.go).
	reloaderA := config.NewConfigReloader(
		config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop())),
		zerolog.Nop(),
	)
	reloaderA.SetLoader(func() error { return nil })
	reloaderA.SetValidator(func() error { return nil })
	reloaderA.SetActivator(func() error { return nil })
	reloaderA.SetPostReloadHook(func() {
		_, _ = db.Exec(`SELECT pg_notify($1, $2)`, channel, aHolderID)
	})

	// Build instance B's reloader with spy counters so the test
	// can assert "B's Reload actually ran".
	var bLoaderCalls, bValidatorCalls, bActivatorCalls int
	var bMu sync.Mutex
	reloaderB := config.NewConfigReloader(
		config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop())),
		zerolog.Nop(),
	)
	reloaderB.SetLoader(func() error {
		bMu.Lock()
		bLoaderCalls++
		bMu.Unlock()
		return nil
	})
	reloaderB.SetValidator(func() error {
		bMu.Lock()
		bValidatorCalls++
		bMu.Unlock()
		return nil
	})
	reloaderB.SetActivator(func() error {
		bMu.Lock()
		bActivatorCalls++
		bMu.Unlock()
		return nil
	})

	// Instance B's listener — mirrors runConfigReloadListener in
	// container_config_broadcast.go: receive on channel, drop
	// self-emitted (payload == own holderID), call Reload on
	// peer-emitted.
	dsn := getTestDBURL()
	listenerB := pq.NewListener(dsn, time.Second, 10*time.Second, nil)
	require.NoError(t, listenerB.Listen(channel))
	defer func() { _ = listenerB.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case n, ok := <-listenerB.Notify:
				if !ok {
					return
				}
				if n == nil || n.Extra == bHolderID {
					continue
				}
				_ = reloaderB.Reload()
			}
		}
	}()

	// Give the LISTEN handshake a moment.
	time.Sleep(200 * time.Millisecond)

	// Fire A's reload. The post-reload hook NOTIFYs; B's listener
	// receives + calls Reload locally.
	require.NoError(t, reloaderA.Reload())

	// Wait up to 2s for B's reload to fire (LISTEN delivery is
	// sub-millisecond in healthy postgres + B's reload is in-
	// process). Poll the spy counters rather than fixed-sleep so
	// fast machines exit quickly.
	deadline := time.After(2 * time.Second)
	for {
		bMu.Lock()
		gotLoader := bLoaderCalls
		bMu.Unlock()
		if gotLoader >= 1 {
			break
		}
		select {
		case <-deadline:
			bMu.Lock()
			t.Fatalf("instance B's Reload was never invoked: loader=%d validator=%d activator=%d",
				bLoaderCalls, bValidatorCalls, bActivatorCalls)
			bMu.Unlock()
		case <-time.After(50 * time.Millisecond):
		}
	}

	bMu.Lock()
	defer bMu.Unlock()
	require.Equal(t, 1, bLoaderCalls, "B's loader should fire exactly once")
	require.Equal(t, 1, bValidatorCalls, "B's validator should fire exactly once")
	require.Equal(t, 1, bActivatorCalls, "B's activator should fire exactly once")
}

// TestHorizontalScaling_ConfigReload_NotifyReachesPeerListener
// proves the LISTEN/NOTIFY broadcast wired in Slice 3a actually
// delivers across separate pq.Listener connections.
//
// Procedure: open two pq.Listener sessions on vornik_config_reloaded.
// Fire NOTIFY via a third connection. Both listeners receive the
// notification within a short timeout.
func TestHorizontalScaling_ConfigReload_NotifyReachesPeerListener(t *testing.T) {
	dsn := getTestDBURL()
	const channel = "vornik_config_reloaded"

	makeListener := func() *pq.Listener {
		l := pq.NewListener(dsn, time.Second, 10*time.Second, nil)
		require.NoError(t, l.Listen(channel))
		return l
	}
	listenerA := makeListener()
	defer func() { _ = listenerA.Close() }()
	listenerB := makeListener()
	defer func() { _ = listenerB.Close() }()

	// Give both listeners a moment to land their LISTEN handshake
	// before firing — pq.Listener does the LISTEN async.
	time.Sleep(200 * time.Millisecond)

	// Fire NOTIFY via the main pool.
	db := connectDB(t)
	defer db.Close()
	_, err := db.Exec(`SELECT pg_notify($1, $2)`, channel, "instance-source")
	require.NoError(t, err)

	// Both peer listeners must see the notification within 2s.
	timeout := time.After(2 * time.Second)
	for _, l := range []*pq.Listener{listenerA, listenerB} {
		select {
		case n := <-l.Notify:
			require.NotNil(t, n, "expected notification, got nil")
			assert.Equal(t, channel, n.Channel)
			assert.Equal(t, "instance-source", n.Extra)
		case <-timeout:
			t.Fatal("listener didn't receive notification within 2s")
		}
	}
}
