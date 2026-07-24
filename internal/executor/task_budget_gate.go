package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
)

// budgetCheckpointDecisionKind is the discriminator stamped on a budget
// checkpoint's metadata (decision.kind == "budget") so the UI / API / metrics
// distinguish a per-task-budget park from a lead-handoff decision question
// (LLD 2026-07-24 §3.6). The metadata rides the same task_messages checkpoint
// shape as a lead handoff, reusing the existing TriggerOperatorAnswer resume.
const budgetCheckpointDecisionKind = "budget"

// enforceTaskBudget is the step-boundary gate for the per-task cost governor
// (LLD 2026-07-24 §3.3). It is called once per agent-step dispatch, before the
// step runs, at the single-threaded-per-task boundary where the just-completed
// step's usage rows are already committed (a race-free cumulative read).
//
// Returns:
//   - nil                → TierOK or TierSoft: dispatch the step. (Soft also
//     emits a warn-only Notifier event + log, deduped once per task, plus the
//     non-deduped metric.)
//   - errLeadHandoff     → TierHard OR a fail-closed Check error: the task has
//     been parked AWAITING_INPUT with a budget `decision` checkpoint and MUST
//     NOT dispatch. The caller returns this sentinel; runExecution treats it
//     exactly like every other lead handoff (records completion, no COMPLETED
//     overwrite, finalization).
//   - other error        → an unexpected park-write failure; the caller fails
//     the step.
//
// The governor is a no-op (returns nil) when no usage repo is wired or the
// effective per-task budget is 0 (disabled) — byte-identical to today's
// behaviour for un-configured projects.
func (e *Executor) enforceTaskBudget(ctx context.Context, task *persistence.Task, execution *persistence.Execution, plan *executionPlan, stepID string) error {
	if e == nil || e.llmUsageRepo == nil || task == nil || plan == nil {
		return nil
	}
	project := plan.project
	// Fast path: governor disabled for this task ⇒ skip the spend read.
	if budget.EffectiveTaskBudgetUSD(project, task.BudgetUSD) <= 0 {
		return nil
	}

	gov := budget.NewTaskGovernor(e.llmUsageRepo)
	dec, err := gov.Check(ctx, project, task)
	if err != nil {
		// FAIL-CLOSED (§3.3): a cost-read error must not let a runaway slip
		// past. Park with a DISTINCT reason so the operator can tell a
		// transient DB blip apart from a real breach. Not a real tier — count
		// it under "hard" occupancy for time-in-tier, but the checkpoint text
		// and log say "budget check unavailable".
		e.recordTaskBudgetTier(task.ProjectID, budget.TierHard)
		e.logger.Warn().
			Err(err).
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Str("step_id", stepID).
			Msg("task budget check unavailable — failing closed, parking AWAITING_INPUT")
		if perr := e.parkForBudget(ctx, task, execution, dec, true); perr != nil {
			return perr
		}
		return errLeadHandoff
	}

	// Metric fires on EVERY evaluation (ok/soft/hard), never deduped (§4).
	e.recordTaskBudgetTier(task.ProjectID, dec.Tier)

	switch dec.Tier {
	case budget.TierHard:
		e.logger.Warn().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Str("step_id", stepID).
			Float64("spent_usd", dec.SpentUSD).
			Float64("budget_usd", dec.BudgetUSD).
			Msg("task hit its per-task budget — parking AWAITING_INPUT")
		if perr := e.parkForBudget(ctx, task, execution, dec, false); perr != nil {
			return perr
		}
		return errLeadHandoff
	case budget.TierSoft:
		e.warnTaskBudgetSoftOnce(ctx, task, dec)
		return nil
	default:
		return nil
	}
}

// recordTaskBudgetTier increments the non-deduped tier metric.
func (e *Executor) recordTaskBudgetTier(projectID string, tier budget.TaskBudgetTier) {
	if e.metrics == nil || e.metrics.TaskBudgetTierTotal == nil {
		return
	}
	e.metrics.TaskBudgetTierTotal.WithLabelValues(projectID, tier.String()).Inc()
}

// warnTaskBudgetSoftOnce emits the soft-breach Notifier event + structured log,
// deduped once per task_id per daemon lifetime (§3.5). The metric is handled
// separately (not deduped) by the caller.
//
// M2 (impl review): the design's "reset the dedup flag on top-up" is best-effort
// only — the top-up now happens in the API/UI answer handler (§3.6), which
// cannot reach this executor's in-memory map. So a re-warn after a top-up is not
// guaranteed within a single daemon lifetime; a daemon restart or the task
// reaching terminal (settleBudgetReservation → Delete, M1) clears the flag. The
// non-deduped metric (§4) remains the durable time-in-tier record.
func (e *Executor) warnTaskBudgetSoftOnce(ctx context.Context, task *persistence.Task, dec budget.TaskBudgetDecision) {
	if _, seen := e.taskBudgetWarned.LoadOrStore(task.ID, struct{}{}); seen {
		return
	}
	e.logger.Warn().
		Str("task_id", task.ID).
		Str("project_id", task.ProjectID).
		Float64("spent_usd", dec.SpentUSD).
		Float64("budget_usd", dec.BudgetUSD).
		Float64("fraction", dec.Fraction).
		Msg("task crossed its per-task soft budget threshold (warn-only)")
	if e.budgetNotifier != nil {
		// Reuse the project-budget Notifier surface; "soft"/"task" reads as a
		// per-task soft breach. Decision carries the human-readable reason.
		e.budgetNotifier.NotifyBudgetBreach(ctx, task.ProjectID, "soft", "task", budget.Decision{
			SoftBreached: true,
			Reason: fmt.Sprintf("task %s spent $%.2f of its $%.2f per-task budget (%.0f%%)",
				task.ID, dec.SpentUSD, dec.BudgetUSD, dec.Fraction*100),
		})
	}
}

// parkForBudget writes a budget `decision` checkpoint task_message and
// transitions the task RUNNING/LEASED → AWAITING_INPUT, reusing the
// lead_handoff seam (§3.6). failClosed distinguishes a real breach from a
// "budget check unavailable" fail-closed park.
//
// After the top-up the operator resumes via the admin budget endpoint
// (RaiseTaskBudget resume mode), which RE-RUNS the task from the entrypoint —
// there is no step replay — so cost is cumulative across re-runs. The
// checkpoint text says so: a re-trip means reduce-scope/abandon, not another
// top-up.
func (e *Executor) parkForBudget(ctx context.Context, task *persistence.Task, execution *persistence.Execution, dec budget.TaskBudgetDecision, failClosed bool) error {
	if e.taskMessageRepo == nil || e.persistTaskRepo == nil {
		// Without the conversational repos we can't park cleanly; surface an
		// error so the caller fails the step rather than silently dispatching.
		return fmt.Errorf("cannot park for budget: conversational repos not wired")
	}

	var question string
	reason := "budget"
	if failClosed {
		question = "The per-task cost check is unavailable (transient error), so this task was paused fail-closed to avoid uncapped spend. Resume it to retry, or cancel."
		reason = "budget_check_unavailable"
	} else {
		question = fmt.Sprintf(
			"This task reached its per-task budget: spent $%.2f of $%.2f. Resuming RE-RUNS the task from the start (cost is cumulative across re-runs), so a top-up must give headroom for the whole re-run — if it re-trips immediately, reduce scope or abandon rather than topping up again.",
			dec.SpentUSD, dec.BudgetUSD,
		)
	}

	meta, _ := json.Marshal(map[string]any{
		"kind": "decision",
		"decision": map[string]any{
			"kind":        budgetCheckpointDecisionKind,
			"reason":      reason,
			"spent_usd":   dec.SpentUSD,
			"budget_usd":  dec.BudgetUSD,
			"fail_closed": failClosed,
		},
		"question": question,
		"options": []map[string]any{
			{"id": "increase", "label": "Increase budget & resume"},
			{"id": "reduce_scope", "label": "Reduce scope & resume"},
			{"id": "abandon", "label": "Abandon (cancel)"},
		},
	})

	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		ExecutionID: &execution.ID,
		AuthorKind:  persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     question,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := e.taskMessageRepo.Insert(ctx, msg); err != nil {
		return fmt.Errorf("insert budget checkpoint: %w", err)
	}

	ok, err := e.persistTaskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusLeased},
		persistence.TaskStatusAwaitingInput,
		persistence.TransitionOpts{ClearLease: true},
	)
	if err != nil {
		return fmt.Errorf("transition to AWAITING_INPUT for budget: %w", err)
	}
	if !ok {
		// Task drifted (cancel race) — the checkpoint is written, the operator
		// can still see it. Log and proceed (treated as handoff by the caller).
		e.logger.Warn().
			Str("task_id", task.ID).
			Str("execution_id", execution.ID).
			Msg("budget checkpoint emitted but task drifted out of RUNNING — transition no-op")
		return nil
	}
	task.Status = persistence.TaskStatusAwaitingInput
	e.notifySteering(ctx, task, string(persistence.TaskStatusAwaitingInput))
	e.emitPaused(ctx, execution.ID, "awaiting_input")

	// M3 (impl review): the hard-park Notifier fires on EACH park, not once per
	// task. This is intentional — a re-park after a top-up is a new operator-
	// actionable event (the task re-tripped the raised ceiling), so alerting
	// again is more useful than suppressing it. Soft breaches stay deduped.
	if e.budgetNotifier != nil {
		e.budgetNotifier.NotifyBudgetBreach(ctx, task.ProjectID, "hard", "task", budget.Decision{
			Blocked: true,
			Reason: fmt.Sprintf("task %s parked: spent $%.2f of its $%.2f per-task budget",
				task.ID, dec.SpentUSD, dec.BudgetUSD),
		})
	}
	return nil
}
