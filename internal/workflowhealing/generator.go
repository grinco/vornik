package workflowhealing

import (
	"errors"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// Deterministic-recipe live generation, part 2 — step selection + the
// retry-budget candidate builder.
//
// A healing trigger names a workflow and its evidence executions but NOT the
// offending step. SelectOffendingStep infers it from per-step failure tallies
// (the caller computes these from execution_step_outcomes over the trigger's
// EvidenceExecutionIDs). BuildRetryBudgetCandidate then runs the retry-budget
// recipe on that step and synthesizes a promotable proposal+candidate, or
// returns ErrNoRecipeApplies so the caller falls back to the LLM architect.
//
// Everything here is pure (no DB): the I/O — loading the baseline genome and
// tallying outcomes — lives in the caller, keeping the heuristic unit-testable.

// ErrNoRecipeApplies signals that no deterministic recipe could produce a
// candidate for this trigger/evidence. It is the EXPECTED, non-error fallback
// path (offending step unidentifiable, nothing to lower, recipe no-op, or the
// recipe produced an invalid genome): the caller defers to the architect.
var ErrNoRecipeApplies = errors.New("workflowhealing: no deterministic recipe applies")

// FailuresByStep tallies failure outcomes per step from a set of outcome
// rows: a failure is a TERMINAL (finalized) outcome that is not `ok` and not
// one of the audit-companion labels (`superseded`, `verifier_warn`). Pure —
// the caller fetches the rows for a trigger's evidence executions and passes
// them here, so the heuristic stays unit-testable without a repository.
func FailuresByStep(rows []*persistence.ExecutionStepOutcome) map[string]int {
	failures := map[string]int{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		o := stepoutcome.Outcome(row.Outcome)
		if o.IsTerminal() && o != stepoutcome.OK && o != stepoutcome.Superseded && o != stepoutcome.VerifierWarn {
			failures[row.StepID]++
		}
	}
	return failures
}

// VerifierFailuresByStep tallies advisory verifier violations
// (outcome=verifier_warn) per step across a set of outcome rows. This is
// the signal the verifier-insertion recipe keys on — a step whose output
// repeatedly trips the warn-tier verifier is a candidate for an explicit
// verification checkpoint. Pure (caller supplies the rows).
func VerifierFailuresByStep(rows []*persistence.ExecutionStepOutcome) map[string]int {
	failures := map[string]int{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		if stepoutcome.Outcome(row.Outcome) == stepoutcome.VerifierWarn {
			failures[row.StepID]++
		}
	}
	return failures
}

// BuildVerifierInsertionCandidate selects the step with the most verifier
// violations and inserts an explicit verifier step after it (gating its
// on_success), synthesizing a pending WorkflowProposal + linked
// HealingCandidate via VerifierInsertionRecipe. Returns ErrNoRecipeApplies
// whenever the recipe can't/shouldn't produce a candidate (no offending
// step, anchor has no on_success, verifier role absent from the genome so
// validation fails, etc.) so the caller falls back to the retry-budget
// recipe or the architect. The candidate is a PROPOSAL — it goes through
// the trial + operator-approval flow before promotion, never auto-applied.
func BuildVerifierInsertionCandidate(baseline *registry.Workflow, trigger *persistence.HealingTrigger, verifierFailuresByStep map[string]int, verifierRole string, now time.Time) (*persistence.WorkflowProposal, *persistence.HealingCandidate, error) {
	if baseline == nil || trigger == nil || verifierRole == "" {
		return nil, nil, ErrNoRecipeApplies
	}
	stepID, ok := SelectOffendingStep(verifierFailuresByStep)
	if !ok {
		return nil, nil, ErrNoRecipeApplies
	}
	res, err := VerifierInsertionRecipe(baseline, stepID, verifierRole, "", trigger.EvidenceExecutionIDs)
	if err != nil {
		// ErrRecipeStepNotFound / ErrRecipeNoChange (no on_success to gate) /
		// verifier-id clash → architect (or retry-budget) handles it.
		return nil, nil, ErrNoRecipeApplies
	}
	if !res.Valid {
		// A structurally-derived genome that fails WORKFLOW.md validation
		// (e.g. the verifier role isn't declared in the swarm) must not be
		// promoted; defer rather than ship a broken candidate.
		return nil, nil, ErrNoRecipeApplies
	}
	proposal := ProposalFromRecipeResult(baseline.ID, res, now)
	candidate := CandidateFromRecipeResult(trigger, proposal, res)
	return proposal, candidate, nil
}

// SelectOffendingStep returns the step id with the most failure outcomes
// across the trigger's evidence executions. Ties break alphabetically so the
// choice is deterministic regardless of map iteration order. ok is false when
// the tally is empty or every step has zero failures.
func SelectOffendingStep(failuresByStep map[string]int) (string, bool) {
	best := ""
	bestN := 0
	for step, n := range failuresByStep {
		if n <= 0 {
			continue
		}
		if n > bestN || (n == bestN && step < best) {
			best, bestN = step, n
		}
	}
	return best, bestN > 0
}

// BuildRetryBudgetCandidate selects the offending step from failuresByStep,
// halves its retry budget (lower-only; floors at 0), ensures it routes to a
// FAILED terminal on exhaustion, and synthesizes a pending WorkflowProposal +
// linked HealingCandidate via the recipe. Returns ErrNoRecipeApplies whenever
// the recipe cannot or should not produce a candidate, so the caller falls
// back to the architect.
func BuildRetryBudgetCandidate(baseline *registry.Workflow, trigger *persistence.HealingTrigger, failuresByStep map[string]int, now time.Time) (*persistence.WorkflowProposal, *persistence.HealingCandidate, error) {
	if baseline == nil || trigger == nil {
		return nil, nil, ErrNoRecipeApplies
	}
	stepID, ok := SelectOffendingStep(failuresByStep)
	if !ok {
		return nil, nil, ErrNoRecipeApplies
	}
	step, exists := baseline.Steps[stepID]
	if !exists || step.RetryPolicy.MaxRetries <= 0 {
		// No step or nothing to lower — defer to the architect.
		return nil, nil, ErrNoRecipeApplies
	}

	// Halve the budget (lower-only; 1 → 0). The recipe re-clamps and refuses
	// to raise, so this is safe even if the genome shifts under us.
	newBudget := step.RetryPolicy.MaxRetries / 2
	// Ensure an exhausted retry routes somewhere terminal. Only supply a
	// target when the step lacks an on_fail; the recipe never overwrites an
	// existing one.
	onFail := step.OnFail
	if onFail == "" {
		onFail = firstFailedTerminal(baseline)
	}

	res, err := RetryBudgetRecipe(baseline, stepID, newBudget, onFail, trigger.EvidenceExecutionIDs)
	if err != nil {
		// ErrRecipeNoChange / ErrRecipeStepNotFound / invalid on_fail target —
		// all map to "architect handles it".
		return nil, nil, ErrNoRecipeApplies
	}
	if !res.Valid {
		// A structurally-derived genome that fails WORKFLOW.md validation must
		// not be promoted; defer to the architect rather than ship a broken
		// candidate.
		return nil, nil, ErrNoRecipeApplies
	}

	proposal := ProposalFromRecipeResult(baseline.ID, res, now)
	candidate := CandidateFromRecipeResult(trigger, proposal, res)
	return proposal, candidate, nil
}

// firstFailedTerminal returns the alphabetically-first terminal whose status
// is FAILED, or "" when the workflow declares none. Deterministic so the
// recipe's on_fail target is reproducible.
func firstFailedTerminal(wf *registry.Workflow) string {
	names := make([]string, 0, len(wf.Terminals))
	for name, term := range wf.Terminals {
		if term.Status == "FAILED" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}
