package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LeaderLockRepository implements
// persistence.DaemonLeaderLockRepository against PostgreSQL.
//
// Acquire uses INSERT … ON CONFLICT DO UPDATE so two daemons
// racing on the same workerID can't both win; the
// WHERE-clause-driven UPDATE only fires for expired locks or
// for the current holder. Renew is the holder-bound UPDATE that
// extends the lease; Release flips expires_at to NOW() so a
// successor can take over without waiting the full TTL.
type LeaderLockRepository struct {
	db DBTX
}

// NewLeaderLockRepository constructs the repo over db.
func NewLeaderLockRepository(db DBTX) *LeaderLockRepository {
	return &LeaderLockRepository{db: db}
}

// Acquire atomically grants the lock to holderID, either by
// inserting a fresh row or by taking over an expired one (or
// refreshing a row already held by holderID).
//
// Returns (acquired, epoch, err):
//   - acquired is true when the calling daemon holds the lock
//     after the statement; false when another daemon still holds
//     an unexpired lock (epoch will be 0 in that case).
//   - epoch is the fence token of the lock after this call — it
//     strictly increases on takeover and is preserved on renew.
func (r *LeaderLockRepository) Acquire(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, int64, error) {
	if workerID == "" || holderID == "" {
		return false, 0, fmt.Errorf("leader_lock: workerID + holderID required")
	}
	if ttl <= 0 {
		return false, 0, fmt.Errorf("leader_lock: ttl must be positive")
	}
	expires := now.Add(ttl)
	// INSERT new row OR take over an expired lock OR refresh
	// our own row. The DO UPDATE … WHERE clause is the
	// atomicity-critical bit: Postgres evaluates the predicate
	// against the EXISTING row before deciding to update.
	//
	// epoch semantics:
	//   - First INSERT (no conflict): epoch = 1.
	//   - Same-holder renew: epoch = daemon_leader_locks.epoch
	//     (the existing row's value — preserved).
	//   - Takeover (different holder wins): epoch =
	//     daemon_leader_locks.epoch + 1 (strictly increases).
	//
	// Note: ON CONFLICT … WHERE doesn't itself short-circuit
	// the conflict resolution — the row still exists. We
	// return based on whether OUR holder_id ends up in the
	// row, queried via RETURNING in the same statement.
	const q = `
INSERT INTO daemon_leader_locks (worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch)
VALUES ($1, $2, $3, $3, $4, 1)
ON CONFLICT (worker_id) DO UPDATE
SET holder_id = EXCLUDED.holder_id,
    acquired_at = CASE WHEN daemon_leader_locks.holder_id = EXCLUDED.holder_id THEN daemon_leader_locks.acquired_at ELSE EXCLUDED.acquired_at END,
    renewed_at = EXCLUDED.renewed_at,
    expires_at = EXCLUDED.expires_at,
    epoch = CASE WHEN daemon_leader_locks.holder_id = EXCLUDED.holder_id
                 THEN daemon_leader_locks.epoch
                 ELSE daemon_leader_locks.epoch + 1 END
WHERE daemon_leader_locks.expires_at < EXCLUDED.renewed_at
   OR daemon_leader_locks.holder_id = EXCLUDED.holder_id
RETURNING holder_id, epoch`
	var winningHolder string
	var epoch int64
	err := r.db.QueryRowContext(ctx, q, workerID, holderID, now.UTC(), expires.UTC()).Scan(&winningHolder, &epoch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// ON CONFLICT WHERE didn't match — someone else
			// holds an unexpired lock. RETURNING produces no
			// row in that case.
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("leader_lock: acquire: %w", err)
	}
	return winningHolder == holderID, epoch, nil
}

// Renew extends the lease only when holderID still matches the
// row's holder. A losing Renew is the signal that another
// daemon took over (clock skew, network partition).
func (r *LeaderLockRepository) Renew(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, error) {
	if workerID == "" || holderID == "" {
		return false, fmt.Errorf("leader_lock: workerID + holderID required")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("leader_lock: ttl must be positive")
	}
	const q = `
UPDATE daemon_leader_locks
SET renewed_at = $3, expires_at = $4
WHERE worker_id = $1 AND holder_id = $2`
	res, err := r.db.ExecContext(ctx, q, workerID, holderID, now.UTC(), now.Add(ttl).UTC())
	if err != nil {
		return false, fmt.Errorf("leader_lock: renew: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Drivers that don't report affected rows shouldn't
		// make us misclassify success — assume the UPDATE
		// landed (the caller will discover loss on the next
		// Acquire).
		return true, nil
	}
	return n > 0, nil
}

// Release flips expires_at to NOW() so a successor doesn't
// have to wait the full TTL. Best-effort.
func (r *LeaderLockRepository) Release(ctx context.Context, workerID, holderID string) error {
	const q = `
UPDATE daemon_leader_locks
SET expires_at = NOW()
WHERE worker_id = $1 AND holder_id = $2`
	if _, err := r.db.ExecContext(ctx, q, workerID, holderID); err != nil {
		return fmt.Errorf("leader_lock: release: %w", err)
	}
	return nil
}

// Get returns the current row by workerID. Used by diagnostics
// + the doctor check.
func (r *LeaderLockRepository) Get(ctx context.Context, workerID string) (*persistence.DaemonLeaderLock, error) {
	const q = `
SELECT worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch
FROM daemon_leader_locks
WHERE worker_id = $1`
	row := r.db.QueryRowContext(ctx, q, workerID)
	var l persistence.DaemonLeaderLock
	if err := row.Scan(&l.WorkerID, &l.HolderID, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt, &l.Epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("leader_lock: get: %w", err)
	}
	return &l, nil
}

// List returns every leader-lock row sorted by worker_id.
// Drives the doctor health check + the admin UI. Empty slice
// when the table is empty (fresh deployment).
func (r *LeaderLockRepository) List(ctx context.Context) ([]*persistence.DaemonLeaderLock, error) {
	const q = `
SELECT worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch
FROM daemon_leader_locks
ORDER BY worker_id ASC`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("leader_lock: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.DaemonLeaderLock
	for rows.Next() {
		var l persistence.DaemonLeaderLock
		if err := rows.Scan(&l.WorkerID, &l.HolderID, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt, &l.Epoch); err != nil {
			return nil, fmt.Errorf("leader_lock: list scan: %w", err)
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}
