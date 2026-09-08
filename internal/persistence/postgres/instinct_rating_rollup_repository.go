package postgres

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/ratingrollupsql"
)

// InstinctRatingRollupRepository is the PostgreSQL implementation of
// persistence.InstinctRatingRollupRepository — the REPORTED half of instinct
// lift (2026-09-08-execution-ratings-approval-paths-design.md §3).
//
// The query lives in ratingrollupsql and is shared with the other backend,
// because it encodes the same per-execution grain rule the skill arms do and
// two copies of that is how the backends come to disagree about what
// "contested" means.
type InstinctRatingRollupRepository struct {
	db DBTX
}

// NewInstinctRatingRollupRepository constructs the repo over db.
func NewInstinctRatingRollupRepository(db DBTX) *InstinctRatingRollupRepository {
	return &InstinctRatingRollupRepository{db: db}
}

// InstinctRecoveryRatingArms returns both arms for one recovery instinct.
func (r *InstinctRatingRollupRepository) InstinctRecoveryRatingArms(ctx context.Context,
	instinctID, projectID, role, errorClass string, since time.Time,
) (persistence.InstinctRatingArms, error) {
	out := persistence.InstinctRatingArms{
		InstinctID: instinctID, ProjectID: projectID, Role: role, ErrorClass: errorClass,
	}
	rows, err := r.db.QueryContext(ctx, ratingrollupsql.InstinctRecoveryArms,
		instinctID, projectID, role, errorClass, since.UTC())
	if err != nil {
		return out, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	if err := scanInstinctArms(rows, &out); err != nil {
		return persistence.InstinctRatingArms{}, err
	}
	return out, nil
}
