// Package leaderelection runs the singleton-worker primitive
// that gates per-daemon background jobs in a multi-replica
// deployment (2026.8.0 horizontal-scaling MUST-HAVE). Each
// worker that must NOT run concurrently across replicas (e.g.
// archive sweeper, autonomy loop, memory consolidate worker)
// constructs an Elector with a worker-ID + holder-ID, calls
// Run() in a goroutine, and asks IsLeader() at every tick.
//
// Contract:
//
//   - Acquire is atomic via the repo's
//     INSERT … ON CONFLICT DO UPDATE … WHERE expires_at < NOW()
//     query. Two daemons calling Acquire simultaneously: one
//     wins, the other gets false.
//
//   - Renew runs at ttl/3 so a brief DB stall (≤ 2/3 of TTL)
//     doesn't cost the lease.
//
//   - IsLeader() reads a cached bool that the Run loop keeps
//     fresh. Worker hot paths can call it without an extra DB
//     round-trip per tick.
//
//   - Release() flips expires_at to NOW() so a successor
//     doesn't have to wait the full TTL when this daemon
//     shuts down gracefully.
//
// Worker-side usage:
//
//	elector := leaderelection.New(repo, "archive_sweeper", c.holderID, 60*time.Second, logger)
//	go elector.Run(ctx)
//	// in the tick loop:
//	if !elector.IsLeader() { return }
//
// Failure mode: a daemon that loses its lock (clock skew,
// network partition, takeover by a healthier replica) sees
// IsLeader() flip to false within ttl/3 + one tick. Workers
// that observe the flip should stop emitting side effects
// immediately; the next Run iteration's Acquire attempt
// determines whether the lease comes back.
package leaderelection

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// DefaultTTL is the lease duration when the caller doesn't pin
// one explicitly. 60s gives the renew loop (every TTL/3 = 20s)
// two attempts to refresh before the lease expires — enough
// headroom for a single network blip without spurious takeover.
const DefaultTTL = 60 * time.Second

// MinTTL prevents a typo (`Elector.TTL = 100 * time.Millisecond`)
// from making the lease so short the renew loop can't keep up.
// 5s is conservative; tests can override via the option below if
// they want faster turnover.
const MinTTL = 5 * time.Second

// Elector is the per-worker leader-election state. Construct
// via New(); drive via Run() in a long-lived goroutine.
type Elector struct {
	repo     persistence.DaemonLeaderLockRepository
	workerID string
	holderID string
	ttl      time.Duration
	logger   zerolog.Logger
	clock    func() time.Time

	mu      sync.RWMutex
	leader  bool
	lastErr error

	// epoch is the fence token of the lock this elector last held.
	// It strictly increases across takeovers; a write carrying a
	// stale (lower) epoch is the signal a new leader has taken over.
	// Written only by tryAcquireOrRenew; read via Epoch().
	epoch atomic.Int64
}

// New constructs an Elector. holderID is a daemon-instance
// identifier — typically `hostname + ":" + pid + ":" +
// boot_uuid` so a daemon restart's holder_id differs from its
// predecessor's (an old crashed leader's row gets taken over
// after the TTL).
func New(repo persistence.DaemonLeaderLockRepository, workerID, holderID string, ttl time.Duration, logger zerolog.Logger) *Elector {
	if ttl < MinTTL {
		ttl = DefaultTTL
	}
	return &Elector{
		repo:     repo,
		workerID: workerID,
		holderID: holderID,
		ttl:      ttl,
		logger:   logger,
		clock:    time.Now,
	}
}

// IsLeader returns the cached leader bit. Safe to call
// concurrently from worker tick loops; the Run goroutine is
// the single writer.
func (e *Elector) IsLeader() bool {
	if e == nil {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leader
}

// LastError returns the most recent acquire/renew error for
// diagnostics. Workers don't need to consult this — IsLeader()
// is the contract. The doctor check + admin UI use it to
// surface stuck elections.
func (e *Elector) LastError() error {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastErr
}

// Epoch returns the fence token of the lock this elector last held.
// It strictly increases across takeovers; a write carrying a stale
// (lower) epoch is the signal a new leader has taken over.
// Returns 0 if this elector has never successfully acquired the lock.
func (e *Elector) Epoch() int64 {
	if e == nil {
		return 0
	}
	return e.epoch.Load()
}

// BootstrapAcquire is a synchronous one-shot acquire callers
// invoke BEFORE starting Run() in a goroutine. Lets the
// downstream worker's first tick see an authoritative
// IsLeader() value — without this, a race between the
// "go elector.Run()" and "go worker.Run()" launches can have
// the worker's first tick fire while the elector is still mid-
// flight, logging a spurious "not the leader; skipping" line.
//
// Idempotent with Run's own immediate-acquire — Run sees a
// holder match on its first tryAcquireOrRenew and reports
// "renew succeeded" rather than "became leader".
func (e *Elector) BootstrapAcquire(ctx context.Context) {
	if e == nil || e.repo == nil {
		return
	}
	e.tryAcquireOrRenew(ctx)
}

// Run blocks on a ticker until ctx is cancelled, periodically
// trying to acquire / renew the lock. Set the leader bit on
// success; clear on failure. On ctx cancellation it calls
// Release so a successor can take over immediately.
func (e *Elector) Run(ctx context.Context) {
	if e == nil || e.repo == nil {
		return
	}
	e.logger.Info().
		Str("worker_id", e.workerID).
		Str("holder_id", e.holderID).
		Dur("ttl", e.ttl).
		Msg("leader-election: started")
	// Renew at ttl/3 so two consecutive failures don't lose
	// the lease (the third attempt still races against
	// expiry).
	tick := e.ttl / 3
	if tick < time.Second {
		tick = time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	// First attempt fires immediately so workers can start
	// gating on the leader bit without waiting one tick.
	e.tryAcquireOrRenew(ctx)

	for {
		select {
		case <-ctx.Done():
			e.gracefulRelease()
			return
		case <-ticker.C:
			e.tryAcquireOrRenew(ctx)
		}
	}
}

// tryAcquireOrRenew does the right thing depending on current
// state: leaders call Renew (faster path; doesn't touch the
// expiry-takeover branch); non-leaders call Acquire.
func (e *Elector) tryAcquireOrRenew(ctx context.Context) {
	wasLeader := e.IsLeader()
	now := e.clock()
	var ok bool
	var err error
	var epoch int64
	if wasLeader {
		ok, err = e.repo.Renew(ctx, e.workerID, e.holderID, now, e.ttl)
	} else {
		ok, epoch, err = e.repo.Acquire(ctx, e.workerID, e.holderID, now, e.ttl)
		if ok && err == nil {
			e.epoch.Store(epoch)
		}
	}
	e.mu.Lock()
	e.leader = ok && err == nil
	e.lastErr = err
	e.mu.Unlock()

	switch {
	case err != nil:
		e.logger.Warn().
			Err(err).
			Str("worker_id", e.workerID).
			Bool("was_leader", wasLeader).
			Msg("leader-election: acquire/renew failed")
	case wasLeader && !ok:
		// Took the leader bit away from this daemon — usually
		// means another replica acquired our expired row. Log
		// loud; workers will stop side effects on their next
		// tick.
		e.logger.Warn().
			Str("worker_id", e.workerID).
			Str("holder_id", e.holderID).
			Msg("leader-election: lost leadership (another replica took over)")
	case !wasLeader && ok:
		e.logger.Info().
			Str("worker_id", e.workerID).
			Str("holder_id", e.holderID).
			Msg("leader-election: became leader")
	}
}

// gracefulRelease releases the lock when the elector's
// context is cancelled. Best-effort: if Release fails the
// lock expires naturally after the TTL.
func (e *Elector) gracefulRelease() {
	if !e.IsLeader() {
		return
	}
	// Use a fresh background context with a short timeout —
	// the caller's ctx is already cancelled (that's what
	// triggered us).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Best-effort release — releaseWithContext already logs any
	// DB error; the lock falls back to TTL expiry if the call
	// fails. Errors are intentionally not propagated to the
	// Run goroutine's return path because there's no surface
	// that would surface them.
	_ = e.releaseWithContext(ctx)
}

// Release synchronously releases the lock under the supplied
// context. Returns the underlying repo error, if any. Safe to
// call from the drain sequence: the daemon needs to release
// leases BEFORE closing the DB so peer replicas can claim them
// within ~1s instead of waiting the full TTL.
//
// Idempotent: a non-leader call short-circuits without touching
// the DB. Callers can blindly invoke Release on every elector
// without checking IsLeader first.
func (e *Elector) Release(ctx context.Context) error {
	if e == nil || e.repo == nil {
		return nil
	}
	if !e.IsLeader() {
		return nil
	}
	return e.releaseWithContext(ctx)
}

func (e *Elector) releaseWithContext(ctx context.Context) error {
	err := e.repo.Release(ctx, e.workerID, e.holderID)
	if err != nil {
		e.logger.Warn().
			Err(err).
			Str("worker_id", e.workerID).
			Msg("leader-election: release failed; lock will expire after TTL")
	} else {
		e.logger.Info().
			Str("worker_id", e.workerID).
			Msg("leader-election: released")
	}
	e.mu.Lock()
	e.leader = false
	e.mu.Unlock()
	return err
}
