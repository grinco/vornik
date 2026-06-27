package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionHintRepository implements
// persistence.ExecutionHintRepository on SQLite. Mirrors the
// postgres impl shape — TIMESTAMPTZ → TEXT (ISO-8601), NULL
// handling via sql.Null*.
//
// SQLite doesn't support `UPDATE ... RETURNING` until 3.35, but
// modernc.org/sqlite ships 3.40+, so we use the same atomic
// UPDATE-RETURNING the postgres impl uses.
//
// 2026-05-26: task_id added so hints can scope to a whole task
// and survive a retry/requeue (new execution_id) — see the
// ExecutionHintRepository interface docstring for the contract.
type ExecutionHintRepository struct {
	db *sql.DB
}

func NewExecutionHintRepository(db *sql.DB) *ExecutionHintRepository {
	return &ExecutionHintRepository{db: db}
}

func (r *ExecutionHintRepository) Insert(ctx context.Context, h *persistence.ExecutionHint) error {
	if h == nil {
		return fmt.Errorf("nil hint")
	}
	if h.ID == "" {
		return fmt.Errorf("hint id required")
	}
	if h.TaskID == "" && h.ExecutionID == "" {
		return fmt.Errorf("hint requires task_id or execution_id")
	}
	if h.TaskID != "" && h.ExecutionID != "" {
		return fmt.Errorf("hint cannot set both task_id and execution_id")
	}
	if h.Content == "" {
		return fmt.Errorf("hint content required")
	}
	createdAt := h.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var (
		taskID      any
		executionID any
		stepID      any
	)
	if h.TaskID != "" {
		taskID = h.TaskID
	}
	if h.ExecutionID != "" {
		executionID = h.ExecutionID
	}
	if h.StepID != "" {
		stepID = h.StepID
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_hints (id, task_id, execution_id, step_id, content, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		h.ID, taskID, executionID, stepID, h.Content, sqliteTime(createdAt), h.CreatedBy)
	return err
}

func (r *ExecutionHintRepository) ConsumePending(ctx context.Context, taskID, executionID, stepID string) ([]*persistence.ExecutionHint, error) {
	if taskID == "" && executionID == "" {
		return nil, fmt.Errorf("consume requires task_id or execution_id")
	}
	now := sqliteTime(time.Now().UTC())
	var (
		taskArg any
		execArg any
		stepArg any
	)
	if taskID != "" {
		taskArg = taskID
	}
	if executionID != "" {
		execArg = executionID
	}
	if stepID != "" {
		stepArg = stepID
	}
	// Equivalent of the postgres scope predicate; sqlite supports
	// the same NULL-comparison semantics.
	rows, err := r.db.QueryContext(ctx, `
		UPDATE execution_hints
		SET applied_at = ?
		WHERE applied_at IS NULL
		  AND (
		      (task_id = ? AND execution_id IS NULL)
		   OR (execution_id = ?)
		  )
		  AND (
		      (? IS NULL AND step_id IS NULL)
		   OR (? IS NOT NULL AND (step_id IS NULL OR step_id = ?))
		  )
		RETURNING id, task_id, execution_id, step_id, content, applied_at, created_at, created_by`,
		now, taskArg, execArg, stepArg, stepArg, stepArg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanSqliteExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (r *ExecutionHintRepository) ListByExecution(ctx context.Context, executionID string) ([]*persistence.ExecutionHint, error) {
	if executionID == "" {
		return nil, fmt.Errorf("execution id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE execution_id = ?
		ORDER BY created_at DESC`,
		executionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanSqliteExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListForExecution returns execution-scoped + task-scoped hints for
// one execution's live view, newest-first. Mirrors the postgres
// impl: the live page must surface task-scoped hints (which carry
// across retries) alongside the per-execution ones (2026-05-29
// LLD-drift audit §8.6). taskID="" disables the task-scoped clause.
func (r *ExecutionHintRepository) ListForExecution(ctx context.Context, executionID, taskID string) ([]*persistence.ExecutionHint, error) {
	if executionID == "" {
		return nil, fmt.Errorf("execution id required")
	}
	var taskArg any
	if taskID != "" {
		taskArg = taskID
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE execution_id = ?
		   OR (task_id = ? AND execution_id IS NULL)
		ORDER BY created_at DESC`,
		executionID, taskArg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanSqliteExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListByTask returns every task-scoped hint (any state) ordered
// oldest-first. Mirrors the postgres impl; sqlite doesn't carry the
// row-cap but the underlying table has the 4 KiB content guard so
// memory exposure stays bounded.
func (r *ExecutionHintRepository) ListByTask(ctx context.Context, taskID string) ([]*persistence.ExecutionHint, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE task_id = ?
		  AND execution_id IS NULL
		ORDER BY created_at ASC`,
		taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanSqliteExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListPendingForTask returns task-scoped pending hints (execution_id
// IS NULL, applied_at IS NULL). Mirrors the postgres impl.
func (r *ExecutionHintRepository) ListPendingForTask(ctx context.Context, taskID string) ([]*persistence.ExecutionHint, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE task_id = ?
		  AND execution_id IS NULL
		  AND applied_at IS NULL
		ORDER BY created_at ASC`,
		taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanSqliteExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanSqliteExecutionHint(scanner interface{ Scan(dest ...any) error }) (*persistence.ExecutionHint, error) {
	var (
		h           persistence.ExecutionHint
		taskID      sql.NullString
		executionID sql.NullString
		stepID      sql.NullString
		appliedAt   sqlNullTime
		createdAt   sqlTime
	)
	err := scanner.Scan(&h.ID, &taskID, &executionID, &stepID, &h.Content, &appliedAt, &createdAt, &h.CreatedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if taskID.Valid {
		h.TaskID = taskID.String
	}
	if executionID.Valid {
		h.ExecutionID = executionID.String
	}
	if stepID.Valid {
		h.StepID = stepID.String
	}
	if appliedAt.Valid {
		t := appliedAt.Time
		h.AppliedAt = &t
	}
	h.CreatedAt = createdAt.Time
	return &h, nil
}
