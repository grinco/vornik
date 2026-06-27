package executor

import "vornik.io/vornik/internal/persistence"

// recoveryFailureClass maps the canonical execution failure class
// (persistence.TaskFailureClass*, produced by ClassifyExecutionFailure)
// to the coarser, lead-facing class the recovery-mode role playbook
// switches on. The lead's RECOVERY MODE prompt has distinct branches for
// only a handful of classes (verifier_block, tool_error,
// budget_exhausted, hallucination_flagged, agent_error); every other
// canonical class collapses to "agent_error", the documented catch-all
// the lead handles by inferring the next-most-likely cause from
// failure_reason.
//
// verifier_block is NOT produced here — it is detected upstream via the
// typed *RecoverableVerifierError (which also carries BlockedURLs) before
// this fallback path runs. See workflow.go's on_fail handler and
// https://docs.vornik.io §3-4.
func recoveryFailureClass(execClass string) string {
	switch execClass {
	case persistence.TaskFailureClassBudgetBlocked:
		return "budget_exhausted"
	case persistence.TaskFailureClassHallucinatedPlacement:
		return "hallucination_flagged"
	case persistence.TaskFailureClassToolError:
		return "tool_error"
	default:
		return "agent_error"
	}
}
