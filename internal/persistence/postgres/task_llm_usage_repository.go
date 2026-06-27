package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskLLMUsageRepository implements persistence.TaskLLMUsageRepository using PostgreSQL.
type TaskLLMUsageRepository struct {
	db DBTX
}

// NewTaskLLMUsageRepository creates a new repository instance.
func NewTaskLLMUsageRepository(db DBTX) *TaskLLMUsageRepository {
	return &TaskLLMUsageRepository{db: db}
}

// Record inserts one usage row. Source defaults to "workflow_step" when
// the caller leaves it empty, preserving behavior for existing writers
// (executor/artifacts.go) that predate the dispatcher split.
func (r *TaskLLMUsageRepository) Record(ctx context.Context, u *persistence.TaskLLMUsage) error {
	if u == nil {
		return fmt.Errorf("nil usage row")
	}
	recordedAt := u.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
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
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		u.ID, u.ProjectID, nullableString(u.TaskID), nullableString(u.ExecutionID), u.StepID,
		u.Role, u.Model, u.PromptTokens, u.CompletionTokens, u.Iterations,
		u.CostUSD, source, nullableString(u.SessionID), recordedAt,
		u.CacheCreationTokens, u.CacheReadTokens,
	)
	return mapDBError(err)
}

// Upsert is the streaming-friendly variant of Record. Unlike
// Record (which expects a unique ID per call and is used at
// step finalize), Upsert lets the agent stream cumulative
// usage stats per iteration with a stable, deterministic ID
// — the row is created on first call and overwritten by every
// subsequent call with the latest cumulative numbers.
//
// This is the path that gives cancelled tasks an LLM cost
// summary: when the agent's container is force-killed mid-
// step, the most-recent stream payload is already in the DB
// (the per-iteration update). Without streaming, the persist-
// at-step-finalize path leaves cancelled tasks at $0 because
// the finalize never runs.
//
// ID is the caller's responsibility to keep stable across
// updates (e.g. `tu_<task_id>_<step_id>_<role>`). Postgres'
// ON CONFLICT (id) DO UPDATE handles the upsert atomically.
func (r *TaskLLMUsageRepository) Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error {
	if u == nil {
		return fmt.Errorf("nil usage row")
	}
	if u.ID == "" {
		return fmt.Errorf("usage row id is required for Upsert")
	}
	recordedAt := u.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
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
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET
			prompt_tokens         = EXCLUDED.prompt_tokens,
			completion_tokens     = EXCLUDED.completion_tokens,
			iterations            = EXCLUDED.iterations,
			cost_usd              = EXCLUDED.cost_usd,
			model                 = EXCLUDED.model,
			recorded_at           = EXCLUDED.recorded_at,
			cache_creation_tokens = EXCLUDED.cache_creation_tokens,
			cache_read_tokens     = EXCLUDED.cache_read_tokens`,
		u.ID, u.ProjectID, nullableString(u.TaskID), nullableString(u.ExecutionID), u.StepID,
		u.Role, u.Model, u.PromptTokens, u.CompletionTokens, u.Iterations,
		u.CostUSD, source, nullableString(u.SessionID), recordedAt,
		u.CacheCreationTokens, u.CacheReadTokens,
	)
	return mapDBError(err)
}

// nullableString converts an optional *string to a sql.NullString for
// Postgres driver binding. nil pointer and empty string both yield
// NULL — dispatcher rows have no task/execution, and there's no
// legitimate use for a zero-length id anywhere.
func nullableString(s *string) sql.NullString {
	if s == nil || *s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// stringPtrOrNil returns a pointer to s if non-empty, else nil. Used when
// scanning sql.NullString back into the model's *string fields.
func stringPtrOrNil(ns sql.NullString) *string {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	v := ns.String
	return &v
}

// List returns usage rows matching the filter, newest first.
func (r *TaskLLMUsageRepository) List(ctx context.Context, f persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	query := `
		SELECT id, project_id, task_id, execution_id, step_id,
		       role, model, prompt_tokens, completion_tokens, iterations,
		       cost_usd, source, session_id, recorded_at,
		       cache_creation_tokens, cache_read_tokens
		FROM task_llm_usage WHERE 1=1`
	args := make([]any, 0, 10)
	pos := 1

	if f.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", pos)
		args = append(args, *f.ProjectID)
		pos++
	}
	if f.TaskID != nil {
		query += fmt.Sprintf(" AND task_id = $%d", pos)
		args = append(args, *f.TaskID)
		pos++
	}
	if f.ExecutionID != nil {
		query += fmt.Sprintf(" AND execution_id = $%d", pos)
		args = append(args, *f.ExecutionID)
		pos++
	}
	if f.Role != nil {
		query += fmt.Sprintf(" AND role = $%d", pos)
		args = append(args, *f.Role)
		pos++
	}
	if f.Model != nil {
		query += fmt.Sprintf(" AND model = $%d", pos)
		args = append(args, *f.Model)
		pos++
	}
	if f.Source != nil {
		query += fmt.Sprintf(" AND source = $%d", pos)
		args = append(args, *f.Source)
		pos++
	}
	if f.SessionID != nil {
		query += fmt.Sprintf(" AND session_id = $%d", pos)
		args = append(args, *f.SessionID)
		pos++
	}
	if f.Since != nil {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, *f.Since)
		pos++
	}
	if f.Until != nil {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, *f.Until)
		pos++
	}

	query += " ORDER BY recorded_at DESC"
	if f.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", pos)
		args = append(args, f.PageSize)
		pos++
	}
	if f.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", pos)
		args = append(args, f.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TaskLLMUsage
	for rows.Next() {
		var (
			u           persistence.TaskLLMUsage
			taskID      sql.NullString
			executionID sql.NullString
			sessionID   sql.NullString
		)
		if err := rows.Scan(
			&u.ID, &u.ProjectID, &taskID, &executionID, &u.StepID,
			&u.Role, &u.Model, &u.PromptTokens, &u.CompletionTokens, &u.Iterations,
			&u.CostUSD, &u.Source, &sessionID, &u.RecordedAt,
			&u.CacheCreationTokens, &u.CacheReadTokens,
		); err != nil {
			return nil, err
		}
		u.TaskID = stringPtrOrNil(taskID)
		u.ExecutionID = stringPtrOrNil(executionID)
		u.SessionID = stringPtrOrNil(sessionID)
		out = append(out, &u)
	}
	return out, rows.Err()
}

// SumCostByProject returns total cost for a project within an optional time
// window. Zero time values mean unbounded on that side.
func (r *TaskLLMUsageRepository) SumCostByProject(ctx context.Context, projectID string, since, until time.Time) (float64, error) {
	query := "SELECT COALESCE(SUM(cost_usd), 0) FROM task_llm_usage WHERE project_id = $1"
	args := []any{projectID}
	pos := 2
	if !since.IsZero() {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, until)
	}

	var total float64
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&total)
	if err != nil && err != sql.ErrNoRows {
		return 0, mapDBError(err)
	}
	return total, nil
}

// SumCostByAPIKey sums LLM spend across every task created by a
// companion API key. Joins task_llm_usage → tasks on the expression
// indexed by migration 82. Low-frequency (the delegate gate), so the
// JOIN is fine — the partial expression index makes the tasks-side
// lookup an index scan and the existing idx_task_llm_usage_task covers
// the usage side. See finding #2 / mitigation plan §7.2.
func (r *TaskLLMUsageRepository) SumCostByAPIKey(ctx context.Context, apiKeyID string, since, until time.Time) (float64, error) {
	query := `SELECT COALESCE(SUM(u.cost_usd), 0)
		FROM task_llm_usage u
		JOIN tasks t ON t.id = u.task_id
		WHERE t.payload->'companion'->>'api_key_id' = $1`
	args := []any{apiKeyID}
	pos := 2
	if !since.IsZero() {
		query += fmt.Sprintf(" AND u.recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND u.recorded_at < $%d", pos)
		args = append(args, until)
	}

	var total float64
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&total)
	if err != nil && err != sql.ErrNoRows {
		return 0, mapDBError(err)
	}
	return total, nil
}

// MeanCostByWorkflow returns the average per-task LLM cost for a
// (project, workflow) pair within an optional window, plus the
// number of distinct tasks the average covers. Joins
// task_llm_usage → tasks on workflow_id (indexed) and divides total
// spend by the distinct-task count so a workflow with many steps per
// task isn't double-counted. Returns (0, 0, nil) when no prior
// completed tasks exist — the caller treats sampleCount 0 as "no
// estimate available". See LLD-21 § delegate cost_estimate /
// drift-mitigation §8.2.
func (r *TaskLLMUsageRepository) MeanCostByWorkflow(ctx context.Context, projectID, workflowID string, since, until time.Time) (float64, int, error) {
	query := `SELECT COALESCE(SUM(u.cost_usd), 0),
	                 COUNT(DISTINCT u.task_id) FILTER (WHERE u.task_id IS NOT NULL)
		FROM task_llm_usage u
		JOIN tasks t ON t.id = u.task_id
		WHERE t.project_id = $1 AND t.workflow_id = $2`
	args := []any{projectID, workflowID}
	pos := 3
	if !since.IsZero() {
		query += fmt.Sprintf(" AND u.recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND u.recorded_at < $%d", pos)
		args = append(args, until)
	}

	var total float64
	var taskCount int
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&total, &taskCount)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, mapDBError(err)
	}
	if taskCount == 0 {
		return 0, 0, nil
	}
	return total / float64(taskCount), taskCount, nil
}

// SumCost returns cross-project total cost within the window.
func (r *TaskLLMUsageRepository) SumCost(ctx context.Context, since, until time.Time) (float64, error) {
	query := "SELECT COALESCE(SUM(cost_usd), 0) FROM task_llm_usage WHERE 1=1"
	var args []any
	pos := 1
	if !since.IsZero() {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, until)
	}
	var total float64
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&total)
	if err != nil && err != sql.ErrNoRows {
		return 0, mapDBError(err)
	}
	return total, nil
}

// AggregateByProject groups usage rows by project_id within the
// window, ordered by total cost descending. TaskCount uses
// COUNT(DISTINCT task_id) so a project with one runaway task is
// distinguishable from a project with many small tasks.
func (r *TaskLLMUsageRepository) AggregateByProject(ctx context.Context, since, until time.Time, limit int) ([]persistence.ProjectSpend, error) {
	query := `
		SELECT project_id,
		       COALESCE(SUM(cost_usd), 0) AS cost_usd,
		       COUNT(*) AS step_count,
		       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
		       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
		       COUNT(DISTINCT task_id) FILTER (WHERE task_id IS NOT NULL) AS task_count,
		       COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
		       COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM task_llm_usage
		WHERE 1=1`
	var args []any
	pos := 1
	if !since.IsZero() {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, until)
		pos++
	}
	query += ` GROUP BY project_id ORDER BY cost_usd DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", pos)
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
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

// AggregateBySource splits spend by the `source` column. Powers
// the dispatcher-vs-workflow attribution chart on the deep-dive
// dashboard.
func (r *TaskLLMUsageRepository) AggregateBySource(ctx context.Context, since, until time.Time, projectID string) ([]persistence.SourceSpend, error) {
	query := `
		SELECT COALESCE(source, '') AS source,
		       COALESCE(SUM(cost_usd), 0) AS cost_usd,
		       COUNT(*) AS call_count,
		       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
		       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
		       COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
		       COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM task_llm_usage
		WHERE 1=1`
	var args []any
	pos := 1
	if !since.IsZero() {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, until)
		pos++
	}
	if projectID != "" {
		query += fmt.Sprintf(" AND project_id = $%d", pos)
		args = append(args, projectID)
	}
	query += ` GROUP BY source ORDER BY cost_usd DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
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

// TimeSeriesByDay returns daily spend buckets. Truncates
// recorded_at to the day in UTC; operators reading on a non-UTC
// timezone may see day boundaries shifted by their local offset
// (a known limitation of using UTC as the bucket axis — alternative
// would require carrying the project's timezone through, which
// budget.Check already does for its own day math).
func (r *TaskLLMUsageRepository) TimeSeriesByDay(ctx context.Context, since, until time.Time, projectID string) ([]persistence.DailySpend, error) {
	query := `
		SELECT date_trunc('day', recorded_at) AS day,
		       COALESCE(SUM(cost_usd), 0) AS cost_usd,
		       COUNT(*) AS call_count
		FROM task_llm_usage
		WHERE 1=1`
	var args []any
	pos := 1
	if !since.IsZero() {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, until)
		pos++
	}
	if projectID != "" {
		query += fmt.Sprintf(" AND project_id = $%d", pos)
		args = append(args, projectID)
	}
	query += ` GROUP BY day ORDER BY day ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.DailySpend
	for rows.Next() {
		var d persistence.DailySpend
		if err := rows.Scan(&d.Day, &d.CostUSD, &d.CallCount); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TopTasks returns the N most-expensive tasks in the window, with
// joined task metadata (status, creation_source). Left-outer join
// so usage rows whose task row has been pruned by retention still
// surface their spend rather than silently disappearing.
func (r *TaskLLMUsageRepository) TopTasks(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.TaskSpend, error) {
	query := `
		SELECT u.task_id,
		       u.project_id,
		       COALESCE(t.status::text, '') AS status,
		       COALESCE(t.creation_source::text, '') AS creation_source,
		       COALESCE(SUM(u.cost_usd), 0) AS cost_usd,
		       COALESCE(SUM(u.prompt_tokens), 0) AS prompt_tokens,
		       COALESCE(SUM(u.completion_tokens), 0) AS completion_tokens,
		       COUNT(*) AS step_count,
		       COALESCE(SUM(u.iterations), 0) AS iterations,
		       MIN(u.recorded_at) AS first_step_at,
		       MAX(u.recorded_at) AS last_step_at
		FROM task_llm_usage u
		LEFT JOIN tasks t ON t.id = u.task_id
		WHERE u.task_id IS NOT NULL`
	var args []any
	pos := 1
	if !since.IsZero() {
		query += fmt.Sprintf(" AND u.recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND u.recorded_at < $%d", pos)
		args = append(args, until)
		pos++
	}
	if projectID != "" {
		query += fmt.Sprintf(" AND u.project_id = $%d", pos)
		args = append(args, projectID)
		pos++
	}
	query += ` GROUP BY u.task_id, u.project_id, t.status, t.creation_source
		ORDER BY cost_usd DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", pos)
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.TaskSpend
	for rows.Next() {
		var (
			s          persistence.TaskSpend
			taskIDNull sql.NullString
		)
		if err := rows.Scan(&taskIDNull, &s.ProjectID, &s.Status, &s.CreationSource,
			&s.CostUSD, &s.PromptTokens, &s.CompletionTokens, &s.StepCount, &s.Iterations,
			&s.FirstStepAt, &s.LastStepAt); err != nil {
			return nil, err
		}
		if taskIDNull.Valid {
			s.TaskID = taskIDNull.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TaskCostBreakdown returns per-step rows for one task, in
// execution order. Each row is one step (workflow_step source) or
// one dispatcher round-trip; the source column distinguishes them
// so the UI labels rows correctly.
func (r *TaskLLMUsageRepository) TaskCostBreakdown(ctx context.Context, taskID string) ([]persistence.StepSpend, error) {
	if taskID == "" {
		return nil, fmt.Errorf("taskID is required")
	}
	const query = `
		SELECT step_id, role, model, prompt_tokens, completion_tokens,
		       iterations, cost_usd, recorded_at, COALESCE(source, '')
		FROM task_llm_usage
		WHERE task_id = $1
		ORDER BY recorded_at ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, query, taskID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.StepSpend
	for rows.Next() {
		var s persistence.StepSpend
		if err := rows.Scan(&s.StepID, &s.Role, &s.Model, &s.PromptTokens, &s.CompletionTokens,
			&s.Iterations, &s.CostUSD, &s.RecordedAt, &s.Source); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AggregateByRoleModel groups usage rows by (role, model) within the
// window and returns the top `limit` combos by total cost. Limit ≤ 0
// returns everything.
func (r *TaskLLMUsageRepository) AggregateByRoleModel(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.RoleModelSpend, error) {
	query := `
		SELECT role, model,
		       COALESCE(SUM(cost_usd), 0) AS cost_usd,
		       COUNT(*) AS step_count,
		       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
		       COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
		       COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
		       COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens
		FROM task_llm_usage
		WHERE 1=1`
	var args []any
	pos := 1
	if !since.IsZero() {
		query += fmt.Sprintf(" AND recorded_at >= $%d", pos)
		args = append(args, since)
		pos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND recorded_at < $%d", pos)
		args = append(args, until)
		pos++
	}
	if projectID != "" {
		query += fmt.Sprintf(" AND project_id = $%d", pos)
		args = append(args, projectID)
		pos++
	}
	query += ` GROUP BY role, model ORDER BY cost_usd DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", pos)
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
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
