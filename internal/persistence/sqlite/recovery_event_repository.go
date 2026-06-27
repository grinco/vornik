package sqlite

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RecoveryEventRepository is the SQLite-backed implementation of
// persistence.RecoveryEventRepository. Append-only marker for
// graceful-recovery exits; Record is idempotent on the (id) PK.
type RecoveryEventRepository struct {
	db DBTX
}

// NewRecoveryEventRepository constructs a RecoveryEventRepository over db.
func NewRecoveryEventRepository(db DBTX) *RecoveryEventRepository {
	return &RecoveryEventRepository{db: db}
}

// Record inserts one recovery event. INSERT OR IGNORE keeps it idempotent.
func (r *RecoveryEventRepository) Record(ctx context.Context, e *persistence.RecoveryEvent) error {
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO recovery_events (
			id, project_id, task_id, execution_id, workflow_id, terminal_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ProjectID, e.TaskID, e.ExecutionID, e.WorkflowID, e.TerminalID,
		sqliteTime(createdAt),
	)
	return err
}

// ListRecent returns recent recovery events, newest first.
func (r *RecoveryEventRepository) ListRecent(ctx context.Context, projectID string, limit int) ([]*persistence.RecoveryEvent, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, task_id, execution_id, workflow_id, terminal_id, created_at
		FROM recovery_events WHERE 1=1`)
	args := make([]any, 0, 2)
	if projectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, projectID)
	}
	b.WriteString(" ORDER BY created_at DESC")
	if limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, limit)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []*persistence.RecoveryEvent
	for rows.Next() {
		var (
			e         persistence.RecoveryEvent
			createdAt sqlTime
		)
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.TaskID, &e.ExecutionID, &e.WorkflowID, &e.TerminalID, &createdAt,
		); err != nil {
			return nil, err
		}
		e.CreatedAt = createdAt.Time
		events = append(events, &e)
	}
	return events, rows.Err()
}
