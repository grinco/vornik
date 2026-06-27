package ui

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// stubLeaderLockSource implements LeaderLockSource with a
// programmable row set + optional error.
type stubLeaderLockSource struct {
	rows []*persistence.DaemonLeaderLock
	err  error
}

func (s *stubLeaderLockSource) List(_ context.Context) ([]*persistence.DaemonLeaderLock, error) {
	return s.rows, s.err
}

// stubClusterNodeSource implements ClusterNodeSource with a
// programmable node set + optional error.
type stubClusterNodeSource struct {
	nodes []*persistence.ClusterNode
	err   error
}

func (s *stubClusterNodeSource) List(_ context.Context) ([]*persistence.ClusterNode, error) {
	return s.nodes, s.err
}

func TestSummarizeFleet_HealthAndCounts(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	nodes := []*persistence.ClusterNode{
		{InstanceID: "job-1", Profile: "all", Version: "v1", Address: "10.0.0.1:8080", LastSeen: now.Add(-5 * time.Second)},
		{InstanceID: "dmz-1", Profile: "webhook", Version: "v1", Address: "10.0.9.4:8080", LastSeen: now.Add(-90 * time.Second)},
	}
	fleet, counts, skew := summarizeFleet(nodes, now)
	require.Len(t, fleet, 2)
	byID := map[string]FleetNode{}
	for _, f := range fleet {
		byID[f.InstanceID] = f
	}
	assert.Equal(t, "ACTIVE", byID["job-1"].Health, "5s-old node is fresh")
	assert.Equal(t, "STALE", byID["dmz-1"].Health, ">45s-old node is stale")
	assert.Equal(t, 2, counts.Nodes)
	assert.Equal(t, 1, counts.Active)
	assert.Equal(t, 1, counts.Stale)
	assert.False(t, skew, "identical versions must not flag skew")
}

func TestSummarizeFleet_VersionSkew(t *testing.T) {
	now := time.Now()
	nodes := []*persistence.ClusterNode{
		{InstanceID: "a", Version: "2026.6.0", LastSeen: now},
		{InstanceID: "b", Version: "2026.6.1", LastSeen: now},
		{InstanceID: "c", Version: "", LastSeen: now}, // empty version ignored
	}
	_, _, skew := summarizeFleet(nodes, now)
	assert.True(t, skew, "distinct non-empty versions must flag rolling-deploy skew")
}

// TestAdminHealthCluster_FleetShowsWebhookNode is the regression for the
// reported gap: a webhook node holds NO leases, so the lease-derived tables
// never showed it. The fleet table (cluster_nodes) must surface it.
func TestAdminHealthCluster_FleetShowsWebhookNode(t *testing.T) {
	fleetSrc := &stubClusterNodeSource{nodes: []*persistence.ClusterNode{
		{InstanceID: "dmz-1", Profile: "webhook", Version: "2026.6.0", Address: "10.0.9.4:8080", LastSeen: time.Now()},
	}}
	// Leases wired but EMPTY — exactly the real shape: a DB node with
	// leader-locks but a webhook node that owns none of them.
	s := NewServer(WithLogger(quietLogger()), WithLeaderLockSource(&stubLeaderLockSource{}), WithClusterNodeSource(fleetSrc))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	body := rec.Body.String()
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, body, "dmz-1", "fleet must list the webhook node by instance id")
	assert.Contains(t, body, "webhook", "fleet must show the node profile")
	assert.Contains(t, body, "10.0.9.4:8080", "fleet must show the advertised address")
}

// TestAdminHealthCluster_UnwiredRendersPlaceholder: an operator
// running SQLite (or pre-migration-57 deployment) should see the
// "single-process deployment" hint, not a stack trace.
func TestAdminHealthCluster_UnwiredRendersPlaceholder(t *testing.T) {
	s := NewServer(WithLogger(quietLogger()))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	assert.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Single-process")
	assert.Contains(t, body, "daemon_leader_locks")
}

// TestAdminHealthCluster_EmptyTableRendersEmptyTables: source
// wired but no rows yet — fresh deployment that hasn't had any
// worker acquire a lock.
func TestAdminHealthCluster_EmptyTableRendersEmptyTables(t *testing.T) {
	src := &stubLeaderLockSource{}
	s := NewServer(WithLogger(quietLogger()), WithLeaderLockSource(src))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	body := rec.Body.String()
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, body, "No lease holders yet")
	assert.Contains(t, body, "No worker rows yet")
}

// TestAdminHealthCluster_AllActive: every worker on a single
// node, every row fresh. Summary should show all ACTIVE + 1
// node.
func TestAdminHealthCluster_AllActive(t *testing.T) {
	now := time.Now()
	src := &stubLeaderLockSource{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "archive_sweeper", HolderID: "host-a:1:nonce", RenewedAt: now.Add(-5 * time.Second), ExpiresAt: now.Add(55 * time.Second), AcquiredAt: now.Add(-1 * time.Minute)},
		{WorkerID: "autonomy_manager", HolderID: "host-a:1:nonce", RenewedAt: now.Add(-2 * time.Second), ExpiresAt: now.Add(58 * time.Second), AcquiredAt: now.Add(-1 * time.Minute)},
	}}
	s := NewServer(WithLogger(quietLogger()), WithLeaderLockSource(src))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	body := rec.Body.String()
	assert.Equal(t, 200, rec.Code)
	// Summary counts (rendered as "active: 2", "expired: 0", etc.)
	assert.Contains(t, body, "active</span>: 2")
	assert.Contains(t, body, "expired</span>: 0")
	assert.Contains(t, body, "lease-holders</span>: 1")
	// Each worker_id appears in the workers table.
	assert.Contains(t, body, "archive_sweeper")
	assert.Contains(t, body, "autonomy_manager")
	// The holder_id appears.
	assert.Contains(t, body, "host-a:1:nonce")
}

// TestAdminHealthCluster_MixedStatuses: stale + expired
// classification flows into the summary + per-row badges. Node
// aggregate should be EXPIRED (worst dominates).
func TestAdminHealthCluster_MixedStatuses(t *testing.T) {
	now := time.Now()
	staleAge := leaderelection.DefaultTTL + 5*time.Second
	src := &stubLeaderLockSource{rows: []*persistence.DaemonLeaderLock{
		// Stale: renewed long ago, lease still valid
		{WorkerID: "stale_worker", HolderID: "host-mixed:1:n", RenewedAt: now.Add(-staleAge), ExpiresAt: now.Add(5 * time.Second), AcquiredAt: now.Add(-1 * time.Hour)},
		// Expired: lease past
		{WorkerID: "dead_worker", HolderID: "host-mixed:1:n", RenewedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-1 * time.Hour), AcquiredAt: now.Add(-2 * time.Hour)},
		// Active: fresh
		{WorkerID: "live_worker", HolderID: "host-mixed:1:n", RenewedAt: now.Add(-5 * time.Second), ExpiresAt: now.Add(55 * time.Second), AcquiredAt: now.Add(-1 * time.Minute)},
	}}
	s := NewServer(WithLogger(quietLogger()), WithLeaderLockSource(src))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	body := rec.Body.String()
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, body, "active</span>: 1")
	assert.Contains(t, body, "stale</span>: 1")
	assert.Contains(t, body, "expired</span>: 1")
	// All three badges should render
	assert.True(t, strings.Contains(body, ">ACTIVE<"), "ACTIVE badge missing")
	assert.True(t, strings.Contains(body, ">STALE<"), "STALE badge missing")
	assert.True(t, strings.Contains(body, ">EXPIRED<"), "EXPIRED badge missing")
}

// TestAdminHealthCluster_NodeGroupsByHolder: rows on different
// holders → multiple node entries with distinct worker lists.
func TestAdminHealthCluster_NodeGroupsByHolder(t *testing.T) {
	now := time.Now()
	src := &stubLeaderLockSource{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "w_a", HolderID: "host-1:1:n", RenewedAt: now, ExpiresAt: now.Add(60 * time.Second), AcquiredAt: now},
		{WorkerID: "w_b", HolderID: "host-2:2:n", RenewedAt: now, ExpiresAt: now.Add(60 * time.Second), AcquiredAt: now},
		{WorkerID: "w_c", HolderID: "host-1:1:n", RenewedAt: now, ExpiresAt: now.Add(60 * time.Second), AcquiredAt: now},
	}}
	s := NewServer(WithLogger(quietLogger()), WithLeaderLockSource(src))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	body := rec.Body.String()
	assert.Contains(t, body, "lease-holders</span>: 2")
	assert.Contains(t, body, "host-1:1:n")
	assert.Contains(t, body, "host-2:2:n")
}

// TestAdminHealthCluster_ErrorRenders: List failure surfaces the
// error banner. The summary tables don't render in that case
// (no rows to process).
func TestAdminHealthCluster_ErrorRenders(t *testing.T) {
	src := &stubLeaderLockSource{err: errors.New("connection refused")}
	s := NewServer(WithLogger(quietLogger()), WithLeaderLockSource(src))
	req := httptest.NewRequest("GET", "/ui/admin/health/cluster", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthCluster(rec, req)

	body := rec.Body.String()
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, body, "connection refused")
}

// TestBuildClusterWorkers_Classification pins the per-worker
// classification (ACTIVE / STALE / EXPIRED) directly. Bypasses
// the HTTP render so the test stays cheap to maintain.
func TestBuildClusterWorkers_Classification(t *testing.T) {
	now := time.Now()
	rows := []*persistence.DaemonLeaderLock{
		{WorkerID: "active", HolderID: "h", RenewedAt: now.Add(-1 * time.Second), ExpiresAt: now.Add(59 * time.Second)},
		{WorkerID: "stale", HolderID: "h", RenewedAt: now.Add(-2 * leaderelection.DefaultTTL), ExpiresAt: now.Add(5 * time.Second)},
		{WorkerID: "expired", HolderID: "h", RenewedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-1 * time.Hour)},
	}
	workers, counts := buildClusterWorkers(rows, now)
	require.Len(t, workers, 3)
	// Sorted alphabetically.
	assert.Equal(t, "active", workers[0].WorkerID)
	assert.Equal(t, "expired", workers[1].WorkerID)
	assert.Equal(t, "stale", workers[2].WorkerID)

	statusByWorker := map[string]string{}
	for _, w := range workers {
		statusByWorker[w.WorkerID] = w.Status
	}
	assert.Equal(t, "ACTIVE", statusByWorker["active"])
	assert.Equal(t, "STALE", statusByWorker["stale"])
	assert.Equal(t, "EXPIRED", statusByWorker["expired"])

	assert.Equal(t, 1, counts.Active)
	assert.Equal(t, 1, counts.Stale)
	assert.Equal(t, 1, counts.Expired)
}

// TestBuildClusterNodes_WorstWins: when one node owns workers of
// mixed statuses, the node-level aggregate is the worst (EXPIRED
// > STALE > ACTIVE). Lets the operator see "this daemon has a
// problem" at the node level without scanning every worker.
func TestBuildClusterNodes_WorstWins(t *testing.T) {
	now := time.Now()
	workers := []AdminClusterWorker{
		{WorkerID: "a", HolderID: "node-1", Status: "ACTIVE", RenewedAt: now.Add(-1 * time.Second)},
		{WorkerID: "b", HolderID: "node-1", Status: "STALE", RenewedAt: now.Add(-1 * time.Hour)},
		{WorkerID: "c", HolderID: "node-1", Status: "EXPIRED", RenewedAt: now.Add(-2 * time.Hour)},
		{WorkerID: "d", HolderID: "node-2", Status: "ACTIVE", RenewedAt: now.Add(-1 * time.Second)},
		{WorkerID: "e", HolderID: "node-2", Status: "STALE", RenewedAt: now.Add(-1 * time.Hour)},
	}
	nodes := buildClusterNodes(workers, now)
	require.Len(t, nodes, 2)
	statusByNode := map[string]string{}
	for _, n := range nodes {
		statusByNode[n.HolderID] = n.Status
	}
	assert.Equal(t, "EXPIRED", statusByNode["node-1"], "EXPIRED must dominate")
	assert.Equal(t, "STALE", statusByNode["node-2"], "STALE > ACTIVE when no EXPIRED present")
}

// TestBuildClusterNodes_LastRenewedIsMax: the node-level
// "last renewed" timestamp should be the most recent across the
// node's workers — operators infer "is this daemon ticking?"
// from that single number.
func TestBuildClusterNodes_LastRenewedIsMax(t *testing.T) {
	now := time.Now()
	recent := now.Add(-2 * time.Second)
	stale := now.Add(-5 * time.Minute)
	workers := []AdminClusterWorker{
		{WorkerID: "a", HolderID: "h", Status: "ACTIVE", RenewedAt: stale},
		{WorkerID: "b", HolderID: "h", Status: "ACTIVE", RenewedAt: recent},
	}
	nodes := buildClusterNodes(workers, now)
	require.Len(t, nodes, 1)
	assert.True(t, nodes[0].LastRenewed.Equal(recent),
		"node-level LastRenewed should be the MAX across workers, got %v", nodes[0].LastRenewed)
}

// TestHumanClusterDuration_Buckets pins the renderer used in the
// rendered template. A regression here would silently move
// numbers in the cluster page (e.g. "1m 30s" → "90s" or "1m").
func TestHumanClusterDuration_Buckets(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "<1s"},
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m 30s"},
		{3 * time.Minute, "3m"},
		{125 * time.Minute, "2h 5m"},
		{2 * time.Hour, "2h"},
		{50 * time.Hour, "2d 2h"},
	}
	for _, tc := range cases {
		got := humanClusterDuration(tc.d)
		if got != tc.want {
			t.Errorf("humanClusterDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
