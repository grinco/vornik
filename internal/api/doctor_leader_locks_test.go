package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// stubLockRepo implements persistence.DaemonLeaderLockRepository
// for the doctor-check tests. Only List/Get are exercised; the
// mutation methods panic so unrelated test paths can't silently
// reach for them.
type stubLockRepo struct {
	rows    []*persistence.DaemonLeaderLock
	listErr error
}

func (s *stubLockRepo) Acquire(context.Context, string, string, time.Time, time.Duration) (bool, int64, error) {
	panic("not used")
}
func (s *stubLockRepo) Renew(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	panic("not used")
}
func (s *stubLockRepo) Release(context.Context, string, string) error { panic("not used") }
func (s *stubLockRepo) Get(_ context.Context, _ string) (*persistence.DaemonLeaderLock, error) {
	panic("not used")
}
func (s *stubLockRepo) DeleteExpired(context.Context, string, time.Time) (*persistence.DaemonLeaderLock, error) {
	// (nil, nil) is the DECLARED contract, not a convenience: a release that
	// matches nothing is a refusal, not a missing row. Panicking here would
	// make the double unable to state the contract its package now asserts.
	return nil, nil
}

func (s *stubLockRepo) List(_ context.Context) ([]*persistence.DaemonLeaderLock, error) {
	return s.rows, s.listErr
}

// TestCheckLeaderLocksHealth_UnwiredReturnsOK confirms the
// check doesn't blow up on a deployment without leader-election.
func TestCheckLeaderLocksHealth_UnwiredReturnsOK(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkLeaderLocksHealth()
	if got.Status != "SKIPPED" {
		t.Errorf("status = %q, want OK", got.Status)
	}
	if !strings.Contains(got.Message, "not wired") {
		t.Errorf("message should explain why; got %q", got.Message)
	}
}

// TestCheckLeaderLocksHealth_EmptyTableOK: a fresh deployment
// hasn't acquired any locks yet — that's fine, not a problem.
func TestCheckLeaderLocksHealth_EmptyTableOK(t *testing.T) {
	h := &DoctorHandlers{leaderLockRepo: &stubLockRepo{}}
	got := h.checkLeaderLocksHealth()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK", got.Status)
	}
	if !strings.Contains(got.Message, "fresh deployment") {
		t.Errorf("message should call out the empty-table case; got %q", got.Message)
	}
}

// TestCheckLeaderLocksHealth_AllActiveOK: every row has a
// recent renewed_at and a future expires_at → OK.
func TestCheckLeaderLocksHealth_AllActiveOK(t *testing.T) {
	now := time.Now()
	repo := &stubLockRepo{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "archive_sweeper", HolderID: "host-a:1:abc",
			RenewedAt: now.Add(-5 * time.Second), ExpiresAt: now.Add(55 * time.Second)},
		{WorkerID: "autonomy_manager", HolderID: "host-a:1:abc",
			RenewedAt: now.Add(-1 * time.Second), ExpiresAt: now.Add(59 * time.Second)},
	}}
	h := &DoctorHandlers{leaderLockRepo: repo}
	got := h.checkLeaderLocksHealth()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK; msg=%q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "2 worker") {
		t.Errorf("should report worker count; got %q", got.Message)
	}
	if len(got.Items) != 2 {
		t.Errorf("Items count = %d, want 2", len(got.Items))
	}
	for _, item := range got.Items {
		if !strings.HasPrefix(item, "[ACTIVE]") {
			t.Errorf("Item should be ACTIVE; got %q", item)
		}
	}
}

// TestCheckLeaderLocksHealth_StaleRowWarns: a row whose
// renewed_at is older than one full TTL → WARNING. The holder
// is alive (expires_at still in future) but hasn't been
// renewing — pointing at a stuck renewal loop.
func TestCheckLeaderLocksHealth_StaleRowWarns(t *testing.T) {
	now := time.Now()
	staleAge := leaderelection.DefaultTTL + 5*time.Second // past the threshold
	repo := &stubLockRepo{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "stuck_worker", HolderID: "host-a:1:abc",
			RenewedAt: now.Add(-staleAge),
			ExpiresAt: now.Add(5 * time.Second)}, // still valid
		{WorkerID: "healthy_worker", HolderID: "host-a:1:abc",
			RenewedAt: now.Add(-5 * time.Second), ExpiresAt: now.Add(55 * time.Second)},
	}}
	h := &DoctorHandlers{leaderLockRepo: repo}
	got := h.checkLeaderLocksHealth()
	if got.Status != "WARNING" {
		t.Errorf("status = %q, want WARNING", got.Status)
	}
	foundStale := false
	for _, item := range got.Items {
		if strings.HasPrefix(item, "[STALE]") && strings.Contains(item, "stuck_worker") {
			foundStale = true
		}
	}
	if !foundStale {
		t.Errorf("stuck_worker row should be classified STALE; items=%v", got.Items)
	}
}

// TestCheckLeaderLocksHealth_ExpiredRowErrors: expires_at in
// the past → ERROR. No daemon is currently the leader for that
// worker; operators must investigate why no replica is picking
// it up.
func TestCheckLeaderLocksHealth_ExpiredRowErrors(t *testing.T) {
	now := time.Now()
	repo := &stubLockRepo{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "dead_worker", HolderID: "host-dead:1:abc",
			RenewedAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(-1 * time.Hour)},
		{WorkerID: "live_worker", HolderID: "host-a:1:abc",
			RenewedAt: now.Add(-5 * time.Second), ExpiresAt: now.Add(55 * time.Second)},
	}}
	h := &DoctorHandlers{leaderLockRepo: repo}
	got := h.checkLeaderLocksHealth()
	if got.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR (expired row)", got.Status)
	}
	if !strings.Contains(got.Message, "1 leader-lock row") || !strings.Contains(got.Message, "expires_at") {
		t.Errorf("message should call out expired count; got %q", got.Message)
	}
}

// TestCheckLeaderLocksHealth_ListErrorSurfaced: repo failure
// downgrades to WARNING with the error message so the operator
// can debug the DB connectivity / permissions issue.
func TestCheckLeaderLocksHealth_ListErrorSurfaced(t *testing.T) {
	h := &DoctorHandlers{leaderLockRepo: &stubLockRepo{listErr: errors.New("connection refused")}}
	got := h.checkLeaderLocksHealth()
	if got.Status != "WARNING" {
		t.Errorf("status = %q, want WARNING", got.Status)
	}
	if !strings.Contains(got.Message, "connection refused") {
		t.Errorf("message should propagate repo error; got %q", got.Message)
	}
}

// TestClassifyLeaderLock_Buckets pins the classification
// function directly so future tweaks (different thresholds,
// extra states) can be regression-tested without spinning up
// the full DoctorCheck.
func TestClassifyLeaderLock_Buckets(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		row  *persistence.DaemonLeaderLock
		want string
	}{
		{
			"active",
			&persistence.DaemonLeaderLock{HolderID: "h", RenewedAt: now.Add(-1 * time.Second), ExpiresAt: now.Add(59 * time.Second)},
			"ACTIVE",
		},
		{
			"stale (renew gap > TTL but still valid)",
			&persistence.DaemonLeaderLock{HolderID: "h", RenewedAt: now.Add(-2 * leaderelection.DefaultTTL), ExpiresAt: now.Add(5 * time.Second)},
			"STALE",
		},
		{
			"expired",
			&persistence.DaemonLeaderLock{HolderID: "h", RenewedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-1 * time.Hour)},
			"EXPIRED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyLeaderLock(tc.row, now)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHumanLeaderLockDuration_Buckets covers the duration
// pretty-printer the items column uses.
func TestHumanLeaderLockDuration_Buckets(t *testing.T) {
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
		got := humanLeaderLockDuration(tc.d)
		if got != tc.want {
			t.Errorf("humanLeaderLockDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// Issue grinco/vornik-enterprise#60, reported 2026-09-23 against CE 2026.9.6.
//
// A Community operator got ERROR on every doctor run for six rows the
// deployment STRUCTURALLY cannot reacquire: three Enterprise-only subsystems
// absent from a CE build, a telegram poller with telegram disabled, a scanner
// behind an off feature flag, and a per-project elector whose project was
// deleted. EXPIRED was a true statement about each row; ERROR was the wrong
// severity, because ERROR means "a leader is missing and one should be there"
// and here none should be, by configuration. With no release verb and editing
// the table by hand ruled out, the check had no reachable green state — and a
// check with no reachable green state is one the operator learns to ignore.
//
// The set that answers it is the electors this process actually constructed:
// a nil elector when a flag is off, an absent EE subsystem in CE, and a
// deleted project's elector are all absent from it by construction, with no
// hand-kept list of "EE-only worker names" to drift.
func TestCheckLeaderLocksHealth_ExpiredButNotWiredHereIsOrphanedNotError(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	repo := &stubLockRepo{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "blackbox_detector", HolderID: "gone", RenewedAt: past, ExpiresAt: past},
		{WorkerID: "telegram_poller", HolderID: "gone", RenewedAt: past, ExpiresAt: past},
	}}
	h := &DoctorHandlers{
		leaderLockRepo: repo,
		// Neither worker is wired in this build/config.
		wiredWorkerIDs: func() []string { return []string{"autonomy", "retention_sweeper"} },
	}

	got := h.checkLeaderLocksHealth()
	if got.Status == "ERROR" {
		t.Fatalf("expired rows for workers this deployment never wires must not be ERROR; got %q: %s",
			got.Status, got.Message)
	}
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING (orphaned rows are reported, not fatal)", got.Status)
	}
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "ORPHANED") {
		t.Errorf("orphaned rows are not labelled as such; items:\n%s", joined)
	}
	if !strings.Contains(got.Message, "leader-lock release") {
		t.Errorf("the message must name the remediation; got: %s", got.Message)
	}
}

// The severity-preservation half, and the reason this fix is not just
// "downgrade the noisy check": a worker this build DOES wire, whose lease has
// expired, is the genuine failure the check was written for and keeps ERROR.
func TestCheckLeaderLocksHealth_ExpiredAndWiredHereStaysError(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	repo := &stubLockRepo{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "autonomy", HolderID: "gone", RenewedAt: past, ExpiresAt: past},
	}}
	h := &DoctorHandlers{
		leaderLockRepo: repo,
		wiredWorkerIDs: func() []string { return []string{"autonomy"} },
	}

	got := h.checkLeaderLocksHealth()
	if got.Status != "ERROR" {
		t.Fatalf("status = %q, want ERROR: a wired worker holding no lease is the real failure", got.Status)
	}
}

// Without the accessor the check cannot distinguish the two, so it must keep
// the old conservative behaviour rather than silently calling everything
// orphaned — the fail-safe direction for a check whose whole point is severity.
func TestCheckLeaderLocksHealth_NoWiringAccessorKeepsErrorSeverity(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	repo := &stubLockRepo{rows: []*persistence.DaemonLeaderLock{
		{WorkerID: "blackbox_detector", HolderID: "gone", RenewedAt: past, ExpiresAt: past},
	}}
	h := &DoctorHandlers{leaderLockRepo: repo} // no wiredWorkerIDs

	got := h.checkLeaderLocksHealth()
	if got.Status != "ERROR" {
		t.Fatalf("status = %q, want ERROR when the wiring set is unknown", got.Status)
	}
}

// TestDaemonLeaderLockDoubles_StateTheMissContract — required by the
// repo-double conformance gate, and the gate is right to require it.
//
// A double for a single-entity lookup states what production does when nothing
// matches. When that statement is wrong, every test of the caller's miss path
// takes a branch production never takes. DeleteExpired's contract is
// MissNilNil and NOT MissErrNotFound, which is the distinction worth pinning:
// a release that matches nothing is the operator being told "no" — unknown
// worker, row no longer expired, someone else first — never the database
// reporting a fault.
func TestDaemonLeaderLockDoubles_StateTheMissContract(t *testing.T) {
	ctx := context.Background()
	repotest.AssertMiss(t, "DaemonLeaderLockRepository.DeleteExpired",
		func() (*persistence.DaemonLeaderLock, error) {
			return (&stubLockRepo{}).DeleteExpired(ctx, "absent", time.Now())
		})
	repotest.AssertMiss(t, "DaemonLeaderLockRepository.DeleteExpired",
		func() (*persistence.DaemonLeaderLock, error) {
			return stubLeaderLocks{}.DeleteExpired(ctx, "absent", time.Now())
		})
}
