package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ForgeCIOutcomeRepository is the SQLite implementation of
// persistence.ForgeCIOutcomeRepository (schema parity with Postgres migration
// 181, 2026-09-08-forge-ci-outcomes-design.md §4.1).
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
// INSERT OR REPLACE rather than an upsert clause, and the full column list is
// supplied every time, so a re-run that uploaded no artifact CLEARS the
// previous attempt's excerpt. Leaving it would show an operator reading a green
// re-run the failed attempt's plan.
func (r *ForgeCIOutcomeRepository) Upsert(ctx context.Context, o *persistence.ForgeCIOutcome) error {
	jobs, err := json.Marshal(o.Jobs)
	if err != nil {
		return fmt.Errorf("marshal ci jobs: %w", err)
	}
	if len(o.Jobs) == 0 {
		jobs = []byte("[]")
	}
	var started any
	if !o.StartedAt.IsZero() {
		started = sqliteTime(o.StartedAt)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO forge_ci_outcomes (`+forgeCIOutcomeColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ProjectID, o.Repo, o.RunID, o.HeadSHA, o.Number,
		o.WorkflowName, o.WorkflowPath, o.RunAttempt, o.Conclusion,
		started, sqliteTime(o.CompletedAt), string(jobs),
		o.ArtifactExcerpt, o.ArtifactBytes, o.ArtifactTruncated, sqliteTime(o.RecordedAt),
	)
	return err
}

// ListByHeadSHA returns every run recorded for one commit, newest first.
func (r *ForgeCIOutcomeRepository) ListByHeadSHA(ctx context.Context, projectID, repo, headSHA string) ([]*persistence.ForgeCIOutcome, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+forgeCIOutcomeColumns+`
		  FROM forge_ci_outcomes
		 WHERE project_id = ? AND repo = ? AND head_sha = ?
		 ORDER BY completed_at DESC, run_id DESC`, projectID, repo, headSHA)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanForgeCIOutcomes(rows)
}

// PruneBefore deletes outcomes completed before the cutoff.
func (r *ForgeCIOutcomeRepository) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM forge_ci_outcomes WHERE completed_at < ?`, sqliteTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanForgeCIOutcomes(rows *sql.Rows) ([]*persistence.ForgeCIOutcome, error) {
	out := []*persistence.ForgeCIOutcome{}
	for rows.Next() {
		var o persistence.ForgeCIOutcome
		var started sql.NullString
		var completed, recorded string
		var jobs string
		if err := rows.Scan(&o.ProjectID, &o.Repo, &o.RunID, &o.HeadSHA, &o.Number,
			&o.WorkflowName, &o.WorkflowPath, &o.RunAttempt, &o.Conclusion,
			&started, &completed, &jobs,
			&o.ArtifactExcerpt, &o.ArtifactBytes, &o.ArtifactTruncated, &recorded); err != nil {
			return nil, err
		}
		if started.Valid && started.String != "" {
			o.StartedAt = parseCITime(started.String)
		}
		o.CompletedAt = parseCITime(completed)
		o.RecordedAt = parseCITime(recorded)
		if jobs != "" {
			if err := json.Unmarshal([]byte(jobs), &o.Jobs); err != nil {
				return nil, fmt.Errorf("decode ci jobs: %w", err)
			}
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// parseCITime reads the RFC3339Nano text sqliteTime writes.
//
// An unparseable value yields the zero time rather than an error: a malformed
// timestamp on one row must not fail a whole review's CI context, and the zero
// value already means "unknown" everywhere this struct is read.
func parseCITime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
