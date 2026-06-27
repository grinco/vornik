package executor

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// forwardPendingRecovery moves a prior step's stashed failure context
// (state.PendingRecovery, set by the on_fail handler) onto the next
// step's agent input and clears it, then attaches the advisory
// learned-remediation overlay (Consumer A).
//
// It exists so the AGENT-step recover path reaches the same instinct
// surface as the plan-step recover path (executePlanStep, plan_step.go):
// before this, attachLearnedRemediations was only called for type:plan
// recover steps, so an agent-type recover step — e.g. dev-pipeline's
// `recover-checkpoint` (type:agent, role:analyst) — forwarded the base
// RecoveryContext but never got the overlay.
//
// Returns true when a pending recovery was forwarded (so the caller can
// log it), false when there was nothing to forward. attachLearnedRemediations
// is itself gated (failure_playbooks) and fail-soft, so this is a no-op
// overlay when the gate is off or no instinct matches — the forwarded
// base context is byte-for-byte unchanged in that case.
func (e *Executor) forwardPendingRecovery(ctx context.Context, task *persistence.Task, execution *persistence.Execution, state *executionState, opts *agentInputOpts) bool {
	if state == nil || state.PendingRecovery == nil || opts == nil {
		return false
	}
	opts.RecoveryContext = state.PendingRecovery
	state.PendingRecovery = nil
	e.attachLearnedRemediations(ctx, task, execution, opts.RecoveryContext)
	return true
}
