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

// ExecutionStepOutcomeRepository persists per-step outcome
// classifications.
//
// FinalizePending uses a two-step pattern (SELECT id of most recent
// pending row → UPDATE returning role/model) because SQLite has no
// UPDATE … FROM (SELECT …) RETURNING combo without subquery in
// WHERE. SweepPending uses RETURNING (SQLite 3.35+).
type ExecutionStepOutcomeRepository struct {
	db DBTX
}

func NewExecutionStepOutcomeRepository(db DBTX) *ExecutionStepOutcomeRepository {
	return &ExecutionStepOutcomeRepository{db: db}
}

func (r *ExecutionStepOutcomeRepository) Record(ctx context.Context, o *persistence.ExecutionStepOutcome) error {
	if o == nil {
		return fmt.Errorf("nil outcome row")
	}
	if o.RecordedAt.IsZero() {
		o.RecordedAt = time.Now().UTC()
	}
	var duration any
	if o.DurationMS != nil {
		duration = *o.DurationMS
	}
	var effectiveBudget any
	if o.EffectiveToolBudget != nil {
		effectiveBudget = *o.EffectiveToolBudget
	}
	var toolCallsUsed any
	if o.ToolCallsUsed != nil {
		toolCallsUsed = *o.ToolCallsUsed
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_step_outcomes (
			id, project_id, task_id, execution_id, step_id,
			role, model, outcome, attributed_to_step_id,
			error_class, error_detail, duration_ms,
			finalized_at, recorded_at, hallucination_signals,
			complexity_tier, effective_tool_budget, tool_calls_used
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.ProjectID, o.TaskID, o.ExecutionID, o.StepID,
		o.Role, o.Model, o.Outcome, o.AttributedToStepID,
		o.ErrorClass, o.ErrorDetail, duration,
		sqliteTimePtr(o.FinalizedAt), sqliteTime(o.RecordedAt),
		nullableBlob(o.HallucinationSignals),
		nullableString(o.ComplexityTier), effectiveBudget, toolCallsUsed,
	)
	return err
}

func (r *ExecutionStepOutcomeRepository) Finalize(ctx context.Context, id, outcome, errorClass, errorDetail string, attributedToStepID *string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = ?,
		    error_class = ?,
		    error_detail = ?,
		    attributed_to_step_id = ?,
		    finalized_at = ?
		WHERE id = ?`,
		outcome, errorClass, errorDetail, attributedToStepID,
		sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *ExecutionStepOutcomeRepository) FinalizePending(ctx context.Context, executionID, stepID, outcome, errorClass, errorDetail string, attributedToStepID *string) (string, string, error) {
	// Two-step: locate the most recent pending row, then CAS-UPDATE it.
	// The UPDATE re-asserts outcome = 'pending_validation' and checks
	// RowsAffected, so if another finalizer terminalised the row between
	// the SELECT and the UPDATE the write is a no-op → ErrNotFound (the
	// first finalizer wins, no last-write-wins overwrite of a terminal
	// outcome). (Memory LLD review batch 4, 2026-06-11.)
	var (
		id          string
		role, model string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, role, model FROM execution_step_outcomes
		WHERE execution_id = ? AND step_id = ? AND outcome = 'pending_validation'
		ORDER BY recorded_at DESC
		LIMIT 1`, executionID, stepID).Scan(&id, &role, &model)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", persistence.ErrNotFound
		}
		return "", "", err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = ?, error_class = ?, error_detail = ?,
		    attributed_to_step_id = ?, finalized_at = ?
		WHERE id = ? AND outcome = 'pending_validation'`,
		outcome, errorClass, errorDetail, attributedToStepID,
		sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return "", "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", "", persistence.ErrNotFound
	}
	return role, model, nil
}

func (r *ExecutionStepOutcomeRepository) SweepPending(ctx context.Context, executionID, fallbackOutcome string) ([]persistence.SweepResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = ?, finalized_at = ?
		WHERE execution_id = ? AND outcome = 'pending_validation'
		RETURNING step_id, role, model`,
		fallbackOutcome, sqliteTime(time.Now().UTC()), executionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.SweepResult
	for rows.Next() {
		var s persistence.SweepResult
		if err := rows.Scan(&s.StepID, &s.Role, &s.Model); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *ExecutionStepOutcomeRepository) List(ctx context.Context, f persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, task_id, execution_id, step_id,
		       role, model, outcome, attributed_to_step_id,
		       error_class, error_detail, duration_ms,
		       finalized_at, recorded_at, hallucination_signals,
		       complexity_tier, effective_tool_budget, tool_calls_used
		FROM execution_step_outcomes WHERE 1=1`)
	args := make([]any, 0, 10)
	if f.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *f.ProjectID)
	}
	if f.TaskID != nil {
		b.WriteString(" AND task_id = ?")
		args = append(args, *f.TaskID)
	}
	if f.ExecutionID != nil {
		b.WriteString(" AND execution_id = ?")
		args = append(args, *f.ExecutionID)
	}
	if f.StepID != nil {
		b.WriteString(" AND step_id = ?")
		args = append(args, *f.StepID)
	}
	if f.Role != nil {
		b.WriteString(" AND role = ?")
		args = append(args, *f.Role)
	}
	if f.Model != nil {
		b.WriteString(" AND model = ?")
		args = append(args, *f.Model)
	}
	if f.Outcome != nil {
		b.WriteString(" AND outcome = ?")
		args = append(args, *f.Outcome)
	}
	if f.Since != nil {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(*f.Since))
	}
	if f.Until != nil {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(*f.Until))
	}
	b.WriteString(" ORDER BY recorded_at DESC, id DESC")
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

	var out []*persistence.ExecutionStepOutcome
	for rows.Next() {
		var (
			o                   persistence.ExecutionStepOutcome
			attributed          sql.NullString
			durationMS          sql.NullInt64
			finalizedAt         sqlNullTime
			recordedAt          sqlTime
			signals             sql.NullString
			complexityTier      sql.NullString
			effectiveToolBudget sql.NullInt64
			toolCallsUsed       sql.NullInt64
		)
		if err := rows.Scan(
			&o.ID, &o.ProjectID, &o.TaskID, &o.ExecutionID, &o.StepID,
			&o.Role, &o.Model, &o.Outcome, &attributed,
			&o.ErrorClass, &o.ErrorDetail, &durationMS,
			&finalizedAt, &recordedAt, &signals,
			&complexityTier, &effectiveToolBudget, &toolCallsUsed,
		); err != nil {
			return nil, err
		}
		if attributed.Valid {
			o.AttributedToStepID = &attributed.String
		}
		if durationMS.Valid {
			v := durationMS.Int64
			o.DurationMS = &v
		}
		if finalizedAt.Valid {
			t := finalizedAt.Time
			o.FinalizedAt = &t
		}
		o.RecordedAt = recordedAt.Time
		if signals.Valid {
			o.HallucinationSignals = []byte(signals.String)
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

func (r *ExecutionStepOutcomeRepository) SupersedeAfter(ctx context.Context, executionID string, cutoff time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = 'superseded', finalized_at = ?
		WHERE execution_id = ?
		  AND recorded_at > ?
		  AND outcome != 'superseded'`,
		sqliteTime(time.Now().UTC()), executionID, sqliteTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *ExecutionStepOutcomeRepository) CountByRoleModelOutcome(ctx context.Context, outcome string, since, until time.Time, projectID string) ([]persistence.RoleModelOutcomeCount, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT role, model, COUNT(*)
		FROM execution_step_outcomes
		WHERE outcome = ?
		  AND role <> ''
		  AND model <> ''`)
	args := []any{outcome}
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
	b.WriteString(" GROUP BY role, model ORDER BY role, model")

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
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
