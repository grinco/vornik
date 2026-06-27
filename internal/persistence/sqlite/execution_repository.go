package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionRepository is the SQLite-backed
// persistence.ExecutionRepository. The Postgres sibling uses
// pq.Array + DISTINCT ON + interval arithmetic for the role-quality
// aggregator; SQLite substitutes:
//
//   - pq.Array(...) → sqliteStringArray (JSON-encoded TEXT).
//   - DISTINCT ON (task_id) → window-style ROW_NUMBER() partitioned
//     by task_id (SQLite 3.25+ supports window functions natively).
//   - execution_status casts → omitted; status is TEXT here.
//   - GetRoleQuality is intentionally unimplemented on SQLite: it
//     joins to execution_step_outcomes which the phase-2 starter
//     schema doesn't include. Returns ErrUnimplemented so a caller
//     can detect the gap rather than silently receiving empty data.
type ExecutionRepository struct {
	db DBTX
}

// ErrUnimplemented signals that a method has no SQLite implementation
// in this phase. Callers that mark methods with this are explicitly
// documented in the per-method comment.
var ErrUnimplemented = errors.New("sqlite: method not implemented in phase-2 starter set")

// NewExecutionRepository constructs an ExecutionRepository over db.
func NewExecutionRepository(db DBTX) *ExecutionRepository {
	return &ExecutionRepository{db: db}
}

// Create inserts one execution row.
func (r *ExecutionRepository) Create(ctx context.Context, e *persistence.Execution) error {
	if e == nil {
		return fmt.Errorf("execution is nil")
	}
	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = e.CreatedAt
	}
	if e.Status == "" {
		e.Status = persistence.ExecutionStatusPending
	}
	if e.WorkflowRevision == "" {
		e.WorkflowRevision = "v1"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO executions (
			id, task_id, project_id, workflow_id, workflow_revision,
			workflow_snapshot, status, current_step_id, completed_steps,
			state_snapshot, result, error_message, error_code,
			started_at, completed_at, created_at, updated_at,
			parent_execution_id, forked_from_step_id, forked_prompt_override
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TaskID, e.ProjectID, e.WorkflowID, e.WorkflowRevision,
		nullableBlob(e.WorkflowSnapshot), string(e.Status), e.CurrentStepID,
		sqliteStringArray(e.CompletedSteps),
		nullableBlob(e.StateSnapshot), nullableBlob(e.Result),
		e.ErrorMessage, e.ErrorCode,
		sqliteTimePtr(e.StartedAt), sqliteTimePtr(e.CompletedAt),
		sqliteTime(e.CreatedAt), sqliteTime(e.UpdatedAt),
		e.ParentExecutionID, e.ForkedFromStepID, e.ForkedPromptOverride,
	)
	return err
}

// Get returns the execution row by ID; ErrNotFound when missing.
func (r *ExecutionRepository) Get(ctx context.Context, id string) (*persistence.Execution, error) {
	row := r.db.QueryRowContext(ctx, executionSelectColumns+" WHERE id = ?", id)
	return scanSqliteExecution(row)
}

// GetByTaskID returns the most recent execution for a task.
func (r *ExecutionRepository) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	row := r.db.QueryRowContext(ctx, executionSelectColumns+`
		WHERE task_id = ?
		ORDER BY created_at DESC
		LIMIT 1`, taskID)
	return scanSqliteExecution(row)
}

// GetByTaskIDs is the batch sibling, returning the latest execution
// per task_id. Uses ROW_NUMBER() partitioned by task_id (SQLite 3.25+
// supports window functions; modernc.org/sqlite ships 3.40+).
func (r *ExecutionRepository) GetByTaskIDs(ctx context.Context, taskIDs []string) (map[string]*persistence.Execution, error) {
	if len(taskIDs) == 0 {
		return map[string]*persistence.Execution{}, nil
	}
	placeholders := strings.Repeat("?,", len(taskIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		args[i] = id
	}
	q := `
		WITH ranked AS (
			SELECT
				id, task_id, project_id, workflow_id, workflow_revision,
				workflow_snapshot, status, current_step_id, completed_steps,
				state_snapshot, result, error_message, error_code,
				started_at, completed_at, created_at, updated_at,
				parent_execution_id, forked_from_step_id, forked_prompt_override,
				ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY created_at DESC) AS rn
			FROM executions
			WHERE task_id IN (` + placeholders + `)
		)
		SELECT
			id, task_id, project_id, workflow_id, workflow_revision,
			workflow_snapshot, status, current_step_id, completed_steps,
			state_snapshot, result, error_message, error_code,
			started_at, completed_at, created_at, updated_at,
			parent_execution_id, forked_from_step_id, forked_prompt_override
		FROM ranked WHERE rn = 1`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]*persistence.Execution, len(taskIDs))
	for rows.Next() {
		e, err := scanSqliteExecution(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out[e.TaskID] = e
		}
	}
	return out, rows.Err()
}

// Update rewrites an execution row by ID.
func (r *ExecutionRepository) Update(ctx context.Context, e *persistence.Execution) error {
	if e == nil {
		return fmt.Errorf("execution is nil")
	}
	e.UpdatedAt = time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET project_id = ?,
		    workflow_id = ?,
		    workflow_revision = ?,
		    status = ?,
		    current_step_id = ?,
		    completed_steps = ?,
		    state_snapshot = ?,
		    result = ?,
		    error_message = ?,
		    error_code = ?,
		    -- started_at is stamped once by UpdateStatus(RUNNING) and is
		    -- never legitimately reset to NULL; COALESCE keeps a full-row
		    -- Update with a nil in-memory StartedAt from clobbering the
		    -- stamp (parity with the postgres repo). See repotest
		    -- Update_preserves_started_at_stamped_by_UpdateStatus.
		    started_at = COALESCE(?, started_at),
		    completed_at = ?,
		    updated_at = ?
		WHERE id = ?`,
		e.ProjectID, e.WorkflowID, e.WorkflowRevision, string(e.Status),
		e.CurrentStepID, sqliteStringArray(e.CompletedSteps),
		nullableBlob(e.StateSnapshot), nullableBlob(e.Result),
		e.ErrorMessage, e.ErrorCode,
		sqliteTimePtr(e.StartedAt), sqliteTimePtr(e.CompletedAt),
		sqliteTime(e.UpdatedAt),
		e.ID,
	)
	return err
}

// UpdateStatus flips status + bookkeeping columns.
func (r *ExecutionRepository) UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error {
	now := time.Now().UTC()
	// Set started_at when transitioning to RUNNING for the first
	// time, completed_at when entering a terminal state. Postgres
	// uses CASE WHEN ... ; SQLite supports the same syntax, so this
	// is a direct port.
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = ?,
		    started_at = CASE WHEN ? = 'RUNNING' AND started_at IS NULL THEN ? ELSE started_at END,
		    completed_at = CASE WHEN ? IN ('COMPLETED','FAILED','CANCELLED') THEN ? ELSE completed_at END,
		    updated_at = ?
		WHERE id = ?`,
		string(status), string(status), sqliteTime(now),
		string(status), sqliteTime(now),
		sqliteTime(now), id,
	)
	return err
}

// SaveStateSnapshot updates the state-snapshot + current-step trio.
func (r *ExecutionRepository) SaveStateSnapshot(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error {
	var current any = currentStepID
	if currentStepID == "" {
		current = nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET state_snapshot = ?,
		    current_step_id = ?,
		    completed_steps = ?,
		    updated_at = ?
		WHERE id = ?`,
		nullableBlob(snapshot), current, sqliteStringArray(completedSteps),
		sqliteTime(time.Now().UTC()), id,
	)
	return err
}

// SetWorkflowSnapshot pins the workflow body captured at start.
// No-op when snapshot is empty (matches Postgres "set if absent"
// semantics).
func (r *ExecutionRepository) SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error {
	if len(snapshot) == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET workflow_snapshot = ?, updated_at = ?
		WHERE id = ?`,
		snapshot, sqliteTime(time.Now().UTC()), id,
	)
	return err
}

// GetWorkflowSnapshot returns the captured workflow body, nil when
// none was stored.
func (r *ExecutionRepository) GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error) {
	var snap []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT workflow_snapshot FROM executions WHERE id = ?`, id).Scan(&snap)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	return snap, nil
}

// RecordCompletion marks an execution COMPLETED + stores its result.
func (r *ExecutionRepository) RecordCompletion(ctx context.Context, id string, result []byte) error {
	now := sqliteTime(time.Now().UTC())
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'COMPLETED',
		    result = ?,
		    completed_at = ?,
		    updated_at = ?
		WHERE id = ?`,
		nullableBlob(result), now, now, id,
	)
	return err
}

// SupersedeNonTerminalForTask closes orphan executions whose
// parent task has reached a terminal status. See the interface
// doc for the full motivation.
func (r *ExecutionRepository) SupersedeNonTerminalForTask(ctx context.Context, taskID string) (int64, error) {
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'CANCELLED',
		    error_code = 'superseded_by_terminal_task',
		    error_message = COALESCE(error_message, 'execution superseded when parent task reached terminal status'),
		    completed_at = COALESCE(completed_at, ?),
		    updated_at = ?
		WHERE task_id = ?
		  AND status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED')`,
		now, now, taskID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// SupersedeOrphanPausedExecutions finalizes every PAUSED execution whose
// parent task is already terminal — the global backstop. See the interface
// doc. (sqlite uses an IN-subquery; UPDATE...FROM parity isn't needed.)
func (r *ExecutionRepository) SupersedeOrphanPausedExecutions(ctx context.Context) (int64, error) {
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'CANCELLED',
		    error_code = 'superseded_orphan_paused',
		    error_message = COALESCE(error_message, 'orphan PAUSED execution finalized: parent task already terminal'),
		    completed_at = COALESCE(completed_at, ?),
		    updated_at = ?
		WHERE status = 'PAUSED'
		  AND task_id IN (SELECT id FROM tasks WHERE status IN ('COMPLETED', 'FAILED', 'CANCELLED', 'CLOSED'))`,
		now, now,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RecordFailure marks an execution FAILED + records error info.
func (r *ExecutionRepository) RecordFailure(ctx context.Context, id, errorMessage, errorCode string) error {
	now := sqliteTime(time.Now().UTC())
	var msg, code any
	if errorMessage != "" {
		msg = errorMessage
	}
	if errorCode != "" {
		code = errorCode
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = 'FAILED',
		    error_message = ?,
		    error_code = ?,
		    completed_at = ?,
		    updated_at = ?
		WHERE id = ?`,
		msg, code, now, now, id,
	)
	return err
}

// CountByStatus groups executions by status for a project (empty
// project = all projects).
func (r *ExecutionRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.ExecutionStatus]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM executions
		WHERE (? = '' OR project_id = ?)
		GROUP BY status`, projectID, projectID)
	if err != nil {
		return nil, err
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

// List returns executions matching filter, newest-first.
func (r *ExecutionRepository) List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	var b strings.Builder
	b.WriteString(executionSelectColumns + " WHERE 1=1")
	args := make([]any, 0, 4)
	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.TaskID != nil {
		b.WriteString(" AND task_id = ?")
		args = append(args, *filter.TaskID)
	}
	if filter.Status != nil {
		b.WriteString(" AND status = ?")
		args = append(args, string(*filter.Status))
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
	var out []*persistence.Execution
	for rows.Next() {
		e, err := scanSqliteExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Count mirrors List's WHERE clauses without LIMIT/OFFSET.
func (r *ExecutionRepository) Count(ctx context.Context, filter persistence.ExecutionFilter) (int64, error) {
	var b strings.Builder
	b.WriteString("SELECT COUNT(*) FROM executions WHERE 1=1")
	args := make([]any, 0, 4)
	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.TaskID != nil {
		b.WriteString(" AND task_id = ?")
		args = append(args, *filter.TaskID)
	}
	if filter.Status != nil {
		b.WriteString(" AND status = ?")
		args = append(args, string(*filter.Status))
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// GetRoleQuality aggregates per-role output quality stats over a
// rolling window. SQLite-specific: replaces Postgres's
// `COUNT(*) FILTER (WHERE ...)` with `SUM(CASE WHEN ... THEN 1 ELSE 0 END)`,
// and the interval arithmetic with a Go-side `time.Now().UTC().Add(-since)`
// passed as a single timestamp parameter.
func (r *ExecutionRepository) GetRoleQuality(ctx context.Context, projectID string, since time.Duration) (map[string]*persistence.RoleQuality, error) {
	if projectID == "" {
		return nil, fmt.Errorf("projectID is required")
	}
	if since <= 0 {
		since = 30 * 24 * time.Hour
	}
	cutoff := sqliteTime(time.Now().UTC().Add(-since))
	rows, err := r.db.QueryContext(ctx, `
		WITH role_outcomes AS (
			SELECT role AS role_name, outcome, duration_ms
			FROM execution_step_outcomes
			WHERE project_id = ?
			  AND COALESCE(finalized_at, recorded_at) > ?
			  AND role <> ''
			  AND outcome NOT IN ('pending_validation', 'cancelled')
		)
		SELECT
		    role_name,
		    COUNT(*)                                          AS total,
		    SUM(CASE WHEN outcome = 'ok' THEN 1 ELSE 0 END)   AS completed,
		    SUM(CASE WHEN outcome <> 'ok' THEN 1 ELSE 0 END)  AS failed,
		    COALESCE(
		        AVG(CASE WHEN outcome = 'ok' AND duration_ms IS NOT NULL THEN duration_ms END),
		        0
		    ) / 1000.0 AS avg_duration_sec
		FROM role_outcomes
		GROUP BY role_name
		ORDER BY role_name`,
		projectID, cutoff)
	if err != nil {
		return nil, err
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
			q.SuccessRatePct = float64(int(q.SuccessRatePct*10+0.5)) / 10.0
		}
		out[q.RoleName] = q
	}
	return out, rows.Err()
}

const executionSelectColumns = `
SELECT id, task_id, project_id, workflow_id, workflow_revision,
       workflow_snapshot, status, current_step_id, completed_steps,
       state_snapshot, result, error_message, error_code,
       started_at, completed_at, created_at, updated_at,
       parent_execution_id, forked_from_step_id, forked_prompt_override
FROM executions`

func scanSqliteExecution(scanner interface{ Scan(dest ...any) error }) (*persistence.Execution, error) {
	var (
		e                    persistence.Execution
		snapshot             sql.NullString // workflow_snapshot stored as BLOB but Scan tolerates text too
		currentStepID        sql.NullString
		completed            sqliteStringArray
		stateSnap            sql.NullString
		result               sql.NullString
		errMsg               sql.NullString
		errCode              sql.NullString
		startedAt            sqlNullTime
		completedAt          sqlNullTime
		createdAt            sqlTime
		updatedAt            sqlTime
		parentExecutionID    sql.NullString
		forkedFromStepID     sql.NullString
		forkedPromptOverride sql.NullString
	)
	err := scanner.Scan(
		&e.ID, &e.TaskID, &e.ProjectID, &e.WorkflowID, &e.WorkflowRevision,
		&snapshot, &e.Status, &currentStepID, &completed,
		&stateSnap, &result, &errMsg, &errCode,
		&startedAt, &completedAt, &createdAt, &updatedAt,
		&parentExecutionID, &forkedFromStepID, &forkedPromptOverride,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if snapshot.Valid {
		e.WorkflowSnapshot = []byte(snapshot.String)
	}
	if currentStepID.Valid {
		e.CurrentStepID = &currentStepID.String
	}
	e.CompletedSteps = []string(completed)
	if stateSnap.Valid {
		e.StateSnapshot = []byte(stateSnap.String)
	}
	if result.Valid {
		e.Result = []byte(result.String)
	}
	if errMsg.Valid {
		e.ErrorMessage = &errMsg.String
	}
	if errCode.Valid {
		e.ErrorCode = &errCode.String
	}
	if startedAt.Valid {
		t := startedAt.Time
		e.StartedAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		e.CompletedAt = &t
	}
	e.CreatedAt = createdAt.Time
	e.UpdatedAt = updatedAt.Time
	if parentExecutionID.Valid {
		e.ParentExecutionID = &parentExecutionID.String
	}
	if forkedFromStepID.Valid {
		e.ForkedFromStepID = &forkedFromStepID.String
	}
	if forkedPromptOverride.Valid {
		e.ForkedPromptOverride = &forkedPromptOverride.String
	}
	return &e, nil
}

func nullableBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
