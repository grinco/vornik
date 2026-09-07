package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionRatingRepository is the PostgreSQL implementation of
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
// created_at ALONE — that is the editability the design is built on, and
// rewriting created_at here would quietly destroy "when was this first judged".
//
// created_at is taken from the caller when set, so a fixture (or a backfill)
// can place a rating in the past; otherwise the column default applies.
func (r *ExecutionRatingRepository) Upsert(ctx context.Context, rating *persistence.ExecutionRating) error {
	created := rating.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_ratings (execution_id, rater_id, verdict, reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (execution_id, rater_id) DO UPDATE
		   SET verdict    = EXCLUDED.verdict,
		       reason     = EXCLUDED.reason,
		       updated_at = NOW()`,
		rating.ExecutionID, rating.RaterID, rating.Verdict, rating.Reason, created,
	)
	return mapDBError(err)
}

// Get returns one rater's rating, or ErrNotFound.
func (r *ExecutionRatingRepository) Get(ctx context.Context, executionID, raterID string) (*persistence.ExecutionRating, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT execution_id, rater_id, verdict, reason, created_at, updated_at
		  FROM execution_ratings
		 WHERE execution_id = $1 AND rater_id = $2`, executionID, raterID)
	out := &persistence.ExecutionRating{}
	err := row.Scan(&out.ExecutionID, &out.RaterID, &out.Verdict, &out.Reason,
		&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// ListByExecution returns every rater's rating for one execution, oldest first.
func (r *ExecutionRatingRepository) ListByExecution(ctx context.Context, executionID string) ([]*persistence.ExecutionRating, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT execution_id, rater_id, verdict, reason, created_at, updated_at
		  FROM execution_ratings
		 WHERE execution_id = $1
		 ORDER BY created_at, rater_id`, executionID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ExecutionRating
	for rows.Next() {
		e := &persistence.ExecutionRating{}
		if err := rows.Scan(&e.ExecutionID, &e.RaterID, &e.Verdict, &e.Reason,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete removes one rater's rating. Absent is not an error.
func (r *ExecutionRatingRepository) Delete(ctx context.Context, executionID, raterID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM execution_ratings WHERE execution_id = $1 AND rater_id = $2`,
		executionID, raterID)
	return mapDBError(err)
}

// DeleteOlderThan prunes by the rating's own horizon and returns the count.
func (r *ExecutionRatingRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM execution_ratings WHERE created_at < $1`, cutoff.UTC())
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
