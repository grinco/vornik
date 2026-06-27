package postgres

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ToolAuditRepository implements persistence.ToolAuditRepository using PostgreSQL.
type ToolAuditRepository struct {
	db DBTX
}

// NewToolAuditRepository creates a new ToolAuditRepository.
func NewToolAuditRepository(db DBTX) *ToolAuditRepository {
	return &ToolAuditRepository{db: db}
}

// Log records a single tool invocation. Idempotent on the (id) PK
// so the realtime per-call audit POST from the agent and the
// post-step batch persist from result.json can both fire safely
// — whichever lands first wins, the second is a no-op. Without
// ON CONFLICT a daemon-side post-step batch race against an
// already-streamed row would surface as a unique-violation error
// in the executor's warn log; with it, the post-step batch is a
// safety net for crashed agents that didn't manage to stream
// every tool call before exiting.
func (r *ToolAuditRepository) Log(ctx context.Context, entry *persistence.ToolAuditEntry) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tool_audit_log (
			id, project_id, task_id, execution_id, step_id,
			tool_name, tool_input, tool_output, duration_ms, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO NOTHING`,
		entry.ID, entry.ProjectID, entry.TaskID, entry.ExecutionID, entry.StepID,
		entry.ToolName, entry.ToolInput, entry.ToolOutput, entry.DurationMs, entry.CreatedAt,
	)
	return mapDBError(err)
}

// List returns tool audit entries matching the filter.
func (r *ToolAuditRepository) List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	query := `
		SELECT id, project_id, task_id, execution_id, step_id,
		       tool_name, tool_input, tool_output, duration_ms, created_at
		FROM tool_audit_log WHERE 1=1`
	args := make([]any, 0, 5)
	argPos := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.TaskID != nil {
		query += fmt.Sprintf(" AND task_id = $%d", argPos)
		args = append(args, *filter.TaskID)
		argPos++
	}
	if filter.ExecutionID != nil {
		query += fmt.Sprintf(" AND execution_id = $%d", argPos)
		args = append(args, *filter.ExecutionID)
		argPos++
	}
	if filter.StepID != nil {
		query += fmt.Sprintf(" AND step_id = $%d", argPos)
		args = append(args, *filter.StepID)
		argPos++
	}
	if filter.ToolName != nil {
		query += fmt.Sprintf(" AND tool_name = $%d", argPos)
		args = append(args, *filter.ToolName)
		argPos++
	}

	query += " ORDER BY created_at DESC"
	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argPos)
		args = append(args, filter.PageSize)
		argPos++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argPos)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*persistence.ToolAuditEntry
	for rows.Next() {
		var e persistence.ToolAuditEntry
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.TaskID, &e.ExecutionID, &e.StepID,
			&e.ToolName, &e.ToolInput, &e.ToolOutput, &e.DurationMs, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

// CountByTool returns tool invocation counts grouped by tool name.
func (r *ToolAuditRepository) CountByTool(ctx context.Context, executionID string) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT tool_name, COUNT(*) FROM tool_audit_log
		WHERE execution_id = $1
		GROUP BY tool_name
		ORDER BY COUNT(*) DESC`, executionID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int64)
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	return counts, rows.Err()
}
