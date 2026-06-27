package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CrossProjectCallRepository persists rows in cross_project_calls
// — the inter-project orchestration ledger. See
// https://docs.vornik.io
type CrossProjectCallRepository struct {
	db DBTX
}

// NewCrossProjectCallRepository wires the postgres-backed
// implementation. The caller-side executor handler holds the
// only writer to the table; the resolve hook + admin tooling
// holds the only mutators.
func NewCrossProjectCallRepository(db DBTX) *CrossProjectCallRepository {
	return &CrossProjectCallRepository{db: db}
}

func (r *CrossProjectCallRepository) Create(ctx context.Context, c *persistence.CrossProjectCall) error {
	if c == nil {
		return errors.New("cross_project_calls: nil row")
	}
	if c.ID == "" {
		c.ID = persistence.GenerateID("ccp")
	}
	if c.Status == "" {
		c.Status = persistence.CPCStatusPending
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO cross_project_calls (
			id, caller_task_id, caller_step_id, caller_project,
			callee_project, callee_workflow, callee_task_id,
			payload, expected_schema, status, timeout_at, cancel_on_timeout
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		c.ID, c.CallerTaskID, c.CallerStepID, c.CallerProject,
		c.CalleeProject, c.CalleeWorkflow, nullableStrPtr(c.CalleeTaskID),
		jsonbValue(c.Payload), c.ExpectedSchema, string(c.Status),
		nullableTimePtr(c.TimeoutAt), c.CancelOnTimeout,
	)
	return mapDBError(err)
}

func (r *CrossProjectCallRepository) Get(ctx context.Context, id string) (*persistence.CrossProjectCall, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, caller_task_id, caller_step_id, caller_project,
		       callee_project, callee_workflow, callee_task_id,
		       payload, expected_schema, status, result_envelope,
		       error_message, timeout_at, created_at, resolved_at,
		       cancel_on_timeout
		FROM cross_project_calls
		WHERE id = $1
	`, id)
	return scanCrossProjectCall(row)
}

func (r *CrossProjectCallRepository) GetByCalleeTaskID(ctx context.Context, calleeTaskID string) (*persistence.CrossProjectCall, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, caller_task_id, caller_step_id, caller_project,
		       callee_project, callee_workflow, callee_task_id,
		       payload, expected_schema, status, result_envelope,
		       error_message, timeout_at, created_at, resolved_at,
		       cancel_on_timeout
		FROM cross_project_calls
		WHERE callee_task_id = $1
	`, calleeTaskID)
	return scanCrossProjectCall(row)
}

func (r *CrossProjectCallRepository) SetCalleeTaskID(ctx context.Context, id, calleeTaskID string) error {
	if calleeTaskID == "" {
		return errors.New("cross_project_calls: callee_task_id required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE cross_project_calls
		SET callee_task_id = $2
		WHERE id = $1 AND callee_task_id IS NULL
	`, id, calleeTaskID)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the row doesn't exist or callee_task_id was
		// already set — either way the caller shouldn't proceed.
		return persistence.ErrNotFound
	}
	return nil
}

func (r *CrossProjectCallRepository) MarkRunning(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE cross_project_calls
		SET status = 'running'
		WHERE id = $1 AND status = 'pending'
	`, id)
	return mapDBError(err)
}

func (r *CrossProjectCallRepository) MarkCompleted(ctx context.Context, id string, envelope []byte) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE cross_project_calls
		SET status = 'completed',
		    result_envelope = $2,
		    resolved_at = NOW()
		WHERE id = $1
		  AND status NOT IN ('completed', 'failed', 'timed_out', 'rejected')
	`, id, jsonbValue(envelope))
	return mapDBError(err)
}

func (r *CrossProjectCallRepository) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE cross_project_calls
		SET status = 'failed',
		    error_message = NULLIF($2, ''),
		    resolved_at = NOW()
		WHERE id = $1
		  AND status NOT IN ('completed', 'failed', 'timed_out', 'rejected')
	`, id, reason)
	return mapDBError(err)
}

func (r *CrossProjectCallRepository) MarkRejected(ctx context.Context, id, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE cross_project_calls
		SET status = 'rejected',
		    error_message = NULLIF($2, ''),
		    resolved_at = NOW()
		WHERE id = $1
		  AND status NOT IN ('completed', 'failed', 'timed_out', 'rejected')
	`, id, reason)
	return mapDBError(err)
}

func scanCrossProjectCall(row *sql.Row) (*persistence.CrossProjectCall, error) {
	var (
		out            persistence.CrossProjectCall
		statusStr      string
		calleeTaskID   sql.NullString
		resultEnvelope []byte
		errorMessage   sql.NullString
		timeoutAt      sql.NullTime
		resolvedAt     sql.NullTime
	)
	err := row.Scan(
		&out.ID, &out.CallerTaskID, &out.CallerStepID, &out.CallerProject,
		&out.CalleeProject, &out.CalleeWorkflow, &calleeTaskID,
		&out.Payload, &out.ExpectedSchema, &statusStr, &resultEnvelope,
		&errorMessage, &timeoutAt, &out.CreatedAt, &resolvedAt,
		&out.CancelOnTimeout,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	out.Status = persistence.CrossProjectCallStatus(statusStr)
	if calleeTaskID.Valid {
		s := calleeTaskID.String
		out.CalleeTaskID = &s
	}
	if len(resultEnvelope) > 0 {
		out.ResultEnvelope = resultEnvelope
	}
	if errorMessage.Valid {
		s := errorMessage.String
		out.ErrorMessage = &s
	}
	if timeoutAt.Valid {
		t := timeoutAt.Time
		out.TimeoutAt = &t
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		out.ResolvedAt = &t
	}
	return &out, nil
}

// nullableTimePtr converts a *time.Time to a sql.Null* wrapper
// that round-trips correctly through QueryRow + ExecContext.
func nullableTimePtr(t any) any {
	if t == nil {
		return nil
	}
	return t
}

// nullableStrPtr converts a *string to a nil-safe argument.
// Nil-pointer maps to NULL; non-nil dereferences.
func nullableStrPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// ClaimTimedOut sweeps every CPC row past its timeout_at into
// status=timed_out and returns the claimed rows so the scanner
// can emit live events + audit rows + metrics for each one.
//
// Atomicity: the UPDATE … RETURNING pattern claims rows under a
// single transaction without the explicit BEGIN/COMMIT roundtrip
// the pg_advisory pattern needed. Two scanners (HA, 2026.8+)
// running concurrently each see a disjoint set because the
// UPDATE matches only `status IN ('pending','running')` —
// once the first scanner flips a row to timed_out, the second
// scanner's WHERE clause stops matching it.
//
// Bounded by limit so a large backlog (e.g. operator deleted
// the receiving project mid-flight, hundreds of rows time
// out at once) doesn't lock the table.
func (r *CrossProjectCallRepository) ClaimTimedOut(ctx context.Context, now time.Time, limit int) ([]*persistence.CrossProjectCall, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		UPDATE cross_project_calls
		SET status = 'timed_out',
		    error_message = COALESCE(error_message, 'cross_project_call timeout elapsed before resolve'),
		    resolved_at = NOW()
		WHERE id IN (
			SELECT id FROM cross_project_calls
			WHERE status IN ('pending', 'running')
			  AND timeout_at IS NOT NULL
			  AND timeout_at < $1
			ORDER BY timeout_at ASC
			LIMIT $2
		)
		RETURNING id, caller_task_id, caller_step_id, caller_project,
		          callee_project, callee_workflow, callee_task_id,
		          payload, expected_schema, status, result_envelope,
		          error_message, timeout_at, created_at, resolved_at,
		          cancel_on_timeout
	`, now, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []*persistence.CrossProjectCall{}
	for rows.Next() {
		c, err := scanCPCFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanCPCFromRows mirrors scanCrossProjectCall but takes a
// *sql.Rows (the existing helper takes *sql.Row). Kept close
// to the row-scan helper so a schema change touches one place.
func scanCPCFromRows(rows *sql.Rows) (*persistence.CrossProjectCall, error) {
	var (
		out            persistence.CrossProjectCall
		statusStr      string
		calleeTaskID   sql.NullString
		resultEnvelope []byte
		errorMessage   sql.NullString
		timeoutAt      sql.NullTime
		resolvedAt     sql.NullTime
	)
	if err := rows.Scan(
		&out.ID, &out.CallerTaskID, &out.CallerStepID, &out.CallerProject,
		&out.CalleeProject, &out.CalleeWorkflow, &calleeTaskID,
		&out.Payload, &out.ExpectedSchema, &statusStr, &resultEnvelope,
		&errorMessage, &timeoutAt, &out.CreatedAt, &resolvedAt,
		&out.CancelOnTimeout,
	); err != nil {
		return nil, mapDBError(err)
	}
	out.Status = persistence.CrossProjectCallStatus(statusStr)
	if calleeTaskID.Valid {
		s := calleeTaskID.String
		out.CalleeTaskID = &s
	}
	if len(resultEnvelope) > 0 {
		out.ResultEnvelope = resultEnvelope
	}
	if errorMessage.Valid {
		s := errorMessage.String
		out.ErrorMessage = &s
	}
	if timeoutAt.Valid {
		t := timeoutAt.Time
		out.TimeoutAt = &t
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		out.ResolvedAt = &t
	}
	return &out, nil
}

// List returns rows matching filter, newest-first, capped by
// PageSize. Hard ceiling at 1000 rows to bound admin-surface
// pull cost. Zero-value filter returns the most recent rows
// across all projects.
func (r *CrossProjectCallRepository) List(ctx context.Context, filter persistence.CPCListFilter) ([]*persistence.CrossProjectCall, error) {
	limit := filter.PageSize
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	// Build the query incrementally. Parameter numbering must
	// match the args slice order; using a counter keeps the
	// two in sync.
	q := `SELECT id, caller_task_id, caller_step_id, caller_project,
		         callee_project, callee_workflow, callee_task_id,
		         payload, expected_schema, status, result_envelope,
		         error_message, timeout_at, created_at, resolved_at,
		         cancel_on_timeout
		   FROM cross_project_calls
		   WHERE 1=1`
	args := []any{}
	paramN := 1
	if filter.Status != "" {
		q += fmt.Sprintf(" AND status = $%d", paramN)
		args = append(args, string(filter.Status))
		paramN++
	}
	if filter.CallerProject != "" {
		q += fmt.Sprintf(" AND caller_project = $%d", paramN)
		args = append(args, filter.CallerProject)
		paramN++
	}
	if filter.CalleeProject != "" {
		q += fmt.Sprintf(" AND callee_project = $%d", paramN)
		args = append(args, filter.CalleeProject)
		paramN++
	}
	if !filter.CreatedSince.IsZero() {
		q += fmt.Sprintf(" AND created_at >= $%d", paramN)
		args = append(args, filter.CreatedSince)
		paramN++
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", paramN)
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []*persistence.CrossProjectCall{}
	for rows.Next() {
		c, err := scanCPCFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AdminCancel resolves a pending/running CPC as rejected with
// the operator-supplied reason. Same WHERE-guard as the
// MarkXxx family: idempotent on an already-terminal row.
func (r *CrossProjectCallRepository) AdminCancel(ctx context.Context, id, reason string) error {
	if reason == "" {
		reason = "operator-cancelled via admin API"
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE cross_project_calls
		SET status = 'rejected',
		    error_message = $2,
		    resolved_at = NOW()
		WHERE id = $1
		  AND status NOT IN ('completed', 'failed', 'timed_out', 'rejected')
	`, id, reason)
	return mapDBError(err)
}

// Avoid unused-import error when fmt isn't referenced after edits.
var _ = fmt.Sprintf
