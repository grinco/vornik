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

// forgeCIOutcomeReadColumns adds commented_at, which the upsert never writes.
const forgeCIOutcomeReadColumns = forgeCIOutcomeColumns + `, commented_at`

// Upsert records a completed run.
//
// ON CONFLICT DO UPDATE naming its columns, NOT `INSERT OR REPLACE`.
//
// The full column list is still supplied every time, so a re-run that uploaded
// no artifact CLEARS the previous attempt's excerpt — leaving it would show an
// operator reading a green re-run the failed attempt's plan.
//
// But `INSERT OR REPLACE` replaces the WHOLE ROW from the supplied values,
// which would null `commented_at` on every redelivery and post a second comment
// on the same run (design §13.6). Postgres preserves a column simply omitted
// from its SET list; SQLite does not, so the statement has to name what it
// means to change. This is the one column the upsert must not touch: every
// other records what CI REPORTED, this records what Forge DID about it.
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
		INSERT INTO forge_ci_outcomes (`+forgeCIOutcomeColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (project_id, repo, run_id) DO UPDATE SET
			head_sha = excluded.head_sha,
			number = excluded.number,
			workflow_name = excluded.workflow_name,
			workflow_path = excluded.workflow_path,
			run_attempt = excluded.run_attempt,
			conclusion = excluded.conclusion,
			started_at = excluded.started_at,
			completed_at = excluded.completed_at,
			jobs_json = excluded.jobs_json,
			artifact_excerpt = excluded.artifact_excerpt,
			artifact_bytes = excluded.artifact_bytes,
			artifact_truncated = excluded.artifact_truncated,
			recorded_at = excluded.recorded_at`,
		o.ProjectID, o.Repo, o.RunID, o.HeadSHA, o.Number,
		o.WorkflowName, o.WorkflowPath, o.RunAttempt, o.Conclusion,
		started, sqliteTime(o.CompletedAt), string(jobs),
		o.ArtifactExcerpt, o.ArtifactBytes, o.ArtifactTruncated, sqliteTime(o.RecordedAt),
	)
	return err
}

// ClaimComment takes the right to comment, atomically.
func (r *ForgeCIOutcomeRepository) ClaimComment(ctx context.Context, projectID, repo string, runID int64, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE forge_ci_outcomes SET commented_at = ?
		 WHERE project_id = ? AND repo = ? AND run_id = ?
		   AND commented_at IS NULL`,
		sqliteTime(at), projectID, repo, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReleaseComment returns the claim after a failed post.
func (r *ForgeCIOutcomeRepository) ReleaseComment(ctx context.Context, projectID, repo string, runID int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE forge_ci_outcomes SET commented_at = NULL
		 WHERE project_id = ? AND repo = ? AND run_id = ?`,
		projectID, repo, runID)
	return err
}

// ListByHeadSHA returns every run recorded for one commit, newest first.
func (r *ForgeCIOutcomeRepository) ListByHeadSHA(ctx context.Context, projectID, repo, headSHA string) ([]*persistence.ForgeCIOutcome, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+forgeCIOutcomeReadColumns+`
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
		var started, commented sql.NullString
		var completed, recorded string
		var jobs string
		if err := rows.Scan(&o.ProjectID, &o.Repo, &o.RunID, &o.HeadSHA, &o.Number,
			&o.WorkflowName, &o.WorkflowPath, &o.RunAttempt, &o.Conclusion,
			&started, &completed, &jobs,
			&o.ArtifactExcerpt, &o.ArtifactBytes, &o.ArtifactTruncated, &recorded,
			&commented); err != nil {
			return nil, err
		}
		if started.Valid && started.String != "" {
			o.StartedAt = parseCITime(started.String)
		}
		o.CompletedAt = parseCITime(completed)
		o.RecordedAt = parseCITime(recorded)
		if commented.Valid && commented.String != "" {
			o.CommentedAt = parseCITime(commented.String)
		}
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
