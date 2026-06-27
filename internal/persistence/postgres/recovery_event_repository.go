package postgres

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// RecoveryEventRepository implements persistence.RecoveryEventRepository
// using PostgreSQL. Append-only marker for graceful-recovery exits.
type RecoveryEventRepository struct {
	db DBTX
}

// NewRecoveryEventRepository creates a new RecoveryEventRepository.
func NewRecoveryEventRepository(db DBTX) *RecoveryEventRepository {
	return &RecoveryEventRepository{db: db}
}

// Record appends one recovery event. Idempotent on the (id) PK.
func (r *RecoveryEventRepository) Record(ctx context.Context, e *persistence.RecoveryEvent) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO recovery_events (
			id, project_id, task_id, execution_id, workflow_id, terminal_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		e.ID, e.ProjectID, e.TaskID, e.ExecutionID, e.WorkflowID, e.TerminalID, e.CreatedAt,
	)
	return mapDBError(err)
}

// ListRecent returns recent recovery events, newest first.
func (r *RecoveryEventRepository) ListRecent(ctx context.Context, projectID string, limit int) ([]*persistence.RecoveryEvent, error) {
	query := `
		SELECT id, project_id, task_id, execution_id, workflow_id, terminal_id, created_at
		FROM recovery_events WHERE 1=1`
	args := make([]any, 0, 2)
	argPos := 1
	if projectID != "" {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, projectID)
		argPos++
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argPos)
		args = append(args, limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var events []*persistence.RecoveryEvent
	for rows.Next() {
		var e persistence.RecoveryEvent
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.TaskID, &e.ExecutionID, &e.WorkflowID, &e.TerminalID, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}
