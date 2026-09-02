package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ForgePRReviewStateRepository is the SQLite implementation of the per-PR
// re-review state.
//
// Design: https://docs.vornik.io
//
// Durable on this backend, unlike ChannelSessionRepository's deliberate no-op:
// the ABSORBING claim is what stops a push burst enqueueing one review per push,
// and a no-op here would silently restore the cost multiplier the design exists
// to remove on every sqlite deployment.
type ForgePRReviewStateRepository struct {
	db *sql.DB
}

// NewForgePRReviewStateRepository builds the repository.
func NewForgePRReviewStateRepository(db *sql.DB) *ForgePRReviewStateRepository {
	return &ForgePRReviewStateRepository{db: db}
}

var _ persistence.ForgePRReviewStateRepository = (*ForgePRReviewStateRepository)(nil)

// Get returns the row, or (nil, nil) when this PR has no state yet.
func (r *ForgePRReviewStateRepository) Get(ctx context.Context, projectID, repo string, number int) (*persistence.ForgePRReviewState, error) {
	var (
		st       persistence.ForgePRReviewState
		reviewed sql.NullString
		updated  string
		paused   int
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT project_id, repo, number, task_id, pending_head_sha, reviewing_head_sha,
		        last_reviewed_head_sha, last_reviewed_at, auto_review_paused, updated_at
		   FROM forge_pr_review_state
		  WHERE project_id = ? AND repo = ? AND number = ?`,
		projectID, repo, number,
	).Scan(&st.ProjectID, &st.Repo, &st.Number, &st.TaskID, &st.PendingHeadSHA, &st.ReviewingHeadSHA,
		&st.LastReviewedHeadSHA, &reviewed, &paused, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: forge_pr_review_state get: %w", err)
	}
	st.AutoReviewPaused = paused != 0
	if reviewed.Valid && reviewed.String != "" {
		if t, perr := time.Parse(time.RFC3339Nano, reviewed.String); perr == nil {
			st.LastReviewedAt = &t
		}
	}
	if t, perr := time.Parse(time.RFC3339Nano, updated); perr == nil {
		st.UpdatedAt = t
	}
	return &st, nil
}

// ClaimOrSupersede records headSHA and returns the task id held BEFORE the call.
//
// One statement, for the same reason as the Postgres implementation: a
// read-then-write would let two deliveries both observe an empty claim. SQLite's
// RETURNING (3.35+) gives the post-statement row, and since the DO UPDATE never
// writes task_id, that value IS the prior claim.
func (r *ForgePRReviewStateRepository) ClaimOrSupersede(ctx context.Context, projectID, repo string, number int, headSHA string) (string, error) {
	var prior string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO forge_pr_review_state
		        (project_id, repo, number, pending_head_sha, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET pending_head_sha = excluded.pending_head_sha,
		        updated_at       = excluded.updated_at
		 RETURNING task_id`,
		projectID, repo, number, headSHA, sqliteTime(time.Now()),
	).Scan(&prior)
	if err != nil {
		return "", fmt.Errorf("sqlite: forge_pr_review_state claim: %w", err)
	}
	return prior, nil
}

// SetTask points the row at the review task that now owns this PR.
func (r *ForgePRReviewStateRepository) SetTask(ctx context.Context, projectID, repo string, number int, taskID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state (project_id, repo, number, task_id, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET task_id = excluded.task_id, updated_at = excluded.updated_at`,
		projectID, repo, number, taskID, sqliteTime(time.Now()))
	if err != nil {
		return fmt.Errorf("sqlite: forge_pr_review_state set task: %w", err)
	}
	return nil
}

// MarkReviewed advances last_reviewed_head_sha and clears the claim.
func (r *ForgePRReviewStateRepository) MarkReviewed(ctx context.Context, projectID, repo string, number int, headSHA string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state
		        (project_id, repo, number, last_reviewed_head_sha, last_reviewed_at, task_id, updated_at)
		 VALUES (?, ?, ?, ?, ?, '', ?)
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET last_reviewed_head_sha = excluded.last_reviewed_head_sha,
		        last_reviewed_at       = excluded.last_reviewed_at,
		        task_id                = '',
		        updated_at             = excluded.updated_at`,
		projectID, repo, number, headSHA, sqliteTime(at), sqliteTime(time.Now()))
	if err != nil {
		return fmt.Errorf("sqlite: forge_pr_review_state mark reviewed: %w", err)
	}
	return nil
}

// SetPaused sets or clears the per-PR automatic-review suppression.
func (r *ForgePRReviewStateRepository) SetPaused(ctx context.Context, projectID, repo string, number int, paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state (project_id, repo, number, auto_review_paused, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET auto_review_paused = excluded.auto_review_paused, updated_at = excluded.updated_at`,
		projectID, repo, number, v, sqliteTime(time.Now()))
	if err != nil {
		return fmt.Errorf("sqlite: forge_pr_review_state set paused: %w", err)
	}
	return nil
}

// BeginClosing releases the claim and records the head this review fetched, in
// one statement so the pair cannot be separated.
func (r *ForgePRReviewStateRepository) BeginClosing(ctx context.Context, projectID, repo string, number int, reviewingHeadSHA string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state
		        (project_id, repo, number, task_id, reviewing_head_sha, updated_at)
		 VALUES (?, ?, ?, '', ?, ?)
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET task_id            = '',
		        reviewing_head_sha = excluded.reviewing_head_sha,
		        updated_at         = excluded.updated_at`,
		projectID, repo, number, reviewingHeadSHA, sqliteTime(time.Now()))
	if err != nil {
		return fmt.Errorf("sqlite: forge_pr_review_state begin closing: %w", err)
	}
	return nil
}
