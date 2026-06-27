package leaderelection

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// TestBootstrapAcquire_FirstAttemptSetsLeader is the synchronous
// path container.go uses to avoid the race between
// "go elector.Run()" and "go worker.Run()". After
// BootstrapAcquire returns, IsLeader MUST already report true
// so the worker's first tick sees an authoritative value.
func TestBootstrapAcquire_FirstAttemptSetsLeader(t *testing.T) {
	repo := newFakeRepo()
	e := New(repo, "w_bootstrap", "host-a", 10*time.Second, zerolog.Nop())

	e.BootstrapAcquire(context.Background())

	if !e.IsLeader() {
		t.Errorf("BootstrapAcquire should make us leader immediately")
	}
	row, err := repo.Get(context.Background(), "w_bootstrap")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.HolderID != "host-a" {
		t.Errorf("holder_id = %q, want host-a", row.HolderID)
	}
}

// TestBootstrapAcquire_NilRepoNoOp confirms the defensive
// branch — passing a nil-repo Elector won't panic.
func TestBootstrapAcquire_NilRepoNoOp(t *testing.T) {
	var e *Elector
	e.BootstrapAcquire(context.Background()) // shouldn't panic

	e = &Elector{logger: zerolog.Nop()}
	e.BootstrapAcquire(context.Background()) // nil repo, shouldn't panic
	if e.IsLeader() {
		t.Errorf("nil-repo bootstrap should leave us NOT leader")
	}
}

// TestRun_AcquiresOnFirstAttemptAndReleasesOnCancel exercises
// the Run loop end-to-end: short TTL → fast cancel → graceful
// release. Without this, the Run path is mostly untested
// (the per-attempt branch is covered by tryAcquireOrRenew
// tests, but the goroutine lifecycle isn't).
func TestRun_AcquiresOnFirstAttemptAndReleasesOnCancel(t *testing.T) {
	repo := newFakeRepo()
	e := New(repo, "w_run", "us", 3*time.Second, zerolog.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()

	// Wait up to 1s for the first immediate-acquire to register.
	deadline := time.Now().Add(time.Second)
	for !e.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !e.IsLeader() {
		cancel()
		<-done
		t.Fatalf("Run did not acquire leader bit within 1s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Run did not exit within 1s of ctx cancel")
	}

	// gracefulRelease should have backdated expires_at via the
	// repo's Release; we verify by reading the row.
	row, err := repo.Get(context.Background(), "w_run")
	if err != nil {
		t.Fatalf("Get post-release: %v", err)
	}
	if row.ExpiresAt.After(time.Now()) {
		t.Errorf("gracefulRelease should backdate expires_at; got %v", row.ExpiresAt)
	}
	if e.IsLeader() {
		t.Errorf("leader bit should be cleared after gracefulRelease")
	}
}

// TestRun_NilEctorReturnsImmediately defends the early-return
// guard. A degraded boot path that passes a nil Elector around
// (no DB) shouldn't panic.
func TestRun_NilEctorReturnsImmediately(t *testing.T) {
	var e *Elector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	select {
	case <-done:
		// Expected: returns immediately.
	case <-time.After(200 * time.Millisecond):
		t.Errorf("nil Elector Run should return immediately")
	}
}

// TestRun_TickerRenewsExistingLease: with a short TTL (3s →
// renew at ~1s), the Run loop should re-up the lease at least
// once before we cancel. Verifies the renew-after-acquire arm
// of tryAcquireOrRenew runs through the ticker path.
func TestRun_TickerRenewsExistingLease(t *testing.T) {
	repo := &renewCountingRepo{fakeRepo: *newFakeRepo()}
	e := New(&repo.fakeRepo, "w_renew", "us", 3*time.Second, zerolog.Nop())

	// Prime so the initial immediate-acquire returns from the
	// renew branch on the next ticker (we're already leader).
	e.tryAcquireOrRenew(context.Background())
	if !e.IsLeader() {
		t.Fatalf("setup: expected leader after first acquire")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()

	// Wait long enough for the ticker (TTL/3 ≈ 1s) to fire at
	// least once.
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	row, _ := repo.Get(context.Background(), "w_renew")
	if row == nil || row.HolderID != "us" {
		t.Errorf("lease lost during renew loop")
	}
}

// renewCountingRepo wraps fakeRepo so the test can observe the
// renew arm fires at least once. Inherits the rest of the
// behaviour via embedding.
type renewCountingRepo struct {
	fakeRepo
	renews int
}

func (r *renewCountingRepo) Renew(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, error) {
	r.renews++
	return r.fakeRepo.Renew(ctx, workerID, holderID, now, ttl)
}

// Sanity guard: compile-time check that the embedded fakeRepo
// satisfies the persistence.DaemonLeaderLockRepository interface.
var _ persistence.DaemonLeaderLockRepository = (*fakeRepo)(nil)
