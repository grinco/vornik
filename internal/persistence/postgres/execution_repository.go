package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// ExecutionRepository provides PostgreSQL-backed execution persistence.
type ExecutionRepository struct {
	db DBTX
}

// NewExecutionRepository creates a new execution repository.
func NewExecutionRepository(db DBTX) *ExecutionRepository {
	return &ExecutionRepository{db: db}
}

// Create inserts a new execution record.
func (r *ExecutionRepository) Create(ctx context.Context, execution *persistence.Execution) error {
	if execution == nil {
		return fmt.Errorf("execution is nil")
	}
	now := time.Now().UTC()
	if execution.CreatedAt.IsZero() {
		execution.CreatedAt = now
	}
	if execution.UpdatedAt.IsZero() {
		execution.UpdatedAt = execution.CreatedAt
	}
	if execution.Status == "" {
		execution.Status = persistence.ExecutionStatusPending
	}
	if execution.WorkflowRevision == "" {
		execution.WorkflowRevision = "v1"
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO executions (
			id, task_id, project_id, workflow_id, workflow_revision,
			status, current_step_id, completed_steps, state_snapshot,
			result, error_message, error_code, started_at, completed_at,
			created_at, updated_at,
			parent_execution_id, forked_from_step_id, forked_prompt_override
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13, $14,
			$15, $16,
			$17, $18, $19
		)
	`,
		execution.ID, execution.TaskID, execution.ProjectID, execution.WorkflowID, execution.WorkflowRevision,
		execution.Status, execution.CurrentStepID, pq.Array(execution.CompletedSteps), jsonbValue(execution.StateSnapshot),
		jsonbValue(execution.Result), execution.ErrorMessage, execution.ErrorCode, execution.StartedAt, execution.CompletedAt,
		execution.CreatedAt, execution.UpdatedAt,
		execution.ParentExecutionID, execution.ForkedFromStepID, execution.ForkedPromptOverride,
	)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// Get retrieves an execution by ID.
func (r *ExecutionRepository) Get(ctx context.Context, id string) (*persistence.Execution, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, project_id, workflow_id, workflow_revision,
		       status, current_step_id, completed_steps, state_snapshot,
		       result, error_message, error_code, started_at, completed_at,
		       created_at, updated_at,
		       parent_execution_id, forked_from_step_id, forked_prompt_override
		FROM executions
		WHERE id = $1
	`, id)
	return scanExecution(row)
}

// GetByTaskID retrieves the latest execution for a task.
func (r *ExecutionRepository) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, project_id, workflow_id, workflow_revision,
		       status, current_step_id, completed_steps, state_snapshot,
		       result, error_message, error_code, started_at, completed_at,
		       created_at, updated_at,
		       parent_execution_id, forked_from_step_id, forked_prompt_override
		FROM executions
		WHERE task_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, taskID)
	return scanExecution(row)
}

// GetByTaskIDs is the batch sibling of GetByTaskID. Single round-trip
// against the (task_id) index, DISTINCT ON to pick the latest
// execution per task. Autonomy replaces its N+1 GetByTaskID loop with
// one call to this method.
//
// Tasks with no execution row are simply absent from the returned
// map — callers see the missing key and treat it as "no execution"
// rather than a zero-valued Execution.
func (r *ExecutionRepository) GetByTaskIDs(ctx context.Context, taskIDs []string) (map[string]*persistence.Execution, error) {
	if len(taskIDs) == 0 {
		return map[string]*persistence.Execution{}, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (task_id)
		       id, task_id, project_id, workflow_id, workflow_revision,
		       status, current_step_id, completed_steps, state_snapshot,
		       result, error_message, error_code, started_at, completed_at,
		       created_at, updated_at,
		       parent_execution_id, forked_from_step_id, forked_prompt_override
		FROM executions
		WHERE task_id = ANY($1)
		ORDER BY task_id, created_at DESC
	`, pq.Array(taskIDs))
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]*persistence.Execution, len(taskIDs))
	for rows.Next() {
		exec, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		if exec != nil {
			out[exec.TaskID] = exec
		}
	}
	return out, rows.Err()
}

// Update modifies an execution.
func (r *ExecutionRepository) Update(ctx context.Context, execution *persistence.Execution) error {
	if execution == nil {
		return fmt.Errorf("execution is nil")
	}
	execution.UpdatedAt = time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET project_id = $2,
		    workflow_id = $3,
		    workflow_revision = $4,
		    status = $5,
		    current_step_id = $6,
		    completed_steps = $7,
		    state_snapshot = $8,
		    result = $9,
		    error_message = $10,
		    error_code = $11,
		    -- started_at is stamped once by UpdateStatus(RUNNING) and is
		    -- never legitimately reset to NULL. COALESCE prevents a
		    -- full-row Update carrying a nil in-memory StartedAt (e.g. the
		    -- executor persisting the resolved workflow_id right after the
		    -- RUNNING transition) from clobbering the stamp — which left
		    -- every row with a blank Started/Duration in /ui/executions.
		    started_at = COALESCE($12, started_at),
		    completed_at = $13,
		    updated_at = $14
		WHERE id = $1
	`,
		execution.ID, execution.ProjectID, execution.WorkflowID, execution.WorkflowRevision, execution.Status,
		execution.CurrentStepID, pq.Array(execution.CompletedSteps), jsonbValue(execution.StateSnapshot), jsonbValue(execution.Result),
		execution.ErrorMessage, execution.ErrorCode, execution.StartedAt, execution.CompletedAt, execution.UpdatedAt,
	)
	if err != nil {
		return mapDBError(err)
	}
	return nil
}

// UpdateStatus changes the execution status.
func (r *ExecutionRepository) UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = $2::execution_status,
		    started_at = CASE WHEN $2::execution_status = 'RUNNING'::execution_status AND started_at IS NULL THEN NOW() ELSE started_at END,
		    completed_at = CASE WHEN $2::execution_status IN ('COMPLETED'::execution_status, 'FAILED'::execution_status, 'CANCELLED'::execution_status) THEN NOW() ELSE completed_at END,
		    updated_at = NOW()
		WHERE id = $1
	`, id, status)
	return mapDBError(err)
}

// SaveStateSnapshot stores resumable execution state.
func (r *ExecutionRepository) SaveStateSnapshot(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET state_snapshot = $2,
		    current_step_id = NULLIF($3, ''),
		    completed_steps = $4,
		    updated_at = NOW()
		WHERE id = $1
	`, id, jsonbValue(snapshot), currentStepID, pq.Array(completedSteps))
	return mapDBError(err)
}

// SetWorkflowSnapshot persists the workflow body captured at
// execution start. Idempotent: passing an empty/nil payload is a
// no-op (no DB write) so the executor's "set if absent" semantics
// can be expressed as a single call without checking first.
func (r *ExecutionRepository) SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error {
	if len(snapshot) == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET workflow_snapshot = $2,
		    updated_at = NOW()
		WHERE id = $1
	`, id, snapshot)
	return mapDBError(err)
}

// GetWorkflowSnapshot returns the bytes stored by SetWorkflowSnapshot,
// or nil when no snapshot was captured (e.g. legacy execution rows
// from before the column existed).
func (r *ExecutionRepository) GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error) {
	var snapshot []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT workflow_snapshot
		FROM executions
		WHERE id = $1
	`, id).Scan(&snapshot)
	if err != nil {
		return nil, mapDBError(err)
	}
	return snapshot, nil
}

// RecordCompletion marks an execution completed.
func (r *ExecutionRepository) RecordCompletion(ctx context.Context, id string, result []byte) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'COMPLETED',
		    result = $2,
		    completed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
	`, id, jsonbValue(result))
	return mapDBError(err)
}

// RecordFailure marks an execution failed.
func (r *ExecutionRepository) RecordFailure(ctx context.Context, id, errorMessage, errorCode string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'FAILED',
		    error_message = NULLIF($2, ''),
		    error_code = NULLIF($3, ''),
		    completed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
	`, id, errorMessage, errorCode)
	return mapDBError(err)
}

// SupersedeNonTerminalForTask closes orphan executions whose
// parent task has reached a terminal status. See the interface
// doc for the full motivation. Rows are marked CANCELLED rather
// than introducing a new SUPERSEDED enum value (which would
// require a postgres type ALTER + a wider audit-surface change);
// the error_code marker preserves the distinction for audits and
// metrics.
func (r *ExecutionRepository) SupersedeNonTerminalForTask(ctx context.Context, taskID string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'CANCELLED',
		    error_code = 'superseded_by_terminal_task',
		    error_message = COALESCE(error_message, 'execution superseded when parent task reached terminal status'),
		    completed_at = COALESCE(completed_at, NOW()),
		    updated_at = NOW()
		WHERE task_id = $1
		  AND status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED')
	`, taskID)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// SupersedeOrphanPausedExecutions finalizes every PAUSED execution whose
// parent task is already terminal — the global backstop for orphans that
// the per-task cascade missed (CLOSED / odd cancel paths) plus historical
// rows. See the interface doc. Marked CANCELLED with a distinct error_code
// so audits/metrics can tell sweep-finalized orphans from the per-task
// cascade and from genuine cancellations.
func (r *ExecutionRepository) SupersedeOrphanPausedExecutions(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE executions e
		SET status = 'CANCELLED',
		    error_code = 'superseded_orphan_paused',
		    error_message = COALESCE(e.error_message, 'orphan PAUSED execution finalized: parent task already terminal'),
		    completed_at = COALESCE(e.completed_at, NOW()),
		    updated_at = NOW()
		FROM tasks t
		WHERE e.task_id = t.id
		  AND e.status = 'PAUSED'
		  AND t.status IN ('COMPLETED', 'FAILED', 'CANCELLED', 'CLOSED')
	`)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// CountByStatus counts executions for a project.
func (r *ExecutionRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.ExecutionStatus]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM executions
		WHERE ($1 = '' OR project_id = $1)
		GROUP BY status
	`, projectID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[persistence.ExecutionStatus]int64)
	for rows.Next() {
		var status persistence.ExecutionStatus
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

// GetRoleQuality aggregates per-role output quality stats for a project
// over a rolling time window. It uses execution_step_outcomes rather than
// tool_audit_log so roles that produce valid output without calling tools
// still count as runs.
func (r *ExecutionRepository) GetRoleQuality(
	ctx context.Context,
	projectID string,
	since time.Duration,
) (map[string]*persistence.RoleQuality, error) {
	if projectID == "" {
		return nil, fmt.Errorf("projectID is required")
	}
	if since <= 0 {
		since = 30 * 24 * time.Hour
	}
	// Terminal, non-cancelled outcome rows are the quality denominator.
	// `ok` is success; every other terminal outcome is a failed usable-output
	// attempt. COALESCE(finalized_at, recorded_at) lets a row count in the
	// window where it became terminal rather than only where it was first
	// recorded as pending_validation.
	const query = `
WITH role_outcomes AS (
    SELECT
        role AS role_name,
        outcome,
        duration_ms
    FROM execution_step_outcomes
    WHERE project_id = $1
      AND COALESCE(finalized_at, recorded_at) > NOW() - ($2::bigint * INTERVAL '1 second')
      AND role <> ''
      AND outcome NOT IN ('pending_validation', 'cancelled')
)
SELECT
    role_name,
    COUNT(*)                                      AS total,
    COUNT(*) FILTER (WHERE outcome = 'ok')        AS completed,
    COUNT(*) FILTER (WHERE outcome <> 'ok')       AS failed,
    COALESCE(
        AVG(duration_ms::double precision) FILTER (WHERE outcome = 'ok' AND duration_ms IS NOT NULL),
        0
    ) / 1000.0 AS avg_duration_sec
FROM role_outcomes
GROUP BY role_name
ORDER BY role_name;`

	rows, err := r.db.QueryContext(ctx, query, projectID, int64(since.Seconds()))
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]*persistence.RoleQuality)
	for rows.Next() {
		q := &persistence.RoleQuality{}
		if err := rows.Scan(&q.RoleName, &q.Executions, &q.Completed, &q.Failed, &q.AvgDurationSec); err != nil {
			return nil, err
		}
		if q.Executions > 0 {
			q.SuccessRatePct = float64(q.Completed) * 100.0 / float64(q.Executions)
			// Round to one decimal.
			q.SuccessRatePct = float64(int(q.SuccessRatePct*10+0.5)) / 10.0
		}
		out[q.RoleName] = q
	}
	return out, rows.Err()
}

func scanExecution(scanner interface {
	Scan(dest ...any) error
}) (*persistence.Execution, error) {
	var (
		execution            persistence.Execution
		currentStepID        sql.NullString
		errorMessage         sql.NullString
		errorCode            sql.NullString
		startedAt            sql.NullTime
		completedAt          sql.NullTime
		completedSteps       pq.StringArray
		parentExecutionID    sql.NullString
		forkedFromStepID     sql.NullString
		forkedPromptOverride sql.NullString
	)

	err := scanner.Scan(
		&execution.ID, &execution.TaskID, &execution.ProjectID, &execution.WorkflowID, &execution.WorkflowRevision,
		&execution.Status, &currentStepID, &completedSteps, &execution.StateSnapshot,
		&execution.Result, &errorMessage, &errorCode, &startedAt, &completedAt,
		&execution.CreatedAt, &execution.UpdatedAt,
		&parentExecutionID, &forkedFromStepID, &forkedPromptOverride,
	)
	if err != nil {
		return nil, mapDBError(err)
	}

	if currentStepID.Valid {
		execution.CurrentStepID = &currentStepID.String
	}
	if errorMessage.Valid {
		execution.ErrorMessage = &errorMessage.String
	}
	if errorCode.Valid {
		execution.ErrorCode = &errorCode.String
	}
	if startedAt.Valid {
		execution.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		execution.CompletedAt = &completedAt.Time
	}
	execution.CompletedSteps = []string(completedSteps)
	if parentExecutionID.Valid {
		execution.ParentExecutionID = &parentExecutionID.String
	}
	if forkedFromStepID.Valid {
		execution.ForkedFromStepID = &forkedFromStepID.String
	}
	if forkedPromptOverride.Valid {
		execution.ForkedPromptOverride = &forkedPromptOverride.String
	}

	return &execution, nil
}

func jsonbValue(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// List retrieves executions based on filter criteria.
func (r *ExecutionRepository) List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	query := `
		SELECT id, task_id, project_id, workflow_id, workflow_revision,
		       status, current_step_id, completed_steps, state_snapshot,
		       result, error_message, error_code, started_at, completed_at,
		       created_at, updated_at,
		       parent_execution_id, forked_from_step_id, forked_prompt_override
		FROM executions
		WHERE 1=1
	`
	args := []interface{}{}
	argNum := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argNum)
		args = append(args, *filter.ProjectID)
		argNum++
	}
	if filter.TaskID != nil {
		query += fmt.Sprintf(" AND task_id = $%d", argNum)
		args = append(args, *filter.TaskID)
		argNum++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d::execution_status", argNum)
		args = append(args, *filter.Status)
		argNum++
	}

	query += " ORDER BY created_at DESC"

	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argNum)
		args = append(args, filter.PageSize)
		argNum++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argNum)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var executions []*persistence.Execution
	for rows.Next() {
		exec, err := scanExecutionRow(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, exec)
	}
	return executions, rows.Err()
}

// Count mirrors List's WHERE clauses without LIMIT/OFFSET so the
// API can return a real Total for paginated responses.
func (r *ExecutionRepository) Count(ctx context.Context, filter persistence.ExecutionFilter) (int64, error) {
	query := `SELECT COUNT(*) FROM executions WHERE 1=1`
	args := []interface{}{}
	argNum := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argNum)
		args = append(args, *filter.ProjectID)
		argNum++
	}
	if filter.TaskID != nil {
		query += fmt.Sprintf(" AND task_id = $%d", argNum)
		args = append(args, *filter.TaskID)
		argNum++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d::execution_status", argNum)
		args = append(args, *filter.Status)
	}

	var total int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, mapDBError(err)
	}
	return total, nil
}

func scanExecutionRow(scanner interface {
	Scan(dest ...any) error
}) (*persistence.Execution, error) {
	return scanExecution(scanner)
}
