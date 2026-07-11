package sqlite

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionNarrationRepository is the SQLite-backed implementation
// of persistence.ExecutionNarrationRepository. Real persistence (not
// a stub) — unlike the cross-replica live-event log, narration is
// the story-view's source of truth on every backend, single-process
// SQLite included.
//
// seq is computed the same way as the Postgres side (MAX(seq)+1 per
// execution_id, inside the INSERT via RETURNING) so both backends
// agree on ordering semantics. Foreign keys are declared in schema.go
// but not enforced (project-wide sqlite.go convention: foreign_keys
// pragma OFF) — cascade-on-delete is a Postgres-only guarantee here.
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
    ?, ?, ?, ?,
    COALESCE((SELECT MAX(seq) + 1 FROM execution_narration WHERE execution_id = ?), 0),
    ?, ?, ?, ?, ?, ?
)
RETURNING seq`
	var stepID interface{}
	if row.StepID != "" {
		stepID = row.StepID
	}
	var meta interface{}
	if len(row.Metadata) > 0 {
		meta = string(row.Metadata)
	}
	degraded := 0
	if row.Degraded {
		degraded = 1
	}
	var seq int64
	err := r.db.QueryRowContext(ctx, q,
		row.ID, row.ProjectID, row.TaskID, row.ExecutionID, row.ExecutionID,
		stepID, row.Kind, row.Text, degraded, meta, sqliteTime(time.Now()),
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
       COALESCE(metadata, ''), created_at
FROM execution_narration
WHERE execution_id = ?
ORDER BY seq ASC`
	rows, err := r.db.QueryContext(ctx, q, executionID)
	if err != nil {
		return nil, fmt.Errorf("execution_narration: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.ExecutionNarration
	for rows.Next() {
		var (
			n         persistence.ExecutionNarration
			degraded  int
			metaText  string
			createdAt sqlTime
		)
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.TaskID, &n.ExecutionID, &n.Seq, &n.StepID,
			&n.Kind, &n.Text, &degraded, &metaText, &createdAt); err != nil {
			return nil, fmt.Errorf("execution_narration: scan: %w", err)
		}
		n.Degraded = degraded != 0
		if metaText != "" {
			n.Metadata = []byte(metaText)
		}
		n.CreatedAt = createdAt.Time
		out = append(out, &n)
	}
	return out, rows.Err()
}
