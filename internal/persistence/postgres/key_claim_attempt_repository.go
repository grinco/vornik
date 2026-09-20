package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// KeyClaimAttemptRepository is the Postgres half of the cluster-wide claim
// bound. See persistence.KeyClaimAttemptRepository.
type KeyClaimAttemptRepository struct{ db DBTX }

// NewKeyClaimAttemptRepository constructs the repository.
func NewKeyClaimAttemptRepository(db DBTX) *KeyClaimAttemptRepository {
	return &KeyClaimAttemptRepository{db: db}
}

// RecordClaimAttempt inserts the attempt and counts the window in ONE
// statement, so no caller can separate the two and reopen the race the
// process-local bucket had.
//
// THE `+ 1` IS LOAD-BEARING, and the shared repotest suite is what proved it.
// In Postgres a data-modifying CTE does NOT see its own changes from the rest
// of the statement: the sibling SELECT runs against the same snapshot the
// INSERT started from, so the row just written is invisible to it and every
// count came back exactly one short. The first version of this shipped that
// way, passed on SQLite — whose transaction genuinely sees the insert — and
// failed five of six cases on the pgvector lane. That is the JSONB
// byte-exactness shape again, and it is the reason the suite runs on both
// drivers rather than on one and an argument.
//
// The BOUNDARY, stated rather than implied: two concurrent recorders under
// READ COMMITTED each see neither other's row, so a burst of N simultaneous
// attempts can each compute the same count and admit. The overshoot is bounded
// by the concurrency, not by the window, and against a 10-per-hour guessing
// bound that is a rounding error — but it is a real weakening of "exactly 10",
// and a caller that needs exactness needs SERIALIZABLE, not this.
func (r *KeyClaimAttemptRepository) RecordClaimAttempt(ctx context.Context, keyID string, now time.Time, window time.Duration) (int, error) {
	if keyID == "" {
		return 0, fmt.Errorf("claim attempt needs a key id")
	}
	cutoff := now.Add(-window)
	var count int
	err := r.db.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO key_claim_attempts (id, key_id, attempted_at)
			VALUES ($1, $2, $3)
			RETURNING key_id
		)
		SELECT count(*) + 1 FROM key_claim_attempts
		WHERE key_id = $2 AND attempted_at > $4`,
		newClaimAttemptID(now), keyID, now.UTC(), cutoff.UTC()).Scan(&count)
	if err != nil {
		return 0, mapDBError(err)
	}
	return count, nil
}

// PruneClaimAttempts deletes attempts older than before.
func (r *KeyClaimAttemptRepository) PruneClaimAttempts(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM key_claim_attempts WHERE attempted_at <= $1`, before.UTC())
	if err != nil {
		return 0, mapDBError(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// newClaimAttemptID is unique per row without a uniqueness CONSTRAINT: two
// genuine attempts in the same nanosecond are two attempts, and a collapsed
// pair would under-count exactly the burst this table exists to catch.
func newClaimAttemptID(now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("kca_%d_%s", now.UTC().UnixNano(), hex.EncodeToString(b[:]))
}

var _ persistence.KeyClaimAttemptRepository = (*KeyClaimAttemptRepository)(nil)
