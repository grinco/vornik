package persistence

import (
	"context"
	"time"
)

// DaemonLeaderLock is one row in the `daemon_leader_locks`
// table. Each named worker holds at most one lock; the daemon
// instance whose holder_id matches the row's holder_id is the
// active leader. Replicas whose holder_id doesn't match skip
// their tick.
//
// See internal/leaderelection for the high-level acquire/renew/
// release flow + the worker-side gating pattern.
type DaemonLeaderLock struct {
	WorkerID   string
	HolderID   string
	AcquiredAt time.Time
	RenewedAt  time.Time
	ExpiresAt  time.Time
	// Epoch is the monotonic fence token for this lock row. It
	// strictly increases on every takeover (a different holder
	// wins) and is preserved on a same-holder renew. A
	// leader-gated write stamps this epoch onto the shared state
	// row; the DB rejects a write whose epoch is older than the
	// row's current epoch (fence against a resumed stale leader —
	// review finding B1).
	Epoch int64
}

// IsHeldBy reports whether the supplied holderID is the active
// leader for the row. Convenience for the worker-side check.
func (l DaemonLeaderLock) IsHeldBy(holderID string, now time.Time) bool {
	return l.HolderID == holderID && !l.ExpiresAt.Before(now)
}

// DaemonLeaderLockRepository persists the per-worker leader
// lock rows. Implementations:
//   - Postgres: real multi-replica HA semantics via
//     INSERT … ON CONFLICT DO UPDATE … WHERE expires_at < NOW().
//   - SQLite: stub that always grants the lock — single-process
//     deployments don't need contention semantics.
type DaemonLeaderLockRepository interface {
	// Acquire atomically takes the lock for workerID under
	// holderID, setting expires_at to now+ttl. Returns (acquired,
	// epoch, err) where:
	//   - acquired is true when the calling daemon holds the lock
	//     after the call; false when another daemon holds an
	//     unexpired lock (epoch will be 0 in that case).
	//   - epoch is the monotonic fence token of the lock AFTER
	//     this call. It strictly increases on every takeover (a
	//     different holder wins the Acquire) and is preserved on
	//     a same-holder renew. A leader-gated write carries this
	//     epoch; the DB rejects a write whose epoch is older than
	//     the state row's current epoch (fence against a resumed
	//     stale leader — review finding B1).
	//
	// Idempotent: a holder calling Acquire again refreshes
	// expires_at and returns (true, same_epoch, nil).
	Acquire(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, int64, error)

	// Renew extends the lock's expires_at when holderID
	// matches the current holder. Returns true on success;
	// false when the lock has been taken by another daemon.
	// A losing Renew is the signal to step down (the goroutine
	// stops acting as leader on the next tick).
	Renew(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, error)

	// Release clears the holder so a successor can acquire
	// without waiting out the TTL. Best-effort — a daemon
	// crashing without calling Release is the normal failure
	// mode; the next acquirer takes over after the TTL elapses.
	Release(ctx context.Context, workerID, holderID string) error

	// Get returns the current row (or ErrNotFound when no
	// daemon has ever held the lock). Used by the doctor check
	// + diagnostics — workers themselves cache the leader bit
	// in-process to avoid hitting the DB per tick.
	Get(ctx context.Context, workerID string) (*DaemonLeaderLock, error)

	// List returns every leader-lock row sorted by worker_id.
	// Drives the daemon_leader_locks_health doctor check + the
	// planned /ui/admin/leader-locks page. Empty slice when no
	// worker has ever acquired (fresh deployment).
	List(ctx context.Context) ([]*DaemonLeaderLock, error)
}
