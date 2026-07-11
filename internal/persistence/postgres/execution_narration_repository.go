package postgres

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionNarrationRepository implements
// persistence.ExecutionNarrationRepository against PostgreSQL.
//
// Insert computes the next per-execution seq via
// `SELECT COALESCE(MAX(seq) + 1, 0)` inside the INSERT — same
// pattern as ExecutionLiveEventRepository.Append. The narrator is a
// single in-daemon goroutine processing one bus stream sequentially,
// so concurrent Insert on the same execution_id doesn't happen in
// practice; the unique (execution_id, seq) index is the backstop.
type ExecutionNarrationRepository struct {
	db DBTX
}

// NewExecutionNarrationRepository constructs the repo over db.
func NewExecutionNarrationRepository(db DBTX) *ExecutionNarrationRepository {
	return &ExecutionNarrationRepository{db: db}
}

// Insert persists one narration row and returns the assigned seq.
func (r *ExecutionNarrationRepository) Insert(ctx context.Context, row *persistence.ExecutionNarration) (int64, error) {
	if row == nil {
		return 0, fmt.Errorf("execution_narration: nil row")
	}
	if row.ExecutionID == "" || row.Kind == "" {
		return 0, fmt.Errorf("execution_narration: execution_id + kind required")
	}
	const q = `
INSERT INTO execution_narration (
    id, project_id, task_id, execution_id, seq, step_id, kind, text, degraded, metadata, created_at
) VALUES (
    $1, $2, $3, $4,
    COALESCE((SELECT MAX(seq) + 1 FROM execution_narration WHERE execution_id = $4), 0),
    $5, $6, $7, $8, $9, NOW()
)
RETURNING seq`
	var stepID interface{}
	if row.StepID != "" {
		stepID = row.StepID
	}
	var meta interface{} = row.Metadata
	if len(row.Metadata) == 0 {
		meta = nil
	}
	var seq int64
	err := r.db.QueryRowContext(ctx, q,
		row.ID, row.ProjectID, row.TaskID, row.ExecutionID,
		stepID, row.Kind, row.Text, row.Degraded, meta,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("execution_narration: insert: %w", err)
	}
	return seq, nil
}

// ListByExecution returns every row for executionID, ordered by seq
// ascending.
func (r *ExecutionNarrationRepository) ListByExecution(ctx context.Context, executionID string) ([]*persistence.ExecutionNarration, error) {
	if executionID == "" {
		return nil, fmt.Errorf("execution_narration: execution_id required")
	}
	const q = `
SELECT id, project_id, task_id, execution_id, seq, COALESCE(step_id, ''), kind, text, degraded,
       COALESCE(metadata::text, ''), created_at
FROM execution_narration
WHERE execution_id = $1
ORDER BY seq ASC`
	rows, err := r.db.QueryContext(ctx, q, executionID)
	if err != nil {
		return nil, fmt.Errorf("execution_narration: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.ExecutionNarration
	for rows.Next() {
		var (
			n        persistence.ExecutionNarration
			metaText string
		)
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.TaskID, &n.ExecutionID, &n.Seq, &n.StepID,
			&n.Kind, &n.Text, &n.Degraded, &metaText, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("execution_narration: scan: %w", err)
		}
		if metaText != "" {
			n.Metadata = []byte(metaText)
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}
