package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
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
			role, model, agent_image_id, outcome, attributed_to_step_id,
			error_class, error_detail, duration_ms,
			finalized_at, recorded_at, hallucination_signals,
			complexity_tier, effective_tool_budget, tool_calls_used,
			untrusted_content_used, untrusted_sources, requires_review,
			container_exit_code,
			prompt_system_hash, prompt_user_hash, prompt_tools_hash,
			input_hash, result_hash
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.ProjectID, o.TaskID, o.ExecutionID, o.StepID,
		o.Role, o.Model, nullableString(o.AgentImageID), o.Outcome, o.AttributedToStepID,
		o.ErrorClass, o.ErrorDetail, duration,
		sqliteTimePtr(o.FinalizedAt), sqliteTime(o.RecordedAt),
		nullableBlob(o.HallucinationSignals),
		nullableString(o.ComplexityTier), effectiveBudget, toolCallsUsed,
		o.UntrustedContentUsed, nullableBlob(o.UntrustedSources), o.RequiresReview,
		o.ContainerExitCode,
		o.PromptHashes.System, o.PromptHashes.User, o.PromptHashes.Tools,
		o.PromptHashes.Input, o.PromptHashes.Result,
	)
	return err
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

// SweepPendingForTerminalExecutions relabels pending rows under executions that
// reached a terminal status without their own sweep running. See the interface
// doc for why this is a reconciler and why the label is `superseded`.
func (r *ExecutionStepOutcomeRepository) SweepPendingForTerminalExecutions(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	cutoff := sqliteTime(time.Now().UTC().Add(-olderThan))
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = ?, finalized_at = ?
		WHERE id IN (
		    SELECT o2.id
		    FROM execution_step_outcomes o2
		    JOIN executions e ON e.id = o2.execution_id
		    WHERE o2.outcome = 'pending_validation'
		      AND e.status IN ('COMPLETED', 'FAILED', 'CANCELLED')
		      AND COALESCE(e.completed_at, e.updated_at) < ?
		    ORDER BY o2.recorded_at ASC
		    LIMIT ?
		)`,
		string(stepoutcome.Orphaned), sqliteTime(time.Now().UTC()), cutoff, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SweepPendingForTaskOrphans finalizes the pending outcomes under a task's
// TERMINAL executions, to `orphaned`, with no settle grace.
//
// Called by the two paths that terminalise a task's leftover executions —
// supersedeStaleExecutions and cascadeOrphanExecutions — immediately after they
// do it, so the fact is recorded by the code that knows it rather than inferred
// minutes later by the watchdog backstop. No grace, because the caller has just
// finished terminalising these rows itself: there is nothing to race with.
//
// Scoped by TASK, not by execution id, because the supersede sweeps report a
// COUNT rather than the ids they touched. It is idempotent and bounded to one
// task.
//
// It does NOT replace the backstop: a step can be writing its outcome row while
// its execution is being cancelled, and that row commits after this sweep has
// run. The backstop is for exactly that race.
//
// https://docs.vornik.io
func (r *ExecutionStepOutcomeRepository) SweepPendingForTaskOrphans(ctx context.Context, taskID string) (int64, error) {
	if taskID == "" {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE execution_step_outcomes
		SET outcome = ?, finalized_at = ?
		WHERE outcome = 'pending_validation'
		  AND execution_id IN (
		      SELECT e.id FROM executions e
		      WHERE e.task_id = ?
		        AND e.status IN ('COMPLETED', 'FAILED', 'CANCELLED')
		  )`,
		string(stepoutcome.Orphaned), sqliteTime(time.Now().UTC()), taskID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *ExecutionStepOutcomeRepository) List(ctx context.Context, f persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, task_id, execution_id, step_id,
		       role, model, agent_image_id, outcome, attributed_to_step_id,
		       error_class, error_detail, duration_ms,
		       finalized_at, recorded_at, hallucination_signals,
		       complexity_tier, effective_tool_budget, tool_calls_used,
		       untrusted_content_used, untrusted_sources, requires_review,
		       container_exit_code,
		       prompt_system_hash, prompt_user_hash, prompt_tools_hash,
		       input_hash, result_hash
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
	if len(f.ExecutionIDs) > 0 {
		b.WriteString(" AND execution_id IN (")
		for i, id := range f.ExecutionIDs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("?")
			args = append(args, id)
		}
		b.WriteString(")")
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
			untrustedUsed       sql.NullBool
			untrustedSources    sql.NullString
			requiresReview      sql.NullBool
			containerExitCode   sql.NullInt64
			agentImageID        sql.NullString
		)
		if err := rows.Scan(
			&o.ID, &o.ProjectID, &o.TaskID, &o.ExecutionID, &o.StepID,
			&o.Role, &o.Model, &agentImageID, &o.Outcome, &attributed,
			&o.ErrorClass, &o.ErrorDetail, &durationMS,
			&finalizedAt, &recordedAt, &signals,
			&complexityTier, &effectiveToolBudget, &toolCallsUsed,
			&untrustedUsed, &untrustedSources, &requiresReview,
			&containerExitCode,
			&o.PromptHashes.System, &o.PromptHashes.User, &o.PromptHashes.Tools,
			&o.PromptHashes.Input, &o.PromptHashes.Result,
		); err != nil {
			return nil, err
		}
		o.UntrustedContentUsed = untrustedUsed.Valid && untrustedUsed.Bool
		if agentImageID.Valid {
			o.AgentImageID = agentImageID.String
		}
		if untrustedSources.Valid && untrustedSources.String != "" {
			o.UntrustedSources = []byte(untrustedSources.String)
		}
		o.RequiresReview = requiresReview.Valid && requiresReview.Bool
		// Valid-checked, never defaulted: a NULL means no container ran, and
		// collapsing that to 0 would report every non-container step as a
		// clean container exit.
		if containerExitCode.Valid {
			v := int(containerExitCode.Int64)
			o.ContainerExitCode = &v
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

// TaintedStepsForTasks returns the untrusted-content step rows for a batch of
// task IDs in ONE query (taint-lineage-tracking §4.3, I7). Uses the
// idx_step_outcomes_task_taint partial index. Unknown-only rows
// (requires_review=false) are returned too (F3). Empty input → nil.
func (r *ExecutionStepOutcomeRepository) TaintedStepsForTasks(ctx context.Context, taskIDs []string) ([]persistence.TaintedStepRow, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString(`
		SELECT task_id, requires_review, untrusted_sources
		FROM execution_step_outcomes
		WHERE untrusted_content_used = 1 AND task_id IN (`)
	args := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("?")
		args[i] = id
	}
	b.WriteString(")")
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.TaintedStepRow
	for rows.Next() {
		var (
			row     persistence.TaintedStepRow
			review  sql.NullBool
			sources sql.NullString
		)
		if err := rows.Scan(&row.TaskID, &review, &sources); err != nil {
			return nil, err
		}
		row.RequiresReview = review.Valid && review.Bool
		if sources.Valid && sources.String != "" {
			row.UntrustedSources = []byte(sources.String)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// StepLatencyP95ByStep returns step-duration p95 (seconds) + count per
// (project, workflow, step, role, model) over rows recorded at/after since —
// the control-plane latency signal's slowest-step attribution. Mirrors the
// Postgres backend; p95 computed in Go (persistence.P95Seconds).
func (r *ExecutionStepOutcomeRepository) StepLatencyP95ByStep(ctx context.Context, since time.Time) ([]persistence.StepLatencyStat, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT o.project_id, e.workflow_id, o.step_id, o.role, o.model, o.duration_ms, o.outcome
		FROM execution_step_outcomes o
		JOIN executions e ON e.id = o.execution_id
		WHERE o.recorded_at >= ? AND o.duration_ms IS NOT NULL AND o.duration_ms >= 0`, sqliteTime(since.UTC()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanStepLatencyRows(rows)
}

// scanStepLatencyRows folds (project, workflow, step, role, model, duration_ms)
// rows into per-key p95 stats.
func scanStepLatencyRows(rows *sql.Rows) ([]persistence.StepLatencyStat, error) {
	type key struct{ project, workflow, step, role, model string }
	byKey := make(map[key][]float64)
	degraded := make(map[key]int64)
	timeouts := make(map[key]int64)
	for rows.Next() {
		var k key
		var durMs int64
		var outcome string
		if err := rows.Scan(&k.project, &k.workflow, &k.step, &k.role, &k.model, &durMs, &outcome); err != nil {
			return nil, err
		}
		byKey[k] = append(byKey[k], float64(durMs)/1000.0)
		if persistence.IsDegradedStepOutcome(outcome) {
			degraded[k]++
		}
		if persistence.IsTimeoutStepOutcome(outcome) {
			timeouts[k]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]persistence.StepLatencyStat, 0, len(byKey))
	for k, durs := range byKey {
		out = append(out, persistence.StepLatencyStat{
			ProjectID: k.project, WorkflowID: k.workflow, StepID: k.step,
			Role: k.role, Model: k.model,
			P95Seconds: persistence.P95Seconds(durs), Count: int64(len(durs)),
			MaxSeconds: persistence.MaxSeconds(durs), DegradedCount: degraded[k],
			TimeoutCount: timeouts[k],
		})
	}
	return out, nil
}
