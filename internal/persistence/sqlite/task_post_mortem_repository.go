package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskPostMortemRepository persists LLM-generated failure explainers.
// One row per task — Record upserts, Get fetches the cached row.
type TaskPostMortemRepository struct {
	db DBTX
}

func NewTaskPostMortemRepository(db DBTX) *TaskPostMortemRepository {
	return &TaskPostMortemRepository{db: db}
}

// Record upserts the post-mortem keyed on task_id.
func (r *TaskPostMortemRepository) Record(ctx context.Context, pm *persistence.TaskPostMortem) error {
	if pm == nil {
		return fmt.Errorf("nil post-mortem")
	}
	if pm.TaskID == "" {
		return fmt.Errorf("task ID required")
	}
	if pm.RecordedAt.IsZero() {
		pm.RecordedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_post_mortems (
			task_id, project_id, summary, model,
			prompt_tokens, completion_tokens, cost_usd, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET
			project_id        = excluded.project_id,
			summary           = excluded.summary,
			model             = excluded.model,
			prompt_tokens     = excluded.prompt_tokens,
			completion_tokens = excluded.completion_tokens,
			cost_usd          = excluded.cost_usd,
			recorded_at       = excluded.recorded_at`,
		pm.TaskID, pm.ProjectID, pm.Summary, pm.Model,
		pm.PromptTokens, pm.CompletionTokens, pm.CostUSD, sqliteTime(pm.RecordedAt),
	)
	return err
}

// Get returns the cached post-mortem, ErrNotFound otherwise.
func (r *TaskPostMortemRepository) Get(ctx context.Context, taskID string) (*persistence.TaskPostMortem, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task ID required")
	}
	pm := &persistence.TaskPostMortem{}
	var recordedAt sqlTime
	err := r.db.QueryRowContext(ctx, `
		SELECT task_id, project_id, summary, model,
		       prompt_tokens, completion_tokens, cost_usd, recorded_at
		FROM task_post_mortems WHERE task_id = ?`, taskID,
	).Scan(
		&pm.TaskID, &pm.ProjectID, &pm.Summary, &pm.Model,
		&pm.PromptTokens, &pm.CompletionTokens, &pm.CostUSD, &recordedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	pm.RecordedAt = recordedAt.Time
	return pm, nil
}
