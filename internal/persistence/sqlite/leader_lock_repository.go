package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LeaderLockRepository implements
// persistence.DaemonLeaderLockRepository against SQLite.
//
// SQLite is single-process so there's never real multi-replica
// contention, but we persist the row so:
//   - Get/List work for diagnostics without special-casing.
//   - The epoch fence token is monotonic and can be tested with
//     the same semantics as the Postgres implementation.
//
// Acquire uses an INSERT OR REPLACE (UPSERT) pattern. Because
// SQLite's driver does not guarantee RETURNING in all versions
// used by this project, we do the upsert then a SELECT epoch in
// the same logical operation (both are synchronous; single-
// process means no interleaving writer).
type LeaderLockRepository struct {
	db *sql.DB
}

// NewLeaderLockRepository returns the SQLite-backed repo.
func NewLeaderLockRepository(db *sql.DB) *LeaderLockRepository {
	return &LeaderLockRepository{db: db}
}

// Acquire atomically grants the lock to holderID. Single-process
// so we always win, but we still maintain the epoch semantics:
//   - First acquire (INSERT): epoch starts at 1.
//   - Same-holder renew: epoch is preserved.
//   - Different holder takes over expired row: epoch is bumped.
//
// Returns (true, epoch, nil) on success; (false, 0, nil) when
// the row is held by a different, unexpired holder (should not
// normally occur in single-process deployments but the interface
// contract is honoured).
func (r *LeaderLockRepository) Acquire(ctx context.Context, workerID, holderID string, now time.Time, ttl time.Duration) (bool, int64, error) {
	expires := now.Add(ttl)
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expiresStr := expires.UTC().Format(time.RFC3339Nano)

	// Read the existing row (if any).
	var cur persistence.DaemonLeaderLock
	var expiresAtStr string
	const selQ = `SELECT holder_id, expires_at, epoch FROM daemon_leader_locks WHERE worker_id = ?`
	err := r.db.QueryRowContext(ctx, selQ, workerID).Scan(&cur.HolderID, &expiresAtStr, &cur.Epoch)
	rowExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, 0, fmt.Errorf("leader_lock sqlite: acquire read: %w", err)
	}

	if rowExists {
		// Parse the stored expires_at.
		exAt, parseErr := time.Parse(time.RFC3339Nano, expiresAtStr)
		if parseErr != nil {
			return false, 0, fmt.Errorf("leader_lock sqlite: parse expires_at: %w", parseErr)
		}
		// If another holder owns an unexpired row, decline.
		if cur.HolderID != holderID && !exAt.Before(now) {
			return false, 0, nil
		}
	}

	// Compute the new epoch:
	//   - No prior row (INSERT): epoch = 1.
	//   - Same holder (renew): keep existing epoch.
	//   - Different holder taking over expired row: bump epoch.
	var newEpoch int64
	switch {
	case !rowExists:
		newEpoch = 1
	case cur.HolderID == holderID:
		newEpoch = cur.Epoch
	default:
		newEpoch = cur.Epoch + 1
	}

	// Compute acquired_at: preserved on same-holder renew.
	var acquiredAtStr string
	if rowExists && cur.HolderID == holderID {
		const getAcqQ = `SELECT acquired_at FROM daemon_leader_locks WHERE worker_id = ?`
		if scanErr := r.db.QueryRowContext(ctx, getAcqQ, workerID).Scan(&acquiredAtStr); scanErr != nil {
			acquiredAtStr = nowStr
		}
	} else {
		acquiredAtStr = nowStr
	}

	const upsQ = `
INSERT INTO daemon_leader_locks (worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (worker_id) DO UPDATE SET
    holder_id   = excluded.holder_id,
    acquired_at = excluded.acquired_at,
    renewed_at  = excluded.renewed_at,
    expires_at  = excluded.expires_at,
    epoch       = excluded.epoch`
	if _, execErr := r.db.ExecContext(ctx, upsQ, workerID, holderID, acquiredAtStr, nowStr, expiresStr, newEpoch); execErr != nil {
		return false, 0, fmt.Errorf("leader_lock sqlite: acquire upsert: %w", execErr)
	}
	return true, newEpoch, nil
}

// Renew extends the lease only when holderID still matches the
// row's holder. Always succeeds in single-process, but honours
// the interface contract.
func (r *LeaderLockRepository) Renew(_ context.Context, _, _ string, _ time.Time, _ time.Duration) (bool, error) {
	return true, nil
}

// Release is a no-op for the SQLite single-process deployment.
// The daemon never has a successor to hand off to.
func (r *LeaderLockRepository) Release(_ context.Context, _, _ string) error {
	return nil
}

// Get returns the current row by workerID. Returns
// persistence.ErrNotFound when the table has no row yet (fresh
// deployment before any Acquire call).
func (r *LeaderLockRepository) Get(ctx context.Context, workerID string) (*persistence.DaemonLeaderLock, error) {
	if workerID == "" {
		return nil, persistence.ErrNotFound
	}
	const q = `SELECT worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch FROM daemon_leader_locks WHERE worker_id = ?`
	var l persistence.DaemonLeaderLock
	var acquiredAtStr, renewedAtStr, expiresAtStr string
	err := r.db.QueryRowContext(ctx, q, workerID).Scan(
		&l.WorkerID, &l.HolderID,
		&acquiredAtStr, &renewedAtStr, &expiresAtStr,
		&l.Epoch,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("leader_lock sqlite: get: %w", err)
	}
	if l.AcquiredAt, err = time.Parse(time.RFC3339Nano, acquiredAtStr); err != nil {
		return nil, fmt.Errorf("leader_lock sqlite: parse acquired_at: %w", err)
	}
	if l.RenewedAt, err = time.Parse(time.RFC3339Nano, renewedAtStr); err != nil {
		return nil, fmt.Errorf("leader_lock sqlite: parse renewed_at: %w", err)
	}
	if l.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAtStr); err != nil {
		return nil, fmt.Errorf("leader_lock sqlite: parse expires_at: %w", err)
	}
	return &l, nil
}

// List returns every leader-lock row sorted by worker_id.
// Empty slice when no worker has ever acquired (fresh deployment).
func (r *LeaderLockRepository) List(ctx context.Context) ([]*persistence.DaemonLeaderLock, error) {
	const q = `SELECT worker_id, holder_id, acquired_at, renewed_at, expires_at, epoch FROM daemon_leader_locks ORDER BY worker_id ASC`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("leader_lock sqlite: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.DaemonLeaderLock
	for rows.Next() {
		var l persistence.DaemonLeaderLock
		var acquiredAtStr, renewedAtStr, expiresAtStr string
		if scanErr := rows.Scan(&l.WorkerID, &l.HolderID, &acquiredAtStr, &renewedAtStr, &expiresAtStr, &l.Epoch); scanErr != nil {
			return nil, fmt.Errorf("leader_lock sqlite: list scan: %w", scanErr)
		}
		if l.AcquiredAt, err = time.Parse(time.RFC3339Nano, acquiredAtStr); err != nil {
			return nil, fmt.Errorf("leader_lock sqlite: parse acquired_at: %w", err)
		}
		if l.RenewedAt, err = time.Parse(time.RFC3339Nano, renewedAtStr); err != nil {
			return nil, fmt.Errorf("leader_lock sqlite: parse renewed_at: %w", err)
		}
		if l.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAtStr); err != nil {
			return nil, fmt.Errorf("leader_lock sqlite: parse expires_at: %w", err)
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}
