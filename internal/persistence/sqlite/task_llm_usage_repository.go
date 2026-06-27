package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskLLMUsageRepository persists per-step LLM token usage + cost.
// Hot path is Record/Upsert (one row per step or per streaming
// iteration); the dashboard aggregators all join + filter on
// task_llm_usage in various shapes.
//
// SQLite caveats: the big-screen dashboard methods —
// AggregateByProject / AggregateBySource / AggregateByRoleModel /
// TimeSeriesByDay / TopTasks / TaskCostBreakdown — return
// ErrUnimplemented for now. The first three need DISTINCT-task
// counting via GROUP BY which works the same on SQLite but TopTasks
// + TaskCostBreakdown join tasks (FK off in phase 2, would need
// schema seeding for tests). Round 1 wired the table; round 4 (or
// when the dashboards are actually exercised in SQLite tests) can
// fill these in.
type TaskLLMUsageRepository struct {
	db DBTX
}

func NewTaskLLMUsageRepository(db DBTX) *TaskLLMUsageRepository {
	return &TaskLLMUsageRepository{db: db}
}

// Record inserts one usage row.
func (r *TaskLLMUsageRepository) Record(ctx context.Context, u *persistence.TaskLLMUsage) error {
	if u == nil {
		return fmt.Errorf("nil usage row")
	}
	if u.RecordedAt.IsZero() {
		u.RecordedAt = time.Now().UTC()
	}
	source := u.Source
	if source == "" {
		source = persistence.TaskLLMUsageSourceWorkflowStep
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_llm_usage (
			id, project_id, task_id, execution_id, step_id,
			role, model, prompt_tokens, completion_tokens, iterations,
			cost_usd, source, session_id, recorded_at,
			cache_creation_tokens, cache_read_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.ProjectID, u.TaskID, u.ExecutionID, u.StepID,
		u.Role, u.Model, u.PromptTokens, u.CompletionTokens, u.Iterations,
		u.CostUSD, source, u.SessionID, sqliteTime(u.RecordedAt),
		u.CacheCreationTokens, u.CacheReadTokens,
	)
	return err
}

// Upsert is the streaming-friendly variant — same ID overwrites.
func (r *TaskLLMUsageRepository) Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error {
	if u == nil {
		return fmt.Errorf("nil usage row")
	}
	if u.RecordedAt.IsZero() {
		u.RecordedAt = time.Now().UTC()
	}
	source := u.Source
	if source == "" {
		source = persistence.TaskLLMUsageSourceWorkflowStep
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_llm_usage (
			id, project_id, task_id, execution_id, step_id,
			role, model, prompt_tokens, completion_tokens, iterations,
			cost_usd, source, session_id, recorded_at,
			cache_creation_tokens, cache_read_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			prompt_tokens         = excluded.prompt_tokens,
			completion_tokens     = excluded.completion_tokens,
			iterations            = excluded.iterations,
			cost_usd              = excluded.cost_usd,
			recorded_at           = excluded.recorded_at,
			cache_creation_tokens = excluded.cache_creation_tokens,
			cache_read_tokens     = excluded.cache_read_tokens`,
		u.ID, u.ProjectID, u.TaskID, u.ExecutionID, u.StepID,
		u.Role, u.Model, u.PromptTokens, u.CompletionTokens, u.Iterations,
		u.CostUSD, source, u.SessionID, sqliteTime(u.RecordedAt),
		u.CacheCreationTokens, u.CacheReadTokens,
	)
	return err
}

// List returns rows matching filter, newest-first.
func (r *TaskLLMUsageRepository) List(ctx context.Context, f persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, task_id, execution_id, step_id,
		       role, model, prompt_tokens, completion_tokens, iterations,
		       cost_usd, source, session_id, recorded_at,
		       cache_creation_tokens, cache_read_tokens
		FROM task_llm_usage WHERE 1=1`)
	args := make([]any, 0, 6)

	if f.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *f.ProjectID)
	}
	if f.TaskID != nil {
		b.WriteString(" AND task_id = ?")
		args = append(args, *f.TaskID)
	}
	if f.Role != nil {
		b.WriteString(" AND role = ?")
		args = append(args, *f.Role)
	}
	if f.Since != nil {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(*f.Since))
	}
	if f.Until != nil {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(*f.Until))
	}
	b.WriteString(" ORDER BY recorded_at DESC")
	if f.PageSize > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, f.PageSize)
	}
	if f.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, f.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TaskLLMUsage
	for rows.Next() {
		var (
			u           persistence.TaskLLMUsage
			taskID      sql.NullString
			executionID sql.NullString
			sessionID   sql.NullString
			recordedAt  sqlTime
		)
		if err := rows.Scan(
			&u.ID, &u.ProjectID, &taskID, &executionID, &u.StepID,
			&u.Role, &u.Model, &u.PromptTokens, &u.CompletionTokens, &u.Iterations,
			&u.CostUSD, &u.Source, &sessionID, &recordedAt,
			&u.CacheCreationTokens, &u.CacheReadTokens,
		); err != nil {
			return nil, err
		}
		if taskID.Valid {
			u.TaskID = &taskID.String
		}
		if executionID.Valid {
			u.ExecutionID = &executionID.String
		}
		if sessionID.Valid {
			u.SessionID = &sessionID.String
		}
		u.RecordedAt = recordedAt.Time
		out = append(out, &u)
	}
	return out, rows.Err()
}

// SumCostByProject returns total cost for a project within a window.
func (r *TaskLLMUsageRepository) SumCostByProject(ctx context.Context, projectID string, since, until time.Time) (float64, error) {
	var b strings.Builder
	b.WriteString(`SELECT COALESCE(SUM(cost_usd), 0) FROM task_llm_usage WHERE project_id = ?`)
	args := []any{projectID}
	if !since.IsZero() {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	var sum float64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

// SumCostByAPIKey sums LLM spend across every task created by a
// companion API key, joining task_llm_usage → tasks on the companion
// api_key_id embedded in the task payload. SQLite extracts the JSON
// path with json_extract (Postgres uses the -> / ->> operators). See
// finding #2 / mitigation plan §7.2.
func (r *TaskLLMUsageRepository) SumCostByAPIKey(ctx context.Context, apiKeyID string, since, until time.Time) (float64, error) {
	var b strings.Builder
	b.WriteString(`SELECT COALESCE(SUM(u.cost_usd), 0)
		FROM task_llm_usage u
		JOIN tasks t ON t.id = u.task_id
		WHERE json_extract(t.payload, '$.companion.api_key_id') = ?`)
	args := []any{apiKeyID}
	if !since.IsZero() {
		b.WriteString(" AND u.recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND u.recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	var sum float64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

// MeanCostByWorkflow returns the average per-task LLM cost for a
// (project, workflow) pair within an optional window, plus the
// number of distinct tasks covered. Joins task_llm_usage → tasks on
// workflow_id and divides total spend by the distinct-task count.
// (0, 0, nil) when no prior tasks exist. See LLD-21 cost_estimate /
// drift-mitigation §8.2.
func (r *TaskLLMUsageRepository) MeanCostByWorkflow(ctx context.Context, projectID, workflowID string, since, until time.Time) (float64, int, error) {
	var b strings.Builder
	b.WriteString(`SELECT COALESCE(SUM(u.cost_usd), 0),
	                      COUNT(DISTINCT u.task_id)
		FROM task_llm_usage u
		JOIN tasks t ON t.id = u.task_id
		WHERE t.project_id = ? AND t.workflow_id = ? AND u.task_id IS NOT NULL`)
	args := []any{projectID, workflowID}
	if !since.IsZero() {
		b.WriteString(" AND u.recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND u.recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	var total float64
	var taskCount int
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&total, &taskCount); err != nil {
		return 0, 0, err
	}
	if taskCount == 0 {
		return 0, 0, nil
	}
	return total / float64(taskCount), taskCount, nil
}

// SumCost returns total cost across all projects within a window.
func (r *TaskLLMUsageRepository) SumCost(ctx context.Context, since, until time.Time) (float64, error) {
	var b strings.Builder
	b.WriteString(`SELECT COALESCE(SUM(cost_usd), 0) FROM task_llm_usage WHERE 1=1`)
	args := make([]any, 0, 2)
	if !since.IsZero() {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	var sum float64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

// AggregateByRoleModel groups spend by (role, model). Postgres-side
// supports a per-project filter; same here.
func (r *TaskLLMUsageRepository) AggregateByRoleModel(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.RoleModelSpend, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT role, model,
		       COALESCE(SUM(cost_usd), 0) AS cost,
		       COUNT(*) AS steps,
		       COALESCE(SUM(prompt_tokens), 0) AS prompt,
		       COALESCE(SUM(completion_tokens), 0) AS completion,
		       COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation,
		       COALESCE(SUM(cache_read_tokens), 0) AS cache_read
		FROM task_llm_usage WHERE 1=1`)
	args := make([]any, 0, 4)
	if projectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, projectID)
	}
	if !since.IsZero() {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	b.WriteString(" GROUP BY role, model ORDER BY cost DESC")
	if limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, limit)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.RoleModelSpend
	for rows.Next() {
		var s persistence.RoleModelSpend
		if err := rows.Scan(&s.Role, &s.Model, &s.CostUSD, &s.StepCount, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AggregateByProject groups spend by project. SQLite-specific
// detail: COUNT(DISTINCT) FILTER (WHERE ...) doesn't exist, so we
// substitute COUNT(DISTINCT CASE WHEN ... THEN task_id END) — same
// semantic, no Postgres-only syntax.
func (r *TaskLLMUsageRepository) AggregateByProject(ctx context.Context, since, until time.Time, limit int) ([]persistence.ProjectSpend, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT project_id,
		       COALESCE(SUM(cost_usd), 0),
		       COUNT(*),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COUNT(DISTINCT CASE WHEN task_id IS NOT NULL THEN task_id END),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0)
		FROM task_llm_usage WHERE 1=1`)
	args := make([]any, 0, 3)
	if !since.IsZero() {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	b.WriteString(" GROUP BY project_id ORDER BY 2 DESC")
	if limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ProjectSpend
	for rows.Next() {
		var s persistence.ProjectSpend
		if err := rows.Scan(&s.ProjectID, &s.CostUSD, &s.StepCount, &s.PromptTokens, &s.CompletionTokens, &s.TaskCount,
			&s.CacheCreationTokens, &s.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AggregateBySource splits spend by the `source` column.
func (r *TaskLLMUsageRepository) AggregateBySource(ctx context.Context, since, until time.Time, projectID string) ([]persistence.SourceSpend, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT COALESCE(source, ''),
		       COALESCE(SUM(cost_usd), 0),
		       COUNT(*),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0)
		FROM task_llm_usage WHERE 1=1`)
	args := make([]any, 0, 3)
	if !since.IsZero() {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	if projectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, projectID)
	}
	b.WriteString(" GROUP BY source ORDER BY 2 DESC")
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.SourceSpend
	for rows.Next() {
		var s persistence.SourceSpend
		if err := rows.Scan(&s.Source, &s.CostUSD, &s.CallCount, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TimeSeriesByDay buckets spend by UTC day. We store recorded_at
// as RFC3339Nano TEXT — `substr(recorded_at, 1, 10)` gives the
// 'YYYY-MM-DD' day prefix; that's the canonical UTC day boundary.
func (r *TaskLLMUsageRepository) TimeSeriesByDay(ctx context.Context, since, until time.Time, projectID string) ([]persistence.DailySpend, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT substr(recorded_at, 1, 10) AS day,
		       COALESCE(SUM(cost_usd), 0),
		       COUNT(*)
		FROM task_llm_usage WHERE 1=1`)
	args := make([]any, 0, 3)
	if !since.IsZero() {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	if projectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, projectID)
	}
	b.WriteString(" GROUP BY day ORDER BY day ASC")
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.DailySpend
	for rows.Next() {
		var (
			d         persistence.DailySpend
			dayString string
		)
		if err := rows.Scan(&dayString, &d.CostUSD, &d.CallCount); err != nil {
			return nil, err
		}
		t, err := time.Parse("2006-01-02", dayString)
		if err != nil {
			return nil, fmt.Errorf("sqlite: parse day bucket %q: %w", dayString, err)
		}
		d.Day = t.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// TopTasks joins task_llm_usage to tasks for the status +
// creation_source columns. LEFT JOIN preserves usage rows for
// pruned tasks. MIN/MAX recorded_at give wall-clock duration
// for the dashboard's "ran for X hours of LLM time" display.
func (r *TaskLLMUsageRepository) TopTasks(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.TaskSpend, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT u.task_id,
		       u.project_id,
		       COALESCE(t.status, ''),
		       COALESCE(t.creation_source, ''),
		       COALESCE(SUM(u.cost_usd), 0),
		       COALESCE(SUM(u.prompt_tokens), 0),
		       COALESCE(SUM(u.completion_tokens), 0),
		       COUNT(*),
		       COALESCE(SUM(u.iterations), 0),
		       MIN(u.recorded_at),
		       MAX(u.recorded_at)
		FROM task_llm_usage u
		LEFT JOIN tasks t ON t.id = u.task_id
		WHERE u.task_id IS NOT NULL`)
	args := make([]any, 0, 3)
	if !since.IsZero() {
		b.WriteString(" AND u.recorded_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND u.recorded_at < ?")
		args = append(args, sqliteTime(until))
	}
	if projectID != "" {
		b.WriteString(" AND u.project_id = ?")
		args = append(args, projectID)
	}
	b.WriteString(" GROUP BY u.task_id, u.project_id, t.status, t.creation_source ORDER BY 5 DESC")
	if limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.TaskSpend
	for rows.Next() {
		var (
			s          persistence.TaskSpend
			taskIDNull sql.NullString
			firstStep  sqlTime
			lastStep   sqlTime
		)
		if err := rows.Scan(&taskIDNull, &s.ProjectID, &s.Status, &s.CreationSource,
			&s.CostUSD, &s.PromptTokens, &s.CompletionTokens, &s.StepCount, &s.Iterations,
			&firstStep, &lastStep); err != nil {
			return nil, err
		}
		if taskIDNull.Valid {
			s.TaskID = taskIDNull.String
		}
		s.FirstStepAt = firstStep.Time
		s.LastStepAt = lastStep.Time
		out = append(out, s)
	}
	return out, rows.Err()
}

// TaskCostBreakdown returns per-step rows for one task in
// execution order.
func (r *TaskLLMUsageRepository) TaskCostBreakdown(ctx context.Context, taskID string) ([]persistence.StepSpend, error) {
	if taskID == "" {
		return nil, fmt.Errorf("taskID is required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT step_id, role, model, prompt_tokens, completion_tokens,
		       iterations, cost_usd, recorded_at, COALESCE(source, '')
		FROM task_llm_usage
		WHERE task_id = ?
		ORDER BY recorded_at ASC, id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.StepSpend
	for rows.Next() {
		var (
			s          persistence.StepSpend
			recordedAt sqlTime
		)
		if err := rows.Scan(&s.StepID, &s.Role, &s.Model, &s.PromptTokens, &s.CompletionTokens,
			&s.Iterations, &s.CostUSD, &recordedAt, &s.Source); err != nil {
			return nil, err
		}
		s.RecordedAt = recordedAt.Time
		out = append(out, s)
	}
	return out, rows.Err()
}
