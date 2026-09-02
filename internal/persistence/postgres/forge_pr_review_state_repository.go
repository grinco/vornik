package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ForgePRReviewStateRepository is the Postgres implementation of the per-PR
// re-review state (migration 171).
//
// Design: https://docs.vornik.io
type ForgePRReviewStateRepository struct {
	db persistence.DBTX
}

// NewForgePRReviewStateRepository builds the repository.
func NewForgePRReviewStateRepository(db persistence.DBTX) *ForgePRReviewStateRepository {
	return &ForgePRReviewStateRepository{db: db}
}

var _ persistence.ForgePRReviewStateRepository = (*ForgePRReviewStateRepository)(nil)

// Get returns the row, or (nil, nil) when this PR has no state yet.
func (r *ForgePRReviewStateRepository) Get(ctx context.Context, projectID, repo string, number int) (*persistence.ForgePRReviewState, error) {
	var (
		st       persistence.ForgePRReviewState
		reviewed sql.NullTime
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT project_id, repo, number, task_id, pending_head_sha, reviewing_head_sha,
		        last_reviewed_head_sha, last_reviewed_at, auto_review_paused, updated_at
		   FROM forge_pr_review_state
		  WHERE project_id = $1 AND repo = $2 AND number = $3`,
		projectID, repo, number,
	).Scan(&st.ProjectID, &st.Repo, &st.Number, &st.TaskID, &st.PendingHeadSHA, &st.ReviewingHeadSHA,
		&st.LastReviewedHeadSHA, &reviewed, &st.AutoReviewPaused, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Not an error: a PR with no state is the normal first-delivery case,
		// and callers read it as "never reviewed, nothing in flight".
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: forge_pr_review_state get: %w", err)
	}
	if reviewed.Valid {
		t := reviewed.Time
		st.LastReviewedAt = &t
	}
	return &st, nil
}

// ClaimOrSupersede records headSHA as the newest observed head and returns the
// task id the row held BEFORE this call.
//
// One statement, deliberately. A read-then-write would let two deliveries
// arriving together both observe an empty claim and both enqueue a review — the
// duplicate this whole mechanism exists to prevent.
//
// `RETURNING task_id` yields the PRIOR claim precisely because the DO UPDATE
// never touches that column: the returned row carries the value already there
// (or the ” default on a fresh insert). Reading it with a sub-SELECT instead
// would depend on statement-snapshot semantics that are easy to misread; not
// writing the column at all makes the guarantee structural.
func (r *ForgePRReviewStateRepository) ClaimOrSupersede(ctx context.Context, projectID, repo string, number int, headSHA string) (string, error) {
	var prior string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO forge_pr_review_state
		        (project_id, repo, number, pending_head_sha, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET pending_head_sha = EXCLUDED.pending_head_sha,
		        updated_at       = now()
		 RETURNING task_id`,
		projectID, repo, number, headSHA,
	).Scan(&prior)
	if err != nil {
		return "", fmt.Errorf("postgres: forge_pr_review_state claim: %w", err)
	}
	return prior, nil
}

// SetTask points the row at the review task that now owns this PR.
func (r *ForgePRReviewStateRepository) SetTask(ctx context.Context, projectID, repo string, number int, taskID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state (project_id, repo, number, task_id, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET task_id = EXCLUDED.task_id, updated_at = now()`,
		projectID, repo, number, taskID)
	if err != nil {
		return fmt.Errorf("postgres: forge_pr_review_state set task: %w", err)
	}
	return nil
}

// MarkReviewed advances last_reviewed_head_sha and clears the claim. Called
// only after the review has actually posted.
func (r *ForgePRReviewStateRepository) MarkReviewed(ctx context.Context, projectID, repo string, number int, headSHA string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state
		        (project_id, repo, number, last_reviewed_head_sha, last_reviewed_at, task_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5, '', now())
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET last_reviewed_head_sha = EXCLUDED.last_reviewed_head_sha,
		        last_reviewed_at       = EXCLUDED.last_reviewed_at,
		        task_id                = '',
		        updated_at             = now()`,
		projectID, repo, number, headSHA, at.UTC())
	if err != nil {
		return fmt.Errorf("postgres: forge_pr_review_state mark reviewed: %w", err)
	}
	return nil
}

// SetPaused sets or clears the per-PR automatic-review suppression.
func (r *ForgePRReviewStateRepository) SetPaused(ctx context.Context, projectID, repo string, number int, paused bool) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state (project_id, repo, number, auto_review_paused, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET auto_review_paused = EXCLUDED.auto_review_paused, updated_at = now()`,
		projectID, repo, number, paused)
	if err != nil {
		return fmt.Errorf("postgres: forge_pr_review_state set paused: %w", err)
	}
	return nil
}

// BeginClosing releases the claim and records the head this review fetched, in
// one statement so the pair cannot be separated.
func (r *ForgePRReviewStateRepository) BeginClosing(ctx context.Context, projectID, repo string, number int, reviewingHeadSHA string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO forge_pr_review_state
		        (project_id, repo, number, task_id, reviewing_head_sha, updated_at)
		 VALUES ($1, $2, $3, '', $4, now())
		 ON CONFLICT (project_id, repo, number) DO UPDATE
		    SET task_id            = '',
		        reviewing_head_sha = EXCLUDED.reviewing_head_sha,
		        updated_at         = now()`,
		projectID, repo, number, reviewingHeadSHA)
	if err != nil {
		return fmt.Errorf("postgres: forge_pr_review_state begin closing: %w", err)
	}
	return nil
}
