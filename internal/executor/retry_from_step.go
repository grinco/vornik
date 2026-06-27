package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Retry-from-step errors. Surface them through the API so the operator
// gets a precise reason instead of a generic 500.
var (
	// ErrRetryNotTerminal — caller asked to retry an execution that
	// hasn't reached a terminal state. Retry-from-step is an
	// operator-driven correction of a finished run; replaying steps
	// of an in-flight execution would race with the executor's own
	// state machine.
	ErrRetryNotTerminal = errors.New("retry-from-step: execution is not in a terminal state")
	// ErrRetryStepNotInExecution — caller asked to retry from a step
	// the original run never reached. Defensive: prevents an
	// operator from typo'ing a step ID and starting a workflow at
	// an entrypoint that has no upstream context.
	ErrRetryStepNotInExecution = errors.New("retry-from-step: step is not in this execution's completed_steps")
	// ErrRetryAlreadyExecuting — task is already being executed by
	// this daemon. Either the operator double-clicked the button or
	// another retry is in flight. Caller should refresh the page.
	ErrRetryAlreadyExecuting = errors.New("retry-from-step: task is already being executed")
)

// RetryFromStep restarts an execution from the named step, reusing the
// existing execution row. The operator is asserting "the run was wrong
// from this step onward; redo it" — typically when a downstream step
// failed but the upstream work was still good enough to keep, or when
// a model gave a clearly broken output that an earlier step's input
// could now be re-tried against.
//
// What gets reset:
//   - completed_steps truncated to BEFORE stepID (the step itself is
//     about to be re-run, so it's no longer "completed")
//   - current_step_id set to stepID so the workflow loop picks up there
//   - VisitCounts and Iterations zeroed — the loop guard would
//     otherwise refuse to revisit a step that already consumed its
//     allotted visits during the original run
//   - PlanIndex / PlanSteps cleared if stepID is outside any plan
//     loop (we conservatively reject retries from inside a plan
//     for v1 — the plan substep machinery has more state we'd
//     need to coordinate)
//   - downstream outcomes (recorded_at strictly after the latest
//     surviving outcome) marked superseded so the dashboard's
//     quality view doesn't double-count them alongside the retry's
//     fresh outcomes
//
// What gets preserved:
//   - LastResult — the previous step's output, which step `stepID`
//     consumes as input
//   - the upstream outcome rows themselves (audit trail)
//   - artifacts produced by upstream steps
//
// Worktree: the original run, if it succeeded, merged its workspace
// changes back to main. A fresh worktree from main therefore
// already contains the upstream-step results, so the retry sees
// the same workspace state the original step `stepID` saw. If the
// original run FAILED before merging (worktree was discarded), the
// retry runs against the project workspace as it stands today —
// which may differ from what stepID originally saw, and the
// operator has to know that.
//
// The execution row is REUSED (not cloned). This preserves the URL,
// audit history, and avoids schema changes for parent_execution_id
// — but it does mean the original run's "completed at" timestamp
// gets overwritten when the retry finishes. v2 may move to a
// cloned-execution model once the operator UX justifies the
// schema work.
func (e *Executor) RetryFromStep(ctx context.Context, executionID, stepID string) (err error) {
	// Count every attempt by result (LLD §3.1.2 vornik_retry_from_step_total).
	// Deferred on the named return so the single classifier covers all of
	// RetryFromStep's exit paths instead of threading Inc() through each.
	defer func() { e.metrics.RecordRetryFromStep(retryFromStepResult(err)) }()

	if ctx == nil {
		ctx = context.Background()
	}

	exec, err := e.execRepo.Get(ctx, executionID)
	if err != nil {
		return fmt.Errorf("retry-from-step: load execution %s: %w", executionID, err)
	}
	if exec == nil {
		return fmt.Errorf("retry-from-step: execution %s not found", executionID)
	}

	switch exec.Status {
	case persistence.ExecutionStatusCompleted, persistence.ExecutionStatusFailed, persistence.ExecutionStatusCancelled:
		// All terminal — fine to retry.
	default:
		return fmt.Errorf("%w: status=%s", ErrRetryNotTerminal, exec.Status)
	}

	// stepID must be in the existing run's completed_steps. Truncate
	// the slice to everything strictly before the FIRST occurrence —
	// loops can have the same step appear multiple times; restarting
	// at the first occurrence is the safe choice (otherwise the
	// loop counter math gets confused).
	cutIdx := -1
	for i, s := range exec.CompletedSteps {
		if s == stepID {
			cutIdx = i
			break
		}
	}
	if cutIdx == -1 {
		return fmt.Errorf("%w: step=%q completed_steps=%v", ErrRetryStepNotInExecution, stepID, exec.CompletedSteps)
	}

	e.mu.Lock()
	if _, busy := e.activeExecutions[exec.TaskID]; busy {
		e.mu.Unlock()
		return ErrRetryAlreadyExecuting
	}
	e.mu.Unlock()

	task, err := e.taskRepo.Get(ctx, exec.TaskID)
	if err != nil {
		return fmt.Errorf("retry-from-step: load task %s: %w", exec.TaskID, err)
	}
	if task == nil {
		return fmt.Errorf("retry-from-step: task %s not found for execution %s", exec.TaskID, executionID)
	}

	// Compute the supersede cutoff BEFORE we mutate state. The cutoff
	// is the latest recorded_at among surviving outcomes — i.e. rows
	// whose step_id is in the truncated completed_steps slice. Rows
	// with recorded_at strictly after that get the "superseded" label.
	// If no upstream steps survived (cutIdx == 0, retrying from the
	// entrypoint), the cutoff is zero-time and every existing
	// outcome for this execution gets superseded.
	survivors := append([]string(nil), exec.CompletedSteps[:cutIdx]...)

	// Side-effect containment guard. The retry preserves survivors as
	// "done" and re-runs from stepID. Any survivor that produced EXTERNAL
	// side effects (a posted forge review, an indexed RAG batch, a spawned
	// callee task) will NOT be replayed — its effect already happened and
	// the re-run sees it as complete. Warn the operator + meter it so the
	// awareness the design doc promised ("the operator has to know that")
	// is actually surfaced, not just implied. Best-effort: never blocks.
	if seSteps := e.sideEffectingUpstreamSteps(exec, survivors); len(seSteps) > 0 {
		e.metrics.RecordRetryFromStepSideEffectingUpstream()
		e.logger.Warn().
			Str("execution_id", executionID).
			Str("task_id", exec.TaskID).
			Str("retry_step", stepID).
			Strs("side_effecting_upstream_steps", seSteps).
			Msg("retry-from-step: preserved upstream steps had external side effects (system/call_project) that will NOT be replayed — their effects already happened and may now be stale")
	}

	cutoff, supersedeErr := e.computeSupersedeCutoff(ctx, executionID, survivors)
	if supersedeErr != nil {
		// Non-fatal: the dashboard will continue showing the
		// original outcomes. Log and proceed — the retry itself is
		// the value, the audit relabel is a polish.
		e.logger.Warn().Err(supersedeErr).
			Str("execution_id", executionID).
			Msg("retry-from-step: failed to compute supersede cutoff — proceeding without relabel")
	} else if e.outcomeRepo != nil {
		if n, err := e.outcomeRepo.SupersedeAfter(ctx, executionID, cutoff); err != nil {
			e.logger.Warn().Err(err).
				Str("execution_id", executionID).
				Msg("retry-from-step: failed to mark downstream outcomes superseded — proceeding")
		} else if n > 0 {
			e.logger.Info().
				Int64("rows", n).
				Str("execution_id", executionID).
				Time("cutoff", cutoff).
				Msg("retry-from-step: marked downstream outcomes superseded")
		}
	}

	// Rewrite the persisted state. Reset the loop guard counters so
	// the executor doesn't immediately bail with "max visits exceeded"
	// on the re-run step.
	state := loadExecutionState(exec)
	state.CompletedSteps = survivors
	state.CurrentStepID = stepID
	state.VisitCounts = nil
	state.Iterations = 0
	state.PlanSteps = nil
	state.PlanIndex = 0
	state.PlanLeadMessage = ""
	state.PlanStartHEAD = ""
	state.ApprovalPendingStep = ""
	state.ApprovalGrantedStep = ""
	state.PausedReason = ""

	exec.CompletedSteps = survivors
	exec.CurrentStepID = &stepID
	exec.Status = persistence.ExecutionStatusRunning
	// Clear any prior terminal markers so the row reads as a fresh
	// in-flight run from the dashboard's perspective.
	exec.ErrorMessage = nil
	exec.ErrorCode = nil
	exec.CompletedAt = nil

	if err := e.saveExecutionState(ctx, exec, state); err != nil {
		return fmt.Errorf("retry-from-step: persist reset state: %w", err)
	}
	if err := e.execRepo.Update(ctx, exec); err != nil {
		return fmt.Errorf("retry-from-step: persist execution status: %w", err)
	}

	// Flip the task back to RUNNING so the executor loop sees a live
	// row. Don't bump task.Attempt — operator-initiated retries
	// shouldn't burn the autonomous retry budget. If the task was
	// already at MaxAttempts, we'd otherwise hit the runExecution
	// guard "attempts >= maxAttempts" and bail immediately.
	task.Status = persistence.TaskStatusRunning
	task.LastError = nil
	task.LastErrorClass = nil
	if err := e.taskRepo.Update(ctx, task); err != nil {
		return fmt.Errorf("retry-from-step: persist task status: %w", err)
	}

	e.logger.Info().
		Str("execution_id", executionID).
		Str("task_id", task.ID).
		Str("step_id", stepID).
		Strs("survivors", survivors).
		Msg("retry-from-step: state reset; relaunching execution")

	// Spawn the executor goroutine. recoverExecution does the
	// activeExecutions registration + goroutine wiring we need; it
	// already short-circuits if the task is already running, and
	// already validates the workflow drift case.
	return e.recoverExecution(ctx, exec)
}

// retryFromStepResult maps a RetryFromStep return error to the
// vornik_retry_from_step_total{result} label. The "refused_*" values
// are the operator-correctable cases (precise so a dashboard can split
// honest misuse from genuine failures); everything unclassified —
// load/persist/relaunch failures — folds into "error".
func retryFromStepResult(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case errors.Is(err, ErrRetryStepNotInExecution):
		return "refused_unknown_step"
	case errors.Is(err, ErrRetryNotTerminal), errors.Is(err, ErrRetryAlreadyExecuting):
		return "refused_bad_state"
	default:
		return "error"
	}
}

// sideEffectingUpstreamSteps returns the survivor (preserved-upstream) step
// IDs whose workflow step type produces external side effects a retry-from-step
// will not replay (see registry.WorkflowStep.HasExternalSideEffects). Resolves
// the workflow off the persisted exec.WorkflowID — best-effort and read-only:
// returns nil if the resolver or workflow is unavailable, so the advisory never
// blocks a retry.
func (e *Executor) sideEffectingUpstreamSteps(exec *persistence.Execution, survivors []string) []string {
	if exec == nil || len(survivors) == 0 || e.workflows == nil {
		return nil
	}
	wf := e.workflows.GetWorkflow(exec.WorkflowID)
	if wf == nil {
		return nil
	}
	var out []string
	for _, id := range survivors {
		if step, ok := wf.Steps[id]; ok && step.HasExternalSideEffects() {
			out = append(out, id)
		}
	}
	return out
}

// computeSupersedeCutoff returns the recorded_at to use as the
// "everything after this gets superseded" cut. The cutoff is the
// max recorded_at among outcomes whose step_id is in `survivors`
// (the truncated completed_steps). When survivors is empty (retrying
// from the entrypoint), the zero time is returned so every existing
// outcome for the execution falls past the cutoff.
func (e *Executor) computeSupersedeCutoff(ctx context.Context, executionID string, survivors []string) (time.Time, error) {
	if e.outcomeRepo == nil || len(survivors) == 0 {
		return time.Time{}, nil
	}
	rows, err := e.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &executionID,
		PageSize:    500,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("list outcomes for cutoff: %w", err)
	}
	survivorSet := make(map[string]struct{}, len(survivors))
	for _, s := range survivors {
		survivorSet[s] = struct{}{}
	}
	var cutoff time.Time
	for _, r := range rows {
		if r == nil {
			continue
		}
		if _, ok := survivorSet[r.StepID]; !ok {
			continue
		}
		if r.RecordedAt.After(cutoff) {
			cutoff = r.RecordedAt
		}
	}
	return cutoff, nil
}
