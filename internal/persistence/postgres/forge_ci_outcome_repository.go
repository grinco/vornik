package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ForgeCIOutcomeRepository is the PostgreSQL implementation of
// persistence.ForgeCIOutcomeRepository (migration 181,
// 2026-09-08-forge-ci-outcomes-design.md §4.1).
type ForgeCIOutcomeRepository struct {
	db DBTX
}

// NewForgeCIOutcomeRepository constructs the repo over db.
func NewForgeCIOutcomeRepository(db DBTX) *ForgeCIOutcomeRepository {
	return &ForgeCIOutcomeRepository{db: db}
}

const forgeCIOutcomeColumns = `project_id, repo, run_id, head_sha, number,
	workflow_name, workflow_path, run_attempt, conclusion,
	started_at, completed_at, jobs_json,
	artifact_excerpt, artifact_bytes, artifact_truncated, recorded_at`

// Upsert records a completed run.
//
// ON CONFLICT updates EVERY column, including the artifact ones. That is
// deliberate: a re-run that uploaded no artifact must CLEAR the previous
// attempt's excerpt rather than leave it attached to a different attempt, or a
// green re-run would show an operator the failed attempt's plan.
func (r *ForgeCIOutcomeRepository) Upsert(ctx context.Context, o *persistence.ForgeCIOutcome) error {
	jobs, err := json.Marshal(o.Jobs)
	if err != nil {
		return fmt.Errorf("marshal ci jobs: %w", err)
	}
	if len(o.Jobs) == 0 {
		jobs = []byte("[]")
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO forge_ci_outcomes (`+forgeCIOutcomeColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (project_id, repo, run_id) DO UPDATE SET
			head_sha = EXCLUDED.head_sha,
			number = EXCLUDED.number,
			workflow_name = EXCLUDED.workflow_name,
			workflow_path = EXCLUDED.workflow_path,
			run_attempt = EXCLUDED.run_attempt,
			conclusion = EXCLUDED.conclusion,
			started_at = EXCLUDED.started_at,
			completed_at = EXCLUDED.completed_at,
			jobs_json = EXCLUDED.jobs_json,
			artifact_excerpt = EXCLUDED.artifact_excerpt,
			artifact_bytes = EXCLUDED.artifact_bytes,
			artifact_truncated = EXCLUDED.artifact_truncated,
			recorded_at = EXCLUDED.recorded_at`,
		o.ProjectID, o.Repo, o.RunID, o.HeadSHA, o.Number,
		o.WorkflowName, o.WorkflowPath, o.RunAttempt, o.Conclusion,
		nullTimeOrNil(o.StartedAt), o.CompletedAt.UTC(), string(jobs),
		o.ArtifactExcerpt, o.ArtifactBytes, o.ArtifactTruncated, o.RecordedAt.UTC(),
	)
	return mapDBError(err)
}

// ListByHeadSHA returns every run recorded for one commit, newest first.
func (r *ForgeCIOutcomeRepository) ListByHeadSHA(ctx context.Context, projectID, repo, headSHA string) ([]*persistence.ForgeCIOutcome, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+forgeCIOutcomeColumns+`
		  FROM forge_ci_outcomes
		 WHERE project_id = $1 AND repo = $2 AND head_sha = $3
		 ORDER BY completed_at DESC, run_id DESC`, projectID, repo, headSHA)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	return scanForgeCIOutcomes(rows)
}

// PruneBefore deletes outcomes completed before the cutoff.
func (r *ForgeCIOutcomeRepository) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM forge_ci_outcomes WHERE completed_at < $1`, cutoff.UTC())
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

func scanForgeCIOutcomes(rows *sql.Rows) ([]*persistence.ForgeCIOutcome, error) {
	out := []*persistence.ForgeCIOutcome{}
	for rows.Next() {
		var o persistence.ForgeCIOutcome
		var started sql.NullTime
		var jobs string
		if err := rows.Scan(&o.ProjectID, &o.Repo, &o.RunID, &o.HeadSHA, &o.Number,
			&o.WorkflowName, &o.WorkflowPath, &o.RunAttempt, &o.Conclusion,
			&started, &o.CompletedAt, &jobs,
			&o.ArtifactExcerpt, &o.ArtifactBytes, &o.ArtifactTruncated, &o.RecordedAt); err != nil {
			return nil, err
		}
		if started.Valid {
			o.StartedAt = started.Time.UTC()
		}
		o.CompletedAt = o.CompletedAt.UTC()
		o.RecordedAt = o.RecordedAt.UTC()
		if jobs != "" {
			if err := json.Unmarshal([]byte(jobs), &o.Jobs); err != nil {
				return nil, fmt.Errorf("decode ci jobs: %w", err)
			}
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// nullTimeOrNil sends a zero time.Time as SQL NULL.
//
// started_at is nullable because a run can be recorded from an event that
// carries no start time, and storing the zero instant instead would put
// year 1 into a column an operator reads as a timestamp.
func nullTimeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
