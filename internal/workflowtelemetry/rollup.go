// Package workflowtelemetry computes per-workflow execution-evidence
// rollups for the self-evolving-workflows arc
// (https://docs.vornik.io). The
// rollup is what the architect agent (Slice 2) reads as its primary
// input; the vornikctl workflow-stats CLI (Slice 1) renders the same
// data human-readably so operators can sanity-check the architect's
// reasoning before approving any proposal.
//
// Design notes:
//
//   - Daemon-wide scope (not project-scoped). The architect proposes
//     workflow YAML edits that affect every project using the
//     workflow, so the rollup MUST aggregate across projects. A
//     project-scoped variant can ship as a future operator-debug
//     surface.
//
//   - Token-budget friendly. A 50-run rollup hits 3-4k tokens when
//     rendered into the architect prompt — well under any context
//     window. We deliberately avoid drilling to per-row detail; the
//     architect can request specific runs via the evidence_run_ids
//     it ultimately cites.
//
//   - 9 telemetry tables touched (per the 2026-05-25 audit):
//     executions, execution_step_outcomes, task_llm_usage,
//     tool_audit_log, task_judge_verdicts, task_messages,
//     memory_retrieval_audit, autonomy_evaluations, profile_use_audit.
//     v1 reads 6 of them (the load-bearing ones); audit-flagged
//     gaps stay v1.1 follow-ons.
package workflowtelemetry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Rollup is the structured summary returned by Service.ForWorkflow.
// Field shapes mirror the design doc's Rollup struct verbatim so the
// architect-prompt schema doesn't drift from the persistence layer.
type Rollup struct {
	WorkflowID     string    `json:"workflow_id"`
	WindowStart    time.Time `json:"window_start"`
	WindowEnd      time.Time `json:"window_end"`
	RunCount       int       `json:"run_count"`
	SuccessCount   int       `json:"success_count"`
	FailureCount   int       `json:"failure_count"`
	CancelledCount int       `json:"cancelled_count"`

	// Per-step distribution, ordered by first appearance in the
	// workflow's execution graph. Operators read this top-to-bottom
	// to match the workflow's authored order.
	Steps []StepRollup `json:"steps"`

	// Aggregate cost + duration across every run in the window.
	// 0 when RunCount is 0 (avoid NaN downstream).
	AvgCostUSD         float64 `json:"avg_cost_usd"`
	AvgDurationSeconds float64 `json:"avg_duration_seconds"`

	// Quality signals. Empty map / 0 rate when the corresponding
	// detector wasn't wired for any run in the window.
	JudgeVerdictDist         map[string]int `json:"judge_verdict_dist"`
	HallucinationRate        float64        `json:"hallucination_rate"`
	OperatorInterventionRate float64        `json:"operator_intervention_rate"`

	// Top failure modes — error_class → count, sorted desc, capped
	// at 10. The architect reads this to find recurring failure
	// classes that might warrant a structural change (e.g. adding a
	// verifier step before a step that keeps emitting schema_violation).
	TopFailureClasses []FailureClassCount `json:"top_failure_classes"`
}

// StepRollup carries per-step metrics aggregated across every run
// of the workflow in the window. Steps appear in the order they
// were first observed (typically matches the workflow YAML order
// but doesn't enforce it — handles workflows where step order
// varies across runs).
type StepRollup struct {
	StepID             string         `json:"step_id"`
	Role               string         `json:"role"`
	Model              string         `json:"model"`
	OutcomeDist        map[string]int `json:"outcome_dist"`
	AvgCostUSD         float64        `json:"avg_cost_usd"`
	AvgDurationSeconds float64        `json:"avg_duration_seconds"`
	TopErrorClass      string         `json:"top_error_class,omitempty"`
}

// FailureClassCount is one entry in Rollup.TopFailureClasses.
type FailureClassCount struct {
	ErrorClass string `json:"error_class"`
	Count      int    `json:"count"`
}

// Service computes rollups against the daemon's database. Constructed
// with a *sql.DB (or anything implementing persistence.DBTX); zero-
// value Service is unusable.
type Service struct {
	db persistence.DBTX
}

// NewService binds the service to a database connection. The
// concrete *sql.DB the rest of the daemon uses satisfies DBTX, so
// callers don't need a separate adapter.
func NewService(db persistence.DBTX) *Service {
	return &Service{db: db}
}

// ErrInvalidWorkflowID is returned when the caller passes an empty
// workflow_id. Caught early so the SQL doesn't run with a wildcard.
var ErrInvalidWorkflowID = errors.New("workflowtelemetry: workflow_id is required")

// ForWorkflow returns the rollup for `workflowID` over the window
// [since, now). Returns an empty rollup (RunCount=0, empty Steps)
// when no executions match — NOT an error. The architect must
// handle the no-data case gracefully.
//
// Implementation notes: six SQL round-trips total, each scoped to
// the workflow via executions.workflow_id (or tasks.workflow_id for
// the judge/messages sources). All queries respect the supplied
// time window so the rollup is stable for the architect's reasoning
// across re-runs.
func (s *Service) ForWorkflow(ctx context.Context, workflowID string, since time.Time) (*Rollup, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("workflowtelemetry: service not configured")
	}
	if workflowID == "" {
		return nil, ErrInvalidWorkflowID
	}

	now := time.Now().UTC()
	out := &Rollup{
		WorkflowID:        workflowID,
		WindowStart:       since.UTC(),
		WindowEnd:         now,
		JudgeVerdictDist:  map[string]int{},
		Steps:             []StepRollup{},
		TopFailureClasses: []FailureClassCount{},
	}

	// 1. Execution counts by status (the load-bearing query — drives
	// every rate calc below). Skip when zero — RunCount remains 0
	// and the rest of the queries cheaply return nothing.
	if err := s.fillExecutionCounts(ctx, out, workflowID, since); err != nil {
		return nil, fmt.Errorf("execution counts: %w", err)
	}
	if out.RunCount == 0 {
		return out, nil
	}

	// 2. Avg cost + duration. Cost comes from task_llm_usage
	// (source='workflow_step' only); duration from executions
	// where completed_at is non-null.
	if err := s.fillAggregates(ctx, out, workflowID, since); err != nil {
		return nil, fmt.Errorf("aggregates: %w", err)
	}

	// 3. Per-step outcome distribution + per-step cost/duration.
	if err := s.fillStepRollups(ctx, out, workflowID, since); err != nil {
		return nil, fmt.Errorf("step rollups: %w", err)
	}

	// 4. Top failure classes across all steps.
	if err := s.fillTopFailureClasses(ctx, out, workflowID, since); err != nil {
		return nil, fmt.Errorf("failure classes: %w", err)
	}

	// 5. Judge verdict distribution. Tolerates absent table (judge
	// disabled deployments) — empty map is the natural shape.
	if err := s.fillJudgeVerdicts(ctx, out, workflowID, since); err != nil {
		return nil, fmt.Errorf("judge verdicts: %w", err)
	}

	// 6. Quality rates: hallucination + operator intervention. Both
	// returned as fractions of RunCount.
	if err := s.fillQualityRates(ctx, out, workflowID, since); err != nil {
		return nil, fmt.Errorf("quality rates: %w", err)
	}

	return out, nil
}

// fillExecutionCounts populates RunCount + SuccessCount + FailureCount
// + CancelledCount from `executions` filtered by workflow_id + window.
// Counts every status; "RunCount" is the total of every row.
func (s *Service) fillExecutionCounts(ctx context.Context, out *Rollup, workflowID string, since time.Time) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, count(*)
		FROM executions
		WHERE workflow_id = $1 AND created_at >= $2
		GROUP BY status`, workflowID, since)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return err
		}
		out.RunCount += n
		switch status {
		case "COMPLETED":
			out.SuccessCount += n
		case "FAILED":
			out.FailureCount += n
		case "CANCELLED":
			out.CancelledCount += n
		}
	}
	return rows.Err()
}

// fillAggregates populates AvgCostUSD + AvgDurationSeconds. Cost
// only sums source='workflow_step' rows so dispatcher/judge spend
// doesn't contaminate the per-workflow figure.
func (s *Service) fillAggregates(ctx context.Context, out *Rollup, workflowID string, since time.Time) error {
	var totalCost sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(u.cost_usd), 0)
		FROM task_llm_usage u
		JOIN executions e ON e.id = u.execution_id
		WHERE u.source = 'workflow_step'
		  AND e.workflow_id = $1
		  AND e.created_at >= $2`, workflowID, since).Scan(&totalCost); err != nil {
		return err
	}
	if out.RunCount > 0 {
		out.AvgCostUSD = totalCost.Float64 / float64(out.RunCount)
	}

	var avgDuration sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT AVG(EXTRACT(EPOCH FROM (completed_at - started_at)))
		FROM executions
		WHERE workflow_id = $1
		  AND created_at >= $2
		  AND completed_at IS NOT NULL
		  AND started_at IS NOT NULL`, workflowID, since).Scan(&avgDuration); err != nil {
		return err
	}
	if avgDuration.Valid {
		out.AvgDurationSeconds = avgDuration.Float64
	}
	return nil
}

// fillStepRollups groups execution_step_outcomes rows by
// (step_id, role, model), then nests outcome counts inside each
// step. Ordering by min(recorded_at) per step gives a stable
// step order that matches the workflow's execution graph in the
// common case.
func (s *Service) fillStepRollups(ctx context.Context, out *Rollup, workflowID string, since time.Time) error {
	// First pass: pull outcome counts + first-seen-at per
	// (step_id, role, model, outcome). Compose into StepRollups.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		    so.step_id,
		    so.role,
		    so.model,
		    so.outcome,
		    count(*) AS n,
		    MIN(so.recorded_at) AS first_seen
		FROM execution_step_outcomes so
		JOIN executions e ON e.id = so.execution_id
		WHERE e.workflow_id = $1 AND e.created_at >= $2
		GROUP BY so.step_id, so.role, so.model, so.outcome
		ORDER BY MIN(so.recorded_at) ASC`, workflowID, since)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	stepIdx := map[string]int{}
	for rows.Next() {
		var stepID, role, model, outcome string
		var n int
		var firstSeen time.Time
		if err := rows.Scan(&stepID, &role, &model, &outcome, &n, &firstSeen); err != nil {
			return err
		}
		key := stepID + "|" + role + "|" + model
		i, ok := stepIdx[key]
		if !ok {
			i = len(out.Steps)
			stepIdx[key] = i
			out.Steps = append(out.Steps, StepRollup{
				StepID:      stepID,
				Role:        role,
				Model:       model,
				OutcomeDist: map[string]int{},
			})
		}
		out.Steps[i].OutcomeDist[outcome] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Second pass: per-step duration + cost. One query, joined into
	// the steps we just built. Steps with no LLM usage rows (e.g.
	// gate-only steps) keep AvgCostUSD = 0.
	dRows, err := s.db.QueryContext(ctx, `
		SELECT
		    so.step_id,
		    so.role,
		    so.model,
		    AVG(so.duration_ms)::float8 / 1000 AS avg_seconds,
		    COALESCE(SUM(u.cost_usd), 0)::float8 AS total_cost,
		    COUNT(DISTINCT so.id) AS step_runs
		FROM execution_step_outcomes so
		JOIN executions e ON e.id = so.execution_id
		LEFT JOIN task_llm_usage u
		    ON u.execution_id = so.execution_id
		   AND u.step_id = so.step_id
		   AND u.source = 'workflow_step'
		WHERE e.workflow_id = $1 AND e.created_at >= $2
		GROUP BY so.step_id, so.role, so.model`, workflowID, since)
	if err != nil {
		return err
	}
	defer func() { _ = dRows.Close() }()
	for dRows.Next() {
		var stepID, role, model string
		var avgSec sql.NullFloat64
		var totalCost float64
		var stepRuns int
		if err := dRows.Scan(&stepID, &role, &model, &avgSec, &totalCost, &stepRuns); err != nil {
			return err
		}
		key := stepID + "|" + role + "|" + model
		i, ok := stepIdx[key]
		if !ok {
			continue
		}
		if avgSec.Valid {
			out.Steps[i].AvgDurationSeconds = avgSec.Float64
		}
		if stepRuns > 0 {
			out.Steps[i].AvgCostUSD = totalCost / float64(stepRuns)
		}
	}
	if err := dRows.Err(); err != nil {
		return err
	}

	// Third pass: per-step top error class. Skipped when the step
	// has no error rows. Tolerates the empty-string error_class
	// (which means "no error" on a successful row).
	eRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (so.step_id, so.role, so.model)
		    so.step_id,
		    so.role,
		    so.model,
		    so.error_class
		FROM execution_step_outcomes so
		JOIN executions e ON e.id = so.execution_id
		WHERE e.workflow_id = $1 AND e.created_at >= $2 AND so.error_class != ''
		GROUP BY so.step_id, so.role, so.model, so.error_class
		ORDER BY so.step_id, so.role, so.model, count(*) DESC`, workflowID, since)
	if err != nil {
		return err
	}
	defer func() { _ = eRows.Close() }()
	for eRows.Next() {
		var stepID, role, model, errClass string
		if err := eRows.Scan(&stepID, &role, &model, &errClass); err != nil {
			return err
		}
		key := stepID + "|" + role + "|" + model
		if i, ok := stepIdx[key]; ok {
			out.Steps[i].TopErrorClass = errClass
		}
	}
	return eRows.Err()
}

// fillTopFailureClasses populates Rollup.TopFailureClasses — the
// global failure-class distribution across every step in every run.
// Capped at 10 entries.
func (s *Service) fillTopFailureClasses(ctx context.Context, out *Rollup, workflowID string, since time.Time) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT so.error_class, count(*)
		FROM execution_step_outcomes so
		JOIN executions e ON e.id = so.execution_id
		WHERE e.workflow_id = $1
		  AND e.created_at >= $2
		  AND so.error_class != ''
		GROUP BY so.error_class
		ORDER BY count(*) DESC
		LIMIT 10`, workflowID, since)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var fc FailureClassCount
		if err := rows.Scan(&fc.ErrorClass, &fc.Count); err != nil {
			return err
		}
		out.TopFailureClasses = append(out.TopFailureClasses, fc)
	}
	return rows.Err()
}

// fillJudgeVerdicts populates Rollup.JudgeVerdictDist from
// task_judge_verdicts. Joined to tasks (which has workflow_id)
// rather than executions because judge verdicts are per-task,
// not per-execution. Tolerates the absent-judge-rows case.
func (s *Service) fillJudgeVerdicts(ctx context.Context, out *Rollup, workflowID string, since time.Time) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.verdict, count(*)
		FROM task_judge_verdicts v
		JOIN tasks t ON t.id = v.task_id
		WHERE t.workflow_id = $1 AND t.created_at >= $2
		GROUP BY v.verdict`, workflowID, since)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var verdict string
		var n int
		if err := rows.Scan(&verdict, &n); err != nil {
			return err
		}
		out.JudgeVerdictDist[verdict] = n
	}
	return rows.Err()
}

// fillQualityRates populates HallucinationRate and
// OperatorInterventionRate. Each is a fraction of RunCount —
// what fraction of runs had at least one hallucination signal
// or operator-directive message respectively.
func (s *Service) fillQualityRates(ctx context.Context, out *Rollup, workflowID string, since time.Time) error {
	if out.RunCount == 0 {
		return nil
	}
	var hallucinatedRuns int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT so.execution_id)
		FROM execution_step_outcomes so
		JOIN executions e ON e.id = so.execution_id
		WHERE e.workflow_id = $1
		  AND e.created_at >= $2
		  AND so.hallucination_signals IS NOT NULL
		  AND jsonb_typeof(so.hallucination_signals) = 'array'
		  AND jsonb_array_length(so.hallucination_signals) > 0`,
		workflowID, since).Scan(&hallucinatedRuns); err != nil {
		return err
	}
	out.HallucinationRate = float64(hallucinatedRuns) / float64(out.RunCount)

	var interventionRuns int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT m.task_id)
		FROM task_messages m
		JOIN tasks t ON t.id = m.task_id
		WHERE t.workflow_id = $1
		  AND t.created_at >= $2
		  AND m.message_kind = 'directive'
		  AND m.author_kind = 'operator'`, workflowID, since).Scan(&interventionRuns); err != nil {
		return err
	}
	out.OperatorInterventionRate = float64(interventionRuns) / float64(out.RunCount)
	return nil
}
