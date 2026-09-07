package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionRatingRepository is the SQLite implementation of
// persistence.ExecutionRatingRepository (LLD
// 2026-09-04-execution-ratings-design).
type ExecutionRatingRepository struct {
	db DBTX
}

// NewExecutionRatingRepository constructs the repo over db.
func NewExecutionRatingRepository(db DBTX) *ExecutionRatingRepository {
	return &ExecutionRatingRepository{db: db}
}

// Upsert records or replaces one rater's verdict.
//
// ON CONFLICT updates the verdict, the reason and updated_at and leaves
// created_at ALONE. Note the SQLite form has to say that explicitly by simply
// not listing created_at in the SET clause — an `INSERT OR REPLACE` would have
// been the obvious spelling and is WRONG here: it deletes the row and inserts a
// new one, so created_at would silently become the edit time and "when was this
// first judged" would be lost on every change of mind.
func (r *ExecutionRatingRepository) Upsert(ctx context.Context, rating *persistence.ExecutionRating) error {
	created := rating.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_ratings (execution_id, rater_id, verdict, reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (execution_id, rater_id) DO UPDATE
		   SET verdict    = excluded.verdict,
		       reason     = excluded.reason,
		       updated_at = excluded.updated_at`,
		rating.ExecutionID, rating.RaterID, rating.Verdict, rating.Reason,
		sqliteTime(created), sqliteTime(now),
	)
	return err
}

// Get returns one rater's rating, or ErrNotFound.
func (r *ExecutionRatingRepository) Get(ctx context.Context, executionID, raterID string) (*persistence.ExecutionRating, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT execution_id, rater_id, verdict, reason, created_at, updated_at
		  FROM execution_ratings
		 WHERE execution_id = ? AND rater_id = ?`, executionID, raterID)
	out, err := scanExecutionRating(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByExecution returns every rater's rating for one execution, oldest first.
func (r *ExecutionRatingRepository) ListByExecution(ctx context.Context, executionID string) ([]*persistence.ExecutionRating, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT execution_id, rater_id, verdict, reason, created_at, updated_at
		  FROM execution_ratings
		 WHERE execution_id = ?
		 ORDER BY created_at, rater_id`, executionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ExecutionRating
	for rows.Next() {
		e, err := scanExecutionRating(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete removes one rater's rating. Absent is not an error.
func (r *ExecutionRatingRepository) Delete(ctx context.Context, executionID, raterID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM execution_ratings WHERE execution_id = ? AND rater_id = ?`,
		executionID, raterID)
	return err
}

// DeleteOlderThan prunes by the rating's own horizon and returns the count.
//
// The comparison is on the RFC3339Nano text, which sorts lexicographically in
// timestamp order for UTC values of the same width — the convention every
// ordered column in this schema relies on.
func (r *ExecutionRatingRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM execution_ratings WHERE created_at < ?`, sqliteTime(cutoff))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// scanExecutionRating decodes one row from either a *sql.Row or *sql.Rows,
// parsing the two text timestamps back into time.Time.
func scanExecutionRating(scan func(dest ...any) error) (*persistence.ExecutionRating, error) {
	var createdRaw, updatedRaw string
	out := &persistence.ExecutionRating{}
	if err := scan(&out.ExecutionID, &out.RaterID, &out.Verdict, &out.Reason,
		&createdRaw, &updatedRaw); err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return nil, err
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedRaw)
	if err != nil {
		return nil, err
	}
	out.CreatedAt = created.UTC()
	out.UpdatedAt = updated.UTC()
	return out, nil
}
