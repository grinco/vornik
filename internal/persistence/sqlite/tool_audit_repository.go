package sqlite

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ToolAuditRepository is the SQLite-backed implementation of
// persistence.ToolAuditRepository. Log is idempotent on the primary
// key (id) — the realtime per-call POST and the post-step batch
// persist may race; whichever lands first wins.
type ToolAuditRepository struct {
	db DBTX
}

// NewToolAuditRepository constructs a ToolAuditRepository over db.
func NewToolAuditRepository(db DBTX) *ToolAuditRepository {
	return &ToolAuditRepository{db: db}
}

// Log inserts one audit entry. INSERT OR IGNORE swallows
// uniqueness violations on the primary key so repeated writes from
// the streaming + batch paths converge.
func (r *ToolAuditRepository) Log(ctx context.Context, entry *persistence.ToolAuditEntry) error {
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO tool_audit_log (
			id, project_id, task_id, execution_id, step_id,
			tool_name, tool_input, tool_output, duration_ms,
			outcome, outcome_class, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.ProjectID, entry.TaskID, entry.ExecutionID, entry.StepID,
		entry.ToolName, entry.ToolInput, entry.ToolOutput, entry.DurationMs,
		entry.Outcome, entry.OutcomeClass,
		sqliteTime(createdAt),
	)
	return err
}

// List returns rows matching filter, newest-first.
func (r *ToolAuditRepository) List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, task_id, execution_id, step_id,
		       tool_name, tool_input, tool_output, duration_ms,
		       COALESCE(outcome, ''), COALESCE(outcome_class, ''), created_at
		FROM tool_audit_log WHERE 1=1`)
	args := make([]any, 0, 5)

	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.TaskID != nil {
		b.WriteString(" AND task_id = ?")
		args = append(args, *filter.TaskID)
	}
	if filter.ExecutionID != nil {
		b.WriteString(" AND execution_id = ?")
		args = append(args, *filter.ExecutionID)
	}
	if filter.StepID != nil {
		b.WriteString(" AND step_id = ?")
		args = append(args, *filter.StepID)
	}
	if filter.ToolName != nil {
		b.WriteString(" AND tool_name = ?")
		args = append(args, *filter.ToolName)
	}

	b.WriteString(" ORDER BY created_at DESC")
	if filter.PageSize > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, filter.PageSize)
	}
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []*persistence.ToolAuditEntry
	for rows.Next() {
		var (
			e         persistence.ToolAuditEntry
			createdAt sqlTime
		)
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.TaskID, &e.ExecutionID, &e.StepID,
			&e.ToolName, &e.ToolInput, &e.ToolOutput, &e.DurationMs,
			&e.Outcome, &e.OutcomeClass, &createdAt,
		); err != nil {
			return nil, err
		}
		e.CreatedAt = createdAt.Time
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

// CountByTool groups invocation counts by tool name for one execution.
func (r *ToolAuditRepository) CountByTool(ctx context.Context, executionID string) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT tool_name, COUNT(*) FROM tool_audit_log
		WHERE execution_id = ?
		GROUP BY tool_name
		ORDER BY COUNT(*) DESC`, executionID)
	if err != nil {
		return nil, err
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

// ToolLatencyP95ByProjectTool returns p95 call latency (seconds) + count per
// (project, tool) over calls created at/after since. Durations are read from
// duration_ms; p95 is computed in Go (persistence.P95Seconds).
func (r *ToolAuditRepository) ToolLatencyP95ByProjectTool(ctx context.Context, since time.Time) ([]persistence.ToolLatencyStat, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT project_id, tool_name, duration_ms FROM tool_audit_log
		WHERE created_at >= ? AND duration_ms >= 0`, sqliteTime(since.UTC()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type key struct{ project, tool string }
	byKey := make(map[key][]float64)
	for rows.Next() {
		var project, tool string
		var durMs int64
		if err := rows.Scan(&project, &tool, &durMs); err != nil {
			return nil, err
		}
		byKey[key{project, tool}] = append(byKey[key{project, tool}], float64(durMs)/1000.0)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]persistence.ToolLatencyStat, 0, len(byKey))
	for k, durs := range byKey {
		out = append(out, persistence.ToolLatencyStat{
			ProjectID: k.project, ToolName: k.tool,
			P95Seconds: persistence.P95Seconds(durs), Count: int64(len(durs)),
		})
	}
	return out, nil
}
