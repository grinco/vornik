package sqlite

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/ratingrollupsql"
)

// RatingRollupRepository is the sqlite implementation of
// persistence.RatingRollupRepository
// (2026-09-07-execution-ratings-rollup-design.md).
//
// The query itself lives in ratingrollupsql and is shared with the other
// backend, because it encodes the grain rule and two copies of that is how the
// two come to disagree about what "contested" means.
type RatingRollupRepository struct {
	db DBTX
}

// NewRatingRollupRepository constructs the repo over db.
func NewRatingRollupRepository(db DBTX) *RatingRollupRepository {
	return &RatingRollupRepository{db: db}
}

// SkillRatingArms returns both arms per (project, workflow) context.
func (r *RatingRollupRepository) SkillRatingArms(ctx context.Context, skillID string, since time.Time) ([]persistence.SkillRatingArms, error) {
	rows, err := r.db.QueryContext(ctx, ratingrollupsql.ToQuestionMarks(ratingrollupsql.SkillArms, 2), skillID, sqliteTime(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// One row per (project, workflow, treated), so the two arms of a context
	// arrive as separate rows and are stitched here.
	byCtx := map[string]*persistence.SkillRatingArms{}
	var order []string
	for rows.Next() {
		var projectID, workflowID string
		var treated int
		var arm persistence.RatingArm
		if err := rows.Scan(&projectID, &workflowID, &treated,
			&arm.EligibleN, &arm.RatedN, &arm.UpN, &arm.ContestedN); err != nil {
			return nil, err
		}
		key := projectID + "\x00" + workflowID
		entry, ok := byCtx[key]
		if !ok {
			entry = &persistence.SkillRatingArms{
				SkillID: skillID, ProjectID: projectID, WorkflowID: workflowID,
			}
			byCtx[key] = entry
			order = append(order, key)
		}
		if treated == 1 {
			entry.Treatment = arm
		} else {
			entry.Baseline = arm
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]persistence.SkillRatingArms, 0, len(order))
	for _, k := range order {
		out = append(out, *byCtx[k])
	}
	return out, nil
}
