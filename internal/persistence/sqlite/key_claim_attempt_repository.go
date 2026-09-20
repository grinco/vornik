package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// KeyClaimAttemptRepository is the SQLite half of the cluster-wide claim bound.
//
// "Cluster-wide" is not the property SQLite delivers — a SQLite deployment is
// one process — but the CONTRACT has to be the same on both drivers, or the
// bound would mean two things and the shared suite could not pin it. What
// SQLite does deliver here, and process memory did not, is survival across a
// daemon restart.
type KeyClaimAttemptRepository struct{ db *sql.DB }

// NewKeyClaimAttemptRepository constructs the repository.
func NewKeyClaimAttemptRepository(db *sql.DB) *KeyClaimAttemptRepository {
	return &KeyClaimAttemptRepository{db: db}
}

// RecordClaimAttempt inserts the attempt and counts the window.
//
// Two statements in one transaction rather than Postgres's single CTE: SQLite
// has no INSERT … RETURNING inside a CTE that a later SELECT can read. The
// transaction is what keeps the two inseparable, which is the property that
// matters — a caller must not be able to count without having recorded.
func (r *KeyClaimAttemptRepository) RecordClaimAttempt(ctx context.Context, keyID string, now time.Time, window time.Duration) (int, error) {
	if keyID == "" {
		return 0, fmt.Errorf("claim attempt needs a key id")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO key_claim_attempts (id, key_id, attempted_at) VALUES (?, ?, ?)`,
		newClaimAttemptID(now), keyID, now.UTC()); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM key_claim_attempts WHERE key_id = ? AND attempted_at > ?`,
		keyID, now.Add(-window).UTC()).Scan(&count); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// PruneClaimAttempts deletes attempts older than before.
func (r *KeyClaimAttemptRepository) PruneClaimAttempts(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM key_claim_attempts WHERE attempted_at <= ?`, before.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func newClaimAttemptID(now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("kca_%d_%s", now.UTC().UnixNano(), hex.EncodeToString(b[:]))
}

var _ persistence.KeyClaimAttemptRepository = (*KeyClaimAttemptRepository)(nil)
