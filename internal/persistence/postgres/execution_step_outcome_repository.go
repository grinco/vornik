package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionStepOutcomeRepository implements
// persistence.ExecutionStepOutcomeRepository using PostgreSQL.
type ExecutionStepOutcomeRepository struct {
	db DBTX
}

// NewExecutionStepOutcomeRepository creates a new repository instance.
func NewExecutionStepOutcomeRepository(db DBTX) *ExecutionStepOutcomeRepository {
	return &ExecutionStepOutcomeRepository{db: db}
}

// Record inserts one outcome row. Callers typically write with
// Outcome="pending_validation" at step completion; the consumer
// finalizes later.
func (r *ExecutionStepOutcomeRepository) Record(ctx context.Context, o *persistence.ExecutionStepOutcome) error {
	if o == nil {
		return fmt.Errorf("nil outcome row")
	}
	recordedAt := o.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_step_outcomes (
			id, project_id, task_id, execution_id, step_id,
			role, model, outcome, attributed_to_step_id,
			error_class, error_detail, duration_ms,
			finalized_at, recorded_at, hallucination_signals,
			context_source,
			complexity_tier, effective_tool_budget, tool_calls_used
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		o.ID, o.ProjectID, o.TaskID, o.ExecutionID, o.StepID,
		o.Role, o.Model, o.Outcome, nullableString(o.AttributedToStepID),
		o.ErrorClass, o.ErrorDetail, nullableInt64(o.DurationMS),
		nullableTime(o.FinalizedAt), recordedAt,
		nullableJSONB(o.HallucinationSignals),
		emptyStringToNullable(o.ContextSource),
		emptyStringToNullable(o.ComplexityTier), nullableInt(o.EffectiveToolBudget), nullableInt(o.ToolCallsUsed),
	)
	return mapDBError(err)
}

// emptyStringToNullable maps an empty string to SQL NULL so the
// context_source column carries NULL on legacy projects rather
// than the empty-sentinel "". Keeps operator queries clean:
// `WHERE context_source = 'plain_autonomy'` returns only rows
// that actually resolved that layout, not "" rows too.
func emptyStringToNullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableJSONB converts a []byte into a driver-binding form
// suitable for a JSONB column: empty/nil bytes go through as
// SQL NULL, non-empty bytes pass through verbatim. Done here
// rather than at every call site because the conversion has a
// subtle gotcha — passing []byte{} (not nil) to lib/pq would
// drive an INSERT of "" which JSONB rejects with a parse
// error.
func nullableJSONB(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Finalize sets outcome + related fields on a specific row by its
// primary key. Idempotent: finalizing an already-finalized row is a
// no-op if the values match, and overwrites otherwise (we keep the
// latest finalizer wins semantics — attribution chains don't revisit).
func (r *ExecutionStepOutcomeRepository) Finalize(ctx context.Context, id, outcome, errorClass, errorDetail string, attributedToStepID *string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = $1,
		    error_class = $2,
		    error_detail = $3,
		    attributed_to_step_id = $4,
		    finalized_at = NOW()
		WHERE id = $5`,
		outcome, errorClass, errorDetail, nullableString(attributedToStepID), id,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// FinalizePending finalizes the most recent pending_validation row for
// (executionID, stepID) and returns that row's (role, model) so callers
// can emit per-outcome metrics. Returns ErrNotFound when there is no
// pending row — callers treat that as non-fatal because the table may
// be empty for pre-outcome executions or the row may already have been
// finalized along another path.
func (r *ExecutionStepOutcomeRepository) FinalizePending(ctx context.Context, executionID, stepID, outcome, errorClass, errorDetail string, attributedToStepID *string) (string, string, error) {
	// UPDATE ... RETURNING to get the finalized row's role+model in one
	// round trip. Narrowed to the single most-recent pending row via a
	// subquery with LIMIT 1 — defensive in case a step somehow has two
	// pending rows (shouldn't happen in practice).
	// Compare-and-set: the outer WHERE re-asserts outcome =
	// 'pending_validation' (not only the subquery that picks the id) so
	// the finalize is a genuine CAS. Without it, two siblings finalizing
	// the same parent pending row (A→B ok vs A→C error) both resolve the
	// same id in their subqueries and the outer `id = X` predicate, which
	// EvalPlanQual re-checks under READ COMMITTED, still matches after the
	// first commit — letting the second overwrite a terminal outcome
	// (last-write-wins). Re-asserting the status makes the second UPDATE
	// match 0 rows → ErrNotFound, so the first finalizer wins. (Memory LLD
	// review batch 4, 2026-06-11.)
	var role, model string
	err := r.db.QueryRowContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = $1,
		    error_class = $2,
		    error_detail = $3,
		    attributed_to_step_id = $4,
		    finalized_at = NOW()
		WHERE id = (
		    SELECT id FROM execution_step_outcomes
		    WHERE execution_id = $5 AND step_id = $6 AND outcome = 'pending_validation'
		    ORDER BY recorded_at DESC
		    LIMIT 1
		)
		AND outcome = 'pending_validation'
		RETURNING role, model`,
		outcome, errorClass, errorDetail, nullableString(attributedToStepID),
		executionID, stepID,
	).Scan(&role, &model)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", persistence.ErrNotFound
		}
		return "", "", mapDBError(err)
	}
	return role, model, nil
}

// SweepPending finalizes every remaining pending_validation row for an
// execution and returns the (step_id, role, model) of each so the
// caller can emit Prometheus events for them — otherwise the quality
// gauges miss the sweep events entirely, which is systematic for the
// last step of every execution (no consumer to finalize it explicitly).
func (r *ExecutionStepOutcomeRepository) SweepPending(ctx context.Context, executionID, fallbackOutcome string) ([]persistence.SweepResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = $1,
		    finalized_at = NOW()
		WHERE execution_id = $2 AND outcome = 'pending_validation'
		RETURNING step_id, role, model`,
		fallbackOutcome, executionID,
	)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.SweepResult
	for rows.Next() {
		var r persistence.SweepResult
		if err := rows.Scan(&r.StepID, &r.Role, &r.Model); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// List returns outcome rows matching the filter, newest first.
func (r *ExecutionStepOutcomeRepository) List(ctx context.Context, f persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	query := `
		SELECT id, project_id, task_id, execution_id, step_id,
		       role, model, outcome, attributed_to_step_id,
		       error_class, error_detail, duration_ms,
		       finalized_at, recorded_at, hallucination_signals,
		       complexity_tier, effective_tool_budget, tool_calls_used
		FROM execution_step_outcomes WHERE 1=1`
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
	if f.StepID != nil {
		query += fmt.Sprintf(" AND step_id = $%d", pos)
		args = append(args, *f.StepID)
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
	if f.Outcome != nil {
		query += fmt.Sprintf(" AND outcome = $%d", pos)
		args = append(args, *f.Outcome)
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

	// id is the secondary sort key so two rows recorded in the same
	// Postgres microsecond come back in a deterministic order. Without
	// it, the UI handler's Go-side resort (which matches the same key
	// pair) had no way to recover the original insert order from a
	// shuffled tie group, and operators reported step lists arriving
	// scrambled across reloads.
	query += " ORDER BY recorded_at DESC, id DESC"
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

	var out []*persistence.ExecutionStepOutcome
	for rows.Next() {
		var (
			o                   persistence.ExecutionStepOutcome
			attributed          sql.NullString
			durationMS          sql.NullInt64
			finalizedAt         sql.NullTime
			signals             []byte
			complexityTier      sql.NullString
			effectiveToolBudget sql.NullInt64
			toolCallsUsed       sql.NullInt64
		)
		if err := rows.Scan(
			&o.ID, &o.ProjectID, &o.TaskID, &o.ExecutionID, &o.StepID,
			&o.Role, &o.Model, &o.Outcome, &attributed,
			&o.ErrorClass, &o.ErrorDetail, &durationMS,
			&finalizedAt, &o.RecordedAt, &signals,
			&complexityTier, &effectiveToolBudget, &toolCallsUsed,
		); err != nil {
			return nil, err
		}
		if len(signals) > 0 {
			o.HallucinationSignals = signals
		}
		o.AttributedToStepID = stringPtrOrNil(attributed)
		if durationMS.Valid {
			v := durationMS.Int64
			o.DurationMS = &v
		}
		if finalizedAt.Valid {
			v := finalizedAt.Time
			o.FinalizedAt = &v
		}
		if complexityTier.Valid {
			o.ComplexityTier = complexityTier.String
		}
		if effectiveToolBudget.Valid {
			v := int(effectiveToolBudget.Int64)
			o.EffectiveToolBudget = &v
		}
		if toolCallsUsed.Valid {
			v := int(toolCallsUsed.Int64)
			o.ToolCallsUsed = &v
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// SupersedeAfter relabels every outcome for an execution whose
// recorded_at is strictly after `cutoff`. Used by retry-from-step
// to mark the original-run rows non-canonical without losing the
// audit trail. The strict "after" comparison is what lets the
// caller pass the recorded_at of the last surviving row's outcome
// and have that row stay intact.
func (r *ExecutionStepOutcomeRepository) SupersedeAfter(ctx context.Context, executionID string, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = 'superseded',
		    finalized_at = NOW()
		WHERE execution_id = $1
		  AND recorded_at > $2
		  AND outcome != 'superseded'`,
		executionID, cutoff,
	)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// CountByRoleModelOutcome groups finalized outcome rows by (role,
// model) and returns the count matching the supplied outcome literal
// within the window. recorded_at is the time axis (not finalized_at)
// because callers (effective-cost alerts) want "rows produced in
// the window" — not "rows finalized in the window" which would mix
// pending-then-finalized rows from prior periods into a recency
// query. Empty role/model rows are skipped at the SQL layer
// because dashboards group on those columns and a blank row pollutes
// the output.
func (r *ExecutionStepOutcomeRepository) CountByRoleModelOutcome(ctx context.Context, outcome string, since, until time.Time, projectID string) ([]persistence.RoleModelOutcomeCount, error) {
	query := `
		SELECT role, model, COUNT(*)
		FROM execution_step_outcomes
		WHERE outcome = $1
		  AND role <> ''
		  AND model <> ''`
	args := []any{outcome}
	pos := 2
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
	query += ` GROUP BY role, model ORDER BY role, model`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.RoleModelOutcomeCount
	for rows.Next() {
		var c persistence.RoleModelOutcomeCount
		if err := rows.Scan(&c.Role, &c.Model, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nullableInt64 wraps a *int64 into sql.NullInt64 for driver binding.
func nullableInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// nullableInt wraps a *int into sql.NullInt64 for driver binding.
// Used for the migration-106 budget-stamp columns (effective_tool_budget,
// tool_calls_used) which are typed as Go *int in the model.
func nullableInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

// nullableTime wraps a *time.Time into sql.NullTime for driver binding.
func nullableTime(v *time.Time) sql.NullTime {
	if v == nil || v.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *v, Valid: true}
}
