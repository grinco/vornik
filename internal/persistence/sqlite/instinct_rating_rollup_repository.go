package sqlite

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/ratingrollupsql"
)

// InstinctRatingRollupRepository is the SQLite implementation of
// persistence.InstinctRatingRollupRepository — the REPORTED half of instinct
// lift (2026-09-08-execution-ratings-approval-paths-design.md §3).
//
// Same shared query text as Postgres; only the placeholder style differs. The
// numbered form is required here because the query references its window
// parameter twice, and the positional rewrite would turn one argument into two.
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
	rows, err := r.db.QueryContext(ctx,
		ratingrollupsql.ToNumberedPlaceholders(ratingrollupsql.InstinctRecoveryArms, 5),
		instinctID, projectID, role, errorClass, sqliteTime(since))
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()

	if err := scanInstinctArms(rows, &out); err != nil {
		return persistence.InstinctRatingArms{}, err
	}
	return out, nil
}

// scanInstinctArms stitches the query's two rows — one per arm — onto one
// result. An arm with no executions produces NO row, so the zero value is the
// right starting point: an absent arm is an empty one, and EligibleN 0 is what
// makes coverage report 0 rather than complete observation.
func scanInstinctArms(rows *sql.Rows, out *persistence.InstinctRatingArms) error {
	for rows.Next() {
		var treated int
		var arm persistence.RatingArm
		if err := rows.Scan(&treated, &arm.EligibleN, &arm.RatedN, &arm.UpN, &arm.ContestedN); err != nil {
			return err
		}
		if treated == 1 {
			out.Treatment = arm
		} else {
			out.Baseline = arm
		}
	}
	return rows.Err()
}
