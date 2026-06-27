package leaderelection

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// fakeRepo is an in-memory DaemonLeaderLockRepository that
// mimics the postgres semantics: at most one holder per
// workerID; takeover allowed when the prior holder's
// expires_at has passed.
type fakeRepo struct {
	mu   sync.Mutex
	rows map[string]*persistence.DaemonLeaderLock
	err  error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[string]*persistence.DaemonLeaderLock{}}
}

func (f *fakeRepo) Acquire(_ context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, int64, error) {
	if f.err != nil {
		return false, 0, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.rows[workerID]
	if ok && cur.HolderID != holderID && !cur.ExpiresAt.Before(now) {
		return false, 0, nil
	}
	// Compute epoch: 1 for new row, preserved for same-holder
	// renew, bumped for takeover.
	var newEpoch int64
	switch {
	case !ok:
		newEpoch = 1
	case cur.HolderID == holderID:
		newEpoch = cur.Epoch
	default:
		newEpoch = cur.Epoch + 1
	}
	f.rows[workerID] = &persistence.DaemonLeaderLock{
		WorkerID:   workerID,
		HolderID:   holderID,
		AcquiredAt: now,
		RenewedAt:  now,
		ExpiresAt:  now.Add(ttl),
		Epoch:      newEpoch,
	}
	return true, newEpoch, nil
}

func (f *fakeRepo) Renew(_ context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.rows[workerID]
	if !ok || cur.HolderID != holderID {
		return false, nil
	}
	cur.RenewedAt = now
	cur.ExpiresAt = now.Add(ttl)
	return true, nil
}

func (f *fakeRepo) Release(_ context.Context, workerID, holderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.rows[workerID]; ok && cur.HolderID == holderID {
		cur.ExpiresAt = time.Now().Add(-1 * time.Second)
	}
	return nil
}

func (f *fakeRepo) Get(_ context.Context, workerID string) (*persistence.DaemonLeaderLock, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[workerID]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeRepo) List(_ context.Context) ([]*persistence.DaemonLeaderLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*persistence.DaemonLeaderLock, 0, len(f.rows))
	for _, r := range f.rows {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// TestElector_FirstAcquireSetsLeader: the initial Run iteration
// fires Acquire immediately so workers can gate on IsLeader()
// without waiting one tick.
func TestElector_FirstAcquireSetsLeader(t *testing.T) {
	repo := newFakeRepo()
	e := New(repo, "archive_sweeper", "host-a", 10*time.Second, zerolog.Nop())
	// Call the internal helper directly so we don't have to
	// run the goroutine + race on the ticker.
	e.tryAcquireOrRenew(context.Background())
	if !e.IsLeader() {
		t.Errorf("first acquire should make us leader")
	}
}

// TestElector_LosesLeadershipOnRenewMiss: holder mismatch on
// Renew → leader bit drops on the next tick. Workers stop
// emitting side effects.
func TestElector_LosesLeadershipOnRenewMiss(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	// Pre-seed with a row held by a DIFFERENT daemon.
	repo.rows["w"] = &persistence.DaemonLeaderLock{
		WorkerID: "w", HolderID: "other-daemon",
		AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	e := New(repo, "w", "us", 10*time.Second, zerolog.Nop())
	// Force the "we think we're leader" pre-condition.
	e.mu.Lock()
	e.leader = true
	e.mu.Unlock()
	e.tryAcquireOrRenew(context.Background())
	if e.IsLeader() {
		t.Errorf("renew miss should clear the leader bit")
	}
}

// TestElector_TakesOverExpiredLock: a daemon holding an
// expired row gets evicted; we acquire.
func TestElector_TakesOverExpiredLock(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()
	// Seed an expired row owned by another daemon.
	repo.rows["w"] = &persistence.DaemonLeaderLock{
		WorkerID: "w", HolderID: "dead-daemon",
		AcquiredAt: now.Add(-1 * time.Hour),
		RenewedAt:  now.Add(-1 * time.Hour),
		ExpiresAt:  now.Add(-30 * time.Second),
	}
	e := New(repo, "w", "us", 10*time.Second, zerolog.Nop())
	e.tryAcquireOrRenew(context.Background())
	if !e.IsLeader() {
		t.Errorf("should take over expired lock")
	}
	// And the row's holder_id should now be us.
	row, _ := repo.Get(context.Background(), "w")
	if row.HolderID != "us" {
		t.Errorf("holder_id should be 'us'; got %q", row.HolderID)
	}
}

// TestElector_ErrorPropagatesToLastError: a repo failure
// keeps the leader bit unchanged (false here) AND stashes the
// error so the doctor check / admin UI can surface it.
func TestElector_ErrorPropagatesToLastError(t *testing.T) {
	repo := newFakeRepo()
	repo.err = errors.New("db down")
	e := New(repo, "w", "us", 10*time.Second, zerolog.Nop())
	e.tryAcquireOrRenew(context.Background())
	if e.IsLeader() {
		t.Errorf("should not be leader on repo error")
	}
	if e.LastError() == nil {
		t.Errorf("LastError should expose the repo failure")
	}
}

// TestElector_NilRepoSafe: defensive — Run with a nil repo
// returns immediately. Caller can pass a degraded Elector
// without crashing.
func TestElector_NilRepoSafe(t *testing.T) {
	e := &Elector{logger: zerolog.Nop()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	e.Run(ctx) // returns immediately
}

// TestElector_GracefulReleaseOnCancel: the Run loop calls
// Release when ctx is cancelled while we hold the lock. The
// fake repo's Release backdates expires_at so a successor can
// take over on the next tick.
func TestElector_GracefulReleaseOnCancel(t *testing.T) {
	repo := newFakeRepo()
	e := New(repo, "w", "us", 10*time.Second, zerolog.Nop())
	e.tryAcquireOrRenew(context.Background())
	if !e.IsLeader() {
		t.Fatalf("setup: should be leader")
	}
	// Simulate Run exit via the same code path.
	e.gracefulRelease()
	if e.IsLeader() {
		t.Errorf("leader bit should clear after release")
	}
	row, _ := repo.Get(context.Background(), "w")
	if row.ExpiresAt.After(time.Now()) {
		t.Errorf("release should backdate expires_at; got %v", row.ExpiresAt)
	}
}

// TestNew_ShortTTLClampedToDefault: catches a typo
// (`ttl: 100*time.Millisecond`) that would make the renew loop
// race against expiry every iteration. New() falls back to
// DefaultTTL.
func TestNew_ShortTTLClampedToDefault(t *testing.T) {
	e := New(nil, "w", "us", 100*time.Millisecond, zerolog.Nop())
	if e.ttl != DefaultTTL {
		t.Errorf("ttl = %v, want %v (clamped to default)", e.ttl, DefaultTTL)
	}
}

// TestElector_Release_BackdatesAndClearsLeaderBit covers the
// public Release path the shutdown sequence uses. Pin:
//   - leader bit clears so subsequent IsLeader returns false
//   - repo row's expires_at is backdated (fakeRepo's Release contract)
//   - non-leader callers no-op without touching the repo
//   - nil-elector / nil-repo callers no-op without panicking
func TestElector_Release_BackdatesAndClearsLeaderBit(t *testing.T) {
	repo := newFakeRepo()
	e := New(repo, "w", "us", 10*time.Second, zerolog.Nop())
	e.tryAcquireOrRenew(context.Background())
	if !e.IsLeader() {
		t.Fatalf("setup: should be leader")
	}

	if err := e.Release(context.Background()); err != nil {
		t.Fatalf("Release returned err: %v", err)
	}
	if e.IsLeader() {
		t.Errorf("leader bit should clear after Release")
	}
	row, _ := repo.Get(context.Background(), "w")
	if row.ExpiresAt.After(time.Now()) {
		t.Errorf("Release should backdate expires_at; got %v", row.ExpiresAt)
	}
}

func TestElector_Release_NonLeaderShortCircuits(t *testing.T) {
	// fakeRepoCountingReleases captures the contract: Release
	// must NOT hit the DB when this instance never held the lock.
	repo := &fakeRepoCountingReleases{inner: newFakeRepo()}
	e := New(repo, "w", "us", 10*time.Second, zerolog.Nop())
	// No tryAcquireOrRenew → not a leader.
	if err := e.Release(context.Background()); err != nil {
		t.Fatalf("Release on non-leader returned err: %v", err)
	}
	if repo.releaseCalls != 0 {
		t.Errorf("non-leader Release reached the repo; calls=%d", repo.releaseCalls)
	}
}

func TestElector_Release_NilSafe(t *testing.T) {
	// nil receiver + nil-repo elector both no-op without panicking.
	var nilE *Elector
	if err := nilE.Release(context.Background()); err != nil {
		t.Errorf("nil receiver Release: %v", err)
	}
	e := New(nil, "w", "us", 10*time.Second, zerolog.Nop())
	if err := e.Release(context.Background()); err != nil {
		t.Errorf("nil-repo Release: %v", err)
	}
}

// fakeRepoCountingReleases wraps a fakeRepo and tracks calls to
// Release for the short-circuit test above.
type fakeRepoCountingReleases struct {
	inner        *fakeRepo
	releaseCalls int
}

func (f *fakeRepoCountingReleases) Acquire(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, int64, error) {
	return f.inner.Acquire(ctx, workerID, holderID, now, ttl)
}
func (f *fakeRepoCountingReleases) Renew(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, error) {
	return f.inner.Renew(ctx, workerID, holderID, now, ttl)
}
func (f *fakeRepoCountingReleases) Release(ctx context.Context, workerID, holderID string) error {
	f.releaseCalls++
	return f.inner.Release(ctx, workerID, holderID)
}
func (f *fakeRepoCountingReleases) Get(ctx context.Context, workerID string) (*persistence.DaemonLeaderLock, error) {
	return f.inner.Get(ctx, workerID)
}
func (f *fakeRepoCountingReleases) List(ctx context.Context) ([]*persistence.DaemonLeaderLock, error) {
	return f.inner.List(ctx)
}

// ---------------------------------------------------------------------------
// Task 2 — Epoch tracking + VerifyEpoch fence guard
// ---------------------------------------------------------------------------

// TestElector_TracksEpochAcrossTakeover: elector A acquires (epoch e1);
// a different holder B takes over via the same repo (epoch e2>e1); A's
// stored epoch must still equal e1 (it stores the last epoch IT won, not
// the epoch B acquired). A's tryAcquireOrRenew while B holds an unexpired
// lock must NOT overwrite A's stored epoch.
func TestElector_TracksEpochAcrossTakeover(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()

	// A acquires.
	eA := New(repo, "w", "holder-a", 10*time.Second, zerolog.Nop())
	eA.clock = func() time.Time { return now }
	eA.tryAcquireOrRenew(context.Background())
	if !eA.IsLeader() {
		t.Fatalf("setup: A should be leader after first acquire")
	}
	epochA := eA.Epoch()
	if epochA != 1 {
		t.Errorf("A's first epoch = %d, want 1", epochA)
	}

	// Expire A's lock by back-dating it, then B takes over.
	repo.mu.Lock()
	repo.rows["w"].ExpiresAt = now.Add(-1 * time.Second)
	repo.mu.Unlock()

	eB := New(repo, "w", "holder-b", 10*time.Second, zerolog.Nop())
	tB := now.Add(time.Second)
	eB.clock = func() time.Time { return tB }
	eB.tryAcquireOrRenew(context.Background())
	if !eB.IsLeader() {
		t.Fatalf("setup: B should take over expired lock")
	}
	epochB := eB.Epoch()
	if epochB <= epochA {
		t.Errorf("B's epoch (%d) should be > A's epoch (%d) after takeover", epochB, epochA)
	}

	// A now calls tryAcquireOrRenew while B holds an unexpired lock.
	// A should NOT become leader, and A's stored epoch must stay at e1.
	eA.mu.Lock()
	eA.leader = false // A already lost it, ensure Acquire (not Renew) branch runs
	eA.mu.Unlock()
	eA.tryAcquireOrRenew(context.Background())
	if eA.IsLeader() {
		t.Errorf("A should NOT have taken back the lock while B holds it")
	}
	if eA.Epoch() != epochA {
		t.Errorf("A's stored epoch = %d after failed acquire; want %d (last epoch A WON)", eA.Epoch(), epochA)
	}
}

// TestElector_VerifyEpoch_StaleLeaderRejected: A holds epoch e1; the repo row
// is force-updated to holder=B epoch e2; VerifyEpoch returns ok=false,
// current=e2. A healthy call (A still holds e1) returns ok=true.
func TestElector_VerifyEpoch_StaleLeaderRejected(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()

	// --- healthy case: A holds e1 ---
	eA := New(repo, "w", "holder-a", 10*time.Second, zerolog.Nop())
	eA.clock = func() time.Time { return now }
	eA.tryAcquireOrRenew(context.Background())
	if !eA.IsLeader() {
		t.Fatalf("setup: A should be leader")
	}
	ok, cur, err := eA.VerifyEpoch(context.Background())
	if err != nil {
		t.Fatalf("VerifyEpoch (healthy): unexpected err: %v", err)
	}
	if !ok {
		t.Errorf("VerifyEpoch (healthy): expected ok=true; got false (epoch %d)", cur)
	}
	if cur != eA.Epoch() {
		t.Errorf("VerifyEpoch (healthy): current epoch = %d, want %d", cur, eA.Epoch())
	}

	// --- stale case: force row to holder=B with epoch e2 > e1 ---
	e1 := eA.Epoch()
	e2 := e1 + 5
	repo.mu.Lock()
	repo.rows["w"] = &persistence.DaemonLeaderLock{
		WorkerID:   "w",
		HolderID:   "holder-b",
		AcquiredAt: now,
		RenewedAt:  now,
		ExpiresAt:  now.Add(10 * time.Second),
		Epoch:      e2,
	}
	repo.mu.Unlock()

	ok, cur, err = eA.VerifyEpoch(context.Background())
	if err != nil {
		t.Fatalf("VerifyEpoch (stale): unexpected err: %v", err)
	}
	if ok {
		t.Errorf("VerifyEpoch (stale): expected ok=false; got true")
	}
	if cur != e2 {
		t.Errorf("VerifyEpoch (stale): current = %d, want %d", cur, e2)
	}
}

// TestElector_VerifyEpoch_ReadErrorFailsClosed: when repo.Get returns an error
// (not ErrNotFound), VerifyEpoch must surface it and return ok=false.
func TestElector_VerifyEpoch_ReadErrorFailsClosed(t *testing.T) {
	repo := newFakeRepo()
	now := time.Now()

	eA := New(repo, "w", "holder-a", 10*time.Second, zerolog.Nop())
	eA.clock = func() time.Time { return now }
	eA.tryAcquireOrRenew(context.Background())
	if !eA.IsLeader() {
		t.Fatalf("setup: A should be leader")
	}

	// Inject a hard error on Get.
	repo.err = errors.New("db connection lost")
	ok, _, err := eA.VerifyEpoch(context.Background())
	if err == nil {
		t.Errorf("VerifyEpoch on repo error: expected non-nil err")
	}
	if ok {
		t.Errorf("VerifyEpoch on repo error: expected ok=false (fail closed)")
	}
}
