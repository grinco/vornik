package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionHintRepository implements
// persistence.ExecutionHintRepository on PostgreSQL.
//
// Feature #3 Phase C — operator hint injection for live
// executions. Each hint row carries either a task_id (carries
// across retries) or execution_id (one-execution scope), an
// optional step_id, content, and audit fields (created_at,
// created_by). The executor consumes pending rows at step start;
// the API endpoint inserts new rows on POST /executions/{id}/hints
// or POST /tasks/{id}/hints.
type ExecutionHintRepository struct {
	db DBTX
}

// NewExecutionHintRepository wires the repo over a DBTX.
func NewExecutionHintRepository(db DBTX) *ExecutionHintRepository {
	return &ExecutionHintRepository{db: db}
}

// Insert writes a new pending hint. applied_at stays NULL until
// the executor consumes it via ConsumePending. Exactly one of
// h.TaskID / h.ExecutionID must be set (both nil → invalid;
// both set is also rejected so the scope is unambiguous).
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
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		h.ID, taskID, executionID, stepID, h.Content, createdAt, h.CreatedBy)
	return mapDBError(err)
}

// ConsumePending atomically reads-and-marks-applied all pending
// hints whose scope matches (taskID, executionID, stepID). The
// atomicity is critical: without it, two concurrent step starts
// could each see the same pending hint and double-apply it.
//
// Scope match:
//   - taskID != "" → hints with task_id = taskID AND execution_id IS NULL
//     (the task-scope class — survives retries).
//   - executionID != "" → hints with execution_id = executionID
//     (the legacy per-execution class).
//
// Both predicates OR together so a single call drains both classes
// for the current step. stepID="" matches step_id IS NULL only;
// stepID set matches step_id IS NULL OR step_id = stepID.
//
// Returns the consumed rows in insertion order (created_at ASC).
func (r *ExecutionHintRepository) ConsumePending(ctx context.Context, taskID, executionID, stepID string) ([]*persistence.ExecutionHint, error) {
	if taskID == "" && executionID == "" {
		return nil, fmt.Errorf("consume requires task_id or execution_id")
	}
	// Build the scope predicate inline since pgx doesn't support
	// optional named parameters cleanly. We always send 4 args
	// regardless and use NULLs for unset scopes.
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
	// Scope clause:
	//   (task_id = $1 AND execution_id IS NULL) OR (execution_id = $2)
	// With NULLs the equality fails (= NULL is unknown), so unset
	// scopes don't accidentally match. Step clause is identical to
	// the pre-task-scope version.
	query := `
		UPDATE execution_hints
		SET applied_at = NOW()
		WHERE applied_at IS NULL
		  AND (
		      (task_id = $1 AND execution_id IS NULL)
		   OR (execution_id = $2)
		  )
		  AND (
		      ($3::text IS NULL AND step_id IS NULL)
		   OR ($3::text IS NOT NULL AND (step_id IS NULL OR step_id = $3))
		  )
		RETURNING id, task_id, execution_id, step_id, content, applied_at, created_at, created_by`
	rows, err := r.db.QueryContext(ctx, query, taskArg, execArg, stepArg)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// maxHintListRows caps the result size of ListByExecution. The
// hint insert path enforces a 4 KiB content cap, but nothing
// stops a caller from inserting thousands of hint rows; an
// unbounded SELECT would then buffer all of them into a slice
// on every GET, enabling memory exhaustion.
//
// 500 is generous — a real operator session won't accumulate
// more than a dozen hints. Pagination can come later if the
// surface grows; for now this is the safety bound.
const maxHintListRows = 500

// ListByExecution returns the most-recent hints for an execution,
// newest first. Hard-capped at maxHintListRows to prevent
// memory-exhaustion via mass-insert.
func (r *ExecutionHintRepository) ListByExecution(ctx context.Context, executionID string) ([]*persistence.ExecutionHint, error) {
	if executionID == "" {
		return nil, fmt.Errorf("execution id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE execution_id = $1
		ORDER BY created_at DESC
		LIMIT $2`,
		executionID, maxHintListRows)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListForExecution returns both the execution-scoped hints
// (execution_id = $1) and the task-scoped hints
// (task_id = $2 AND execution_id IS NULL) for one execution's live
// view, newest-first. Task-scoped hints carry across retries, so the
// live page must surface them alongside the per-execution ones — the
// execution-only ListByExecution under-reported them (2026-05-29
// LLD-drift audit §8.6). taskID="" disables the task-scoped predicate
// (= NULL never matches), degrading to execution-only.
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
		WHERE execution_id = $1
		   OR (task_id = $2 AND execution_id IS NULL)
		ORDER BY created_at DESC
		LIMIT $3`,
		executionID, taskArg, maxHintListRows)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListByTask returns every task-scoped hint (any state) ordered
// oldest-first. The unified-timeline refactor (2026-05-26) uses
// this to interleave hints with task_messages by CreatedAt in the
// task detail conversation thread.
func (r *ExecutionHintRepository) ListByTask(ctx context.Context, taskID string) ([]*persistence.ExecutionHint, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE task_id = $1
		  AND execution_id IS NULL
		ORDER BY created_at ASC
		LIMIT $2`,
		taskID, maxHintListRows)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListPendingForTask returns task-scoped hints (execution_id IS
// NULL) that have not yet been consumed. Used by the API to show
// operators which task-level steering messages are still queued
// for the next execution.
func (r *ExecutionHintRepository) ListPendingForTask(ctx context.Context, taskID string) ([]*persistence.ExecutionHint, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, step_id, content, applied_at, created_at, created_by
		FROM execution_hints
		WHERE task_id = $1
		  AND execution_id IS NULL
		  AND applied_at IS NULL
		ORDER BY created_at ASC
		LIMIT $2`,
		taskID, maxHintListRows)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ExecutionHint, 0)
	for rows.Next() {
		h, err := scanExecutionHint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanExecutionHint(scanner interface {
	Scan(dest ...any) error
}) (*persistence.ExecutionHint, error) {
	var (
		h           persistence.ExecutionHint
		taskID      sql.NullString
		executionID sql.NullString
		stepID      sql.NullString
		appliedAt   sql.NullTime
	)
	err := scanner.Scan(
		&h.ID, &taskID, &executionID, &stepID, &h.Content, &appliedAt, &h.CreatedAt, &h.CreatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
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
	return &h, nil
}
