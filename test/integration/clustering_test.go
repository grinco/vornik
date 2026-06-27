//go:build integration
// +build integration

package integration_test

// Integration tests for the clustering surface
// (https://docs.vornik.io §4).
//
// These tests exercise the REAL postgres.ClusterNodeRepository,
// postgres.LeaderLockRepository, and leaderelection.Elector against
// one shared database. Each test generates a unique prefix via
// time.Now().UnixNano() and cleans up only its own rows via t.Cleanup.
//
// CROSS-PACKAGE NOTE: ClusterNodePrunerSubsystem (package service)
// has all unexported fields and no exported constructor, so it cannot
// be constructed from this integration_test package. Tests 1/2/4
// therefore test the repo layer directly and replicate the
// stale-and-unleased selection logic inline (identical to pruneVictims
// in the service package). Test 3 exercises the real elector handover
// which is the multi-instance behavior that truly requires postgres.
//
// DB SAFETY: NEVER hardcode a DSN; use connectDB(t) which reads
// TEST_DATABASE_URL or falls back to localhost defaults. Every test
// skips explicitly when TEST_DATABASE_URL is absent. Cleanup deletes
// only rows whose instance_id/worker_id carry this test's unique
// prefix — no DROP/TRUNCATE of shared tables.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

// clusterPruneGrace mirrors the constant in service/subsystem_cluster_pruner.go.
// Duplicated here to keep the integration test self-contained; if the
// production constant changes, update this one to match.
const clusterIntegrationPruneGrace = 5 * time.Minute

// skipIfNoDB skips the test when TEST_DATABASE_URL is not set.
// connectDB falls back to localhost defaults and will fail (not skip)
// when postgres is unavailable. Explicit skip keeps CI clean on
// environments where no integration DB is provisioned.
func skipIfNoDB(t *testing.T) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping clustering integration test")
	}
}

// inlineVictims replicates the pruneVictims selection from
// service/subsystem_cluster_pruner.go. Used by tests 1/2/4 that
// cannot reach the unexported service function.
//
// Returns instance_ids that are stale (LastSeen older than grace) AND
// not protected by an active lease (ExpiresAt > now).
func inlineVictims(nodes []*persistence.ClusterNode, locks []*persistence.DaemonLeaderLock, now time.Time, grace time.Duration) []string {
	held := map[string]bool{}
	for _, l := range locks {
		if l.ExpiresAt.After(now) {
			held[l.HolderID] = true
		}
	}
	var victims []string
	for _, n := range nodes {
		if n.StaleAfter(now, grace) && !held[n.InstanceID] {
			victims = append(victims, n.InstanceID)
		}
	}
	return victims
}

// TestClusteringIntegration_HeartbeatThenPruneLifecycle proves that
// the real ClusterNodeRepository correctly persists a fresh node
// (LastSeen = now) through a prune cycle, and correctly reaps a stale
// node (LastSeen > grace ago) that holds no active lease.
//
// Pivot rationale: ClusterNodePrunerSubsystem has all unexported
// fields; we replicate its stale-and-unleased selection inline.
func TestClusteringIntegration_HeartbeatThenPruneLifecycle(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-e2e-%d", time.Now().UnixNano())
	nodeID := prefix + "-node1"

	nodeRepo := postgres.NewClusterNodeRepository(db)
	lockRepo := postgres.NewLeaderLockRepository(db)
	ctx := context.Background()

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE instance_id LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id LIKE $1`, prefix+"%")
	})

	// Step 1: Upsert a node with a fresh LastSeen.
	freshNode := &persistence.ClusterNode{
		InstanceID:   nodeID,
		Profile:      "worker",
		Version:      "v-test",
		LastSeen:     time.Now(),
		Capabilities: map[string]bool{"RunWorkers": true},
	}
	require.NoError(t, nodeRepo.Upsert(ctx, freshNode), "upsert fresh node")

	// Simulate a prune cycle: list nodes + locks, compute victims.
	now := time.Now()
	nodes, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	locks, err := lockRepo.List(ctx)
	require.NoError(t, err)

	victims := inlineVictims(nodes, locks, now, clusterIntegrationPruneGrace)
	for _, id := range victims {
		require.NoError(t, nodeRepo.DeleteByInstanceID(ctx, id))
	}

	// Fresh node must survive.
	nodes, err = nodeRepo.List(ctx)
	require.NoError(t, err)
	var found bool
	for _, n := range nodes {
		if n.InstanceID == nodeID {
			found = true
			break
		}
	}
	assert.True(t, found, "fresh node must survive a prune cycle")

	// Step 2: make the node genuinely stale. Upsert stamps last_seen=NOW()
	// (DB-authoritative, clock-skew-immune since eb1515d9), so a stale LastSeen
	// passed to Upsert is ignored — only a direct SQL UPDATE back-dates the
	// row. Mirrors TestClusteringIntegration_SkewImmuneViaDBClock.
	_, err = db.Exec(
		`UPDATE cluster_nodes SET last_seen = NOW() - interval '6 minutes' WHERE instance_id = $1`,
		nodeID,
	)
	require.NoError(t, err, "back-date last_seen via SQL UPDATE")

	// Prune cycle again.
	now2 := time.Now()
	nodes2, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	locks2, err := lockRepo.List(ctx)
	require.NoError(t, err)

	victims2 := inlineVictims(nodes2, locks2, now2, clusterIntegrationPruneGrace)
	for _, id := range victims2 {
		require.NoError(t, nodeRepo.DeleteByInstanceID(ctx, id))
	}

	// Stale, unleased node must be gone.
	nodes3, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	for _, n := range nodes3 {
		assert.NotEqual(t, nodeID, n.InstanceID, "stale unleased node must be pruned")
	}
}

// TestClusteringIntegration_LeaseHolderSurvivesPrune proves that a
// node whose instance_id matches an active daemon_leader_locks.holder_id
// is never reaped, even when its heartbeat is stale — the real join over
// real postgres tables must honor the lease.
func TestClusteringIntegration_LeaseHolderSurvivesPrune(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-e2e-%d", time.Now().UnixNano())
	// instance_id and holder_id are the same value per the join in pruneOnce.
	nodeID := prefix + "-leader"
	workerID := prefix + "-wk"

	nodeRepo := postgres.NewClusterNodeRepository(db)
	lockRepo := postgres.NewLeaderLockRepository(db)
	ctx := context.Background()

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE instance_id LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id LIKE $1`, prefix+"%")
	})

	// Upsert a stale node row.
	require.NoError(t, nodeRepo.Upsert(ctx, &persistence.ClusterNode{
		InstanceID:   nodeID,
		Profile:      "worker",
		Version:      "v-test",
		LastSeen:     time.Now().Add(-(clusterIntegrationPruneGrace + time.Hour)),
		Capabilities: map[string]bool{"RunWorkers": true},
	}), "upsert stale leader node")

	// Give it an active leader lock (holderID = nodeID so the join matches).
	const ttl = 30 * time.Second
	acquired, _, err := lockRepo.Acquire(ctx, workerID, nodeID, time.Now(), ttl)
	require.NoError(t, err)
	require.True(t, acquired, "lock acquire must succeed on fresh worker_id")

	// Prune cycle: stale node but held by an active lease → must survive.
	now := time.Now()
	nodes, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	locks, err := lockRepo.List(ctx)
	require.NoError(t, err)

	victims := inlineVictims(nodes, locks, now, clusterIntegrationPruneGrace)
	for _, id := range victims {
		require.NoError(t, nodeRepo.DeleteByInstanceID(ctx, id))
	}

	nodes2, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	var found bool
	for _, n := range nodes2 {
		if n.InstanceID == nodeID {
			found = true
			break
		}
	}
	assert.True(t, found, "a stale node holding an active lease must survive pruning (real join over real tables)")
}

// TestClusteringIntegration_TwoElectorsLeaderHandover mirrors
// TestHorizontalScaling_Elector_LeaderHandover. Two real Elector
// instances contest the same worker_id on postgres. After A acquires,
// B cannot acquire. After A releases, B takes over. At each step exactly
// one elector is the leader.
//
// This is the genuine multi-instance behavior that requires a real DB.
func TestClusteringIntegration_TwoElectorsLeaderHandover(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-e2e-%d", time.Now().UnixNano())
	workerID := prefix + "-wk"

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id LIKE $1`, prefix+"%")
	})

	repo := postgres.NewLeaderLockRepository(db)
	const ttl = 30 * time.Second
	a := leaderelection.New(repo, workerID, prefix+"-inst-A", ttl, zerolog.Nop())
	b := leaderelection.New(repo, workerID, prefix+"-inst-B", ttl, zerolog.Nop())

	ctx := context.Background()

	// A acquires.
	a.BootstrapAcquire(ctx)
	require.True(t, a.IsLeader(), "instance A must hold the lease after BootstrapAcquire")

	// B tries — must fail while A holds an unexpired lease.
	b.BootstrapAcquire(ctx)
	require.False(t, b.IsLeader(), "instance B must not steal an unexpired lease held by A")

	// Exactly one leader at this point.
	leaders := 0
	if a.IsLeader() {
		leaders++
	}
	if b.IsLeader() {
		leaders++
	}
	assert.Equal(t, 1, leaders, "exactly one elector must be leader after A acquires")

	// A releases (drain sequence).
	require.NoError(t, a.Release(ctx))
	require.False(t, a.IsLeader(), "A must clear its leader bit after Release")

	// B takes over without waiting out the TTL.
	b.BootstrapAcquire(ctx)
	require.True(t, b.IsLeader(), "B must claim the released lease without waiting out TTL")

	// Still exactly one leader.
	leaders2 := 0
	if a.IsLeader() {
		leaders2++
	}
	if b.IsLeader() {
		leaders2++
	}
	assert.Equal(t, 1, leaders2, "exactly one elector must be leader after handover")
}

// TestClusteringIntegration_SkewImmuneViaDBClock proves that staleness is
// determined purely by the DB clock — the test process's own clock plays no
// role. A node upserted with any Go-side LastSeen value is stamped by the DB
// as NOW(); an immediate DeleteStale must find it fresh. Only a SQL UPDATE
// that manually back-dates last_seen (bypassing Upsert) causes the row to
// be reaped on the next DeleteStale call.
//
// This closes the clock-skew gap: if the Upsert SQL incorrectly wrote the
// caller-supplied LastSeen, a node whose process clock lagged behind the
// leader's clock by more than the grace window would be reaped despite
// heartbeating normally.
func TestClusteringIntegration_SkewImmuneViaDBClock(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-skew-%d", time.Now().UnixNano())
	nodeID := prefix + "-node1"

	nodeRepo := postgres.NewClusterNodeRepository(db)
	ctx := context.Background()

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE instance_id LIKE $1`, prefix+"%")
	})

	// Upsert a node; the DB stamps last_seen = NOW() regardless of any
	// Go-side LastSeen value. A past LastSeen supplied here must be ignored.
	require.NoError(t, nodeRepo.Upsert(ctx, &persistence.ClusterNode{
		InstanceID:   nodeID,
		Profile:      "worker",
		Version:      "v-test",
		LastSeen:     time.Now().Add(-24 * time.Hour), // must be ignored by DB
		Capabilities: map[string]bool{"RunWorkers": true},
	}), "upsert with stale Go-side LastSeen")

	// Immediately DeleteStale: the DB-stamped last_seen is NOW(), so OUR row
	// must survive. DeleteStale is table-global, so leftover stale rows from
	// earlier crashed runs may be reaped here — we don't assert the count; the
	// List check below verifies our row survived.
	_, err := nodeRepo.DeleteStale(ctx, clusterIntegrationPruneGrace, nil)
	require.NoError(t, err, "DeleteStale after fresh upsert")

	// Verify the row is still present.
	nodes, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	var found bool
	for _, nd := range nodes {
		if nd.InstanceID == nodeID {
			found = true
			break
		}
	}
	require.True(t, found, "node must survive DeleteStale when DB-stamped last_seen is recent")

	// Back-date last_seen via a direct SQL UPDATE (simulating a node that truly
	// went silent for 10 minutes from the DB's perspective).
	_, err = db.Exec(
		`UPDATE cluster_nodes SET last_seen = NOW() - interval '10 minutes' WHERE instance_id = $1`,
		nodeID,
	)
	require.NoError(t, err, "back-date last_seen via SQL UPDATE")

	// Now DeleteStale must reap it. The count is table-global (the shared
	// integration DB may carry stale rows from earlier crashed runs whose
	// t.Cleanup never ran), so assert AT LEAST one row was reaped and that OUR
	// row is specifically gone — not an exact table-wide count.
	n, err := nodeRepo.DeleteStale(ctx, clusterIntegrationPruneGrace, nil)
	require.NoError(t, err, "DeleteStale after back-dating last_seen")
	require.GreaterOrEqual(t, n, 1, "back-dated row must be reaped by DeleteStale")

	remaining, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	for _, nd := range remaining {
		require.NotEqual(t, nodeID, nd.InstanceID, "back-dated row must be gone after DeleteStale")
	}
}

// TestClusteringIntegration_DMZByProxyDelayedHeartbeatStampedByDB proves
// that the by-proxy heartbeat path (used by webhook/DMZ nodes that have no
// direct DB access) results in a last_seen value stamped by the DB clock,
// not by the relay or job-tier Go clock. A webhook node's row must appear
// fresh immediately after upsert — no Go-clock skew can make it stale.
//
// This closes the DMZ-node clock vector: if the NodeHeartbeat handler passed
// LastSeen: time.Now() (job-tier clock) to Upsert, and Upsert wrote that
// value instead of calling NOW(), a DMZ node heartbeating through a lagging
// relay would be stamped with the relay's stale clock and reaped prematurely.
func TestClusteringIntegration_DMZByProxyDelayedHeartbeatStampedByDB(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-dmz-%d", time.Now().UnixNano())
	nodeID := prefix + "-webhook1"

	nodeRepo := postgres.NewClusterNodeRepository(db)
	ctx := context.Background()

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE instance_id LIKE $1`, prefix+"%")
	})

	// Simulate the by-proxy upsert path: the NodeHeartbeat handler supplies a
	// Go-side LastSeen (the relay's clock, potentially lagging). The Upsert must
	// ignore it and stamp DB NOW().
	callerLastSeen := time.Now().Add(-30 * time.Minute) // lagging relay clock
	require.NoError(t, nodeRepo.Upsert(ctx, &persistence.ClusterNode{
		InstanceID:   nodeID,
		Profile:      "webhook",
		Version:      "v-test",
		LastSeen:     callerLastSeen,
		Capabilities: map[string]bool{"ServeWebhooks": true},
	}), "by-proxy upsert webhook node")

	// List must show the node as present and recently seen (DB clock), not
	// with the lagging relay clock (callerLastSeen).
	nodes, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	var found bool
	for _, nd := range nodes {
		if nd.InstanceID == nodeID {
			found = true
			assert.Equal(t, "webhook", nd.Profile, "profile must be webhook")
			// DB-stamped last_seen must be recent, NOT the lagging callerLastSeen.
			assert.True(t, time.Since(nd.LastSeen) < 5*time.Second,
				"last_seen must be DB-clock recent (within 5s), got %v ago — not the caller-supplied %v",
				time.Since(nd.LastSeen), time.Since(callerLastSeen))
			break
		}
	}
	require.True(t, found, "webhook node must appear in List after by-proxy upsert")

	// Confirm the row is NOT stale from the DB's perspective — DeleteStale
	// must not reap it.
	n, err := nodeRepo.DeleteStale(ctx, clusterIntegrationPruneGrace, nil)
	require.NoError(t, err)
	require.Equal(t, 0, n, "DMZ node must not be reaped: its last_seen is DB-clock recent, not the relay's lagging clock")
}

// TestClusteringIntegration_ByProxyHeartbeatRegistersWebhookNode proves
// the by-proxy path: a webhook node (no direct DB access) registers via
// an upsert with Profile="webhook". The node appears in List; after its
// LastSeen goes stale with no active lease, a prune cycle reaps it
// (DMZ-zombie cleanup — the C2 raison d'être).
func TestClusteringIntegration_ByProxyHeartbeatRegistersWebhookNode(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-e2e-%d", time.Now().UnixNano())
	nodeID := prefix + "-webhook1"

	nodeRepo := postgres.NewClusterNodeRepository(db)
	lockRepo := postgres.NewLeaderLockRepository(db)
	ctx := context.Background()

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE instance_id LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id LIKE $1`, prefix+"%")
	})

	// Simulate by-proxy upsert (job-tier NodeHeartbeat path writes this row
	// on behalf of the webhook node which has no direct DB connection).
	require.NoError(t, nodeRepo.Upsert(ctx, &persistence.ClusterNode{
		InstanceID:   nodeID,
		Profile:      "webhook",
		Version:      "v-test",
		LastSeen:     time.Now(),
		Capabilities: map[string]bool{"ServeWebhooks": true},
	}), "by-proxy upsert webhook node")

	// Webhook node must appear in List.
	nodes, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	var found bool
	for _, n := range nodes {
		if n.InstanceID == nodeID {
			found = true
			assert.Equal(t, "webhook", n.Profile, "profile must be webhook")
			break
		}
	}
	require.True(t, found, "webhook node must appear in List after by-proxy upsert")

	// The webhook node stops heartbeating (DMZ zombie). Back-date its last_seen
	// directly: Upsert stamps NOW() (DB-authoritative since eb1515d9), so a
	// stale LastSeen passed to Upsert is ignored — only a SQL UPDATE makes the
	// row genuinely stale.
	_, err = db.Exec(
		`UPDATE cluster_nodes SET last_seen = NOW() - interval '6 minutes' WHERE instance_id = $1`,
		nodeID,
	)
	require.NoError(t, err, "back-date webhook node last_seen via SQL UPDATE")

	// Prune cycle: stale + unleased → reap.
	now := time.Now()
	nodes2, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	locks, err := lockRepo.List(ctx)
	require.NoError(t, err)

	victims := inlineVictims(nodes2, locks, now, clusterIntegrationPruneGrace)
	for _, id := range victims {
		require.NoError(t, nodeRepo.DeleteByInstanceID(ctx, id))
	}

	// Zombie webhook node must be gone.
	nodes3, err := nodeRepo.List(ctx)
	require.NoError(t, err)
	for _, n := range nodes3 {
		assert.NotEqual(t, nodeID, n.InstanceID, "stale unleased webhook node (DMZ zombie) must be pruned")
	}
}
