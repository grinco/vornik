package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskPostMortemRepository implements
// persistence.TaskPostMortemRepository on PostgreSQL.
//
// One row per task (PRIMARY KEY on task_id). Re-running the
// explainer for the same task overwrites the prior summary —
// the schema is intentionally last-write-wins because
// post-mortems are derived data: if the operator re-triggers,
// they want the new attempt's summary, not the old one.
type TaskPostMortemRepository struct {
	db DBTX
}

// NewTaskPostMortemRepository constructs a new repo over db.
func NewTaskPostMortemRepository(db DBTX) *TaskPostMortemRepository {
	return &TaskPostMortemRepository{db: db}
}

// Record upserts a post-mortem row keyed on task_id. Atomic
// via ON CONFLICT — the caller doesn't need to pre-check for
// an existing row, and concurrent re-trigger races resolve
// without leaving a stale row in place.
func (r *TaskPostMortemRepository) Record(ctx context.Context, pm *persistence.TaskPostMortem) error {
	if pm == nil {
		return fmt.Errorf("nil post-mortem")
	}
	if pm.TaskID == "" {
		return fmt.Errorf("task ID required")
	}
	recordedAt := pm.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_post_mortems (
		    task_id, project_id, summary, model,
		    prompt_tokens, completion_tokens, cost_usd, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (task_id) DO UPDATE SET
		    project_id        = EXCLUDED.project_id,
		    summary           = EXCLUDED.summary,
		    model             = EXCLUDED.model,
		    prompt_tokens     = EXCLUDED.prompt_tokens,
		    completion_tokens = EXCLUDED.completion_tokens,
		    cost_usd          = EXCLUDED.cost_usd,
		    recorded_at       = EXCLUDED.recorded_at`,
		pm.TaskID, pm.ProjectID, pm.Summary, pm.Model,
		pm.PromptTokens, pm.CompletionTokens, pm.CostUSD, recordedAt,
	)
	return mapDBError(err)
}

// Get returns the cached post-mortem for a task, or
// persistence.ErrNotFound if none exists.
func (r *TaskPostMortemRepository) Get(ctx context.Context, taskID string) (*persistence.TaskPostMortem, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task ID required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT task_id, project_id, summary, model,
		       prompt_tokens, completion_tokens, cost_usd, recorded_at
		FROM task_post_mortems
		WHERE task_id = $1`, taskID)
	pm := &persistence.TaskPostMortem{}
	if err := row.Scan(
		&pm.TaskID, &pm.ProjectID, &pm.Summary, &pm.Model,
		&pm.PromptTokens, &pm.CompletionTokens, &pm.CostUSD, &pm.RecordedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	return pm, nil
}
