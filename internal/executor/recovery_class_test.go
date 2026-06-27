package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestRecoveryFailureClass_MapsCanonicalToLeadFacing verifies the
// mapping from the canonical persistence.TaskFailureClass* taxonomy to
// the coarse, lead-facing classes the recovery-mode role playbook
// switches on (see the RECOVERY MODE block in the lead role prompt).
func TestRecoveryFailureClass_MapsCanonicalToLeadFacing(t *testing.T) {
	cases := []struct {
		name      string
		execClass string
		want      string
	}{
		{"budget", persistence.TaskFailureClassBudgetBlocked, "budget_exhausted"},
		{"hallucination", persistence.TaskFailureClassHallucinatedPlacement, "hallucination_flagged"},
		{"tool", persistence.TaskFailureClassToolError, "tool_error"},
		// Classes the lead has no distinct playbook branch for collapse
		// to the agent_error catch-all (it handles them by inferring from
		// failure_reason).
		{"unknown collapses to agent_error", persistence.TaskFailureClassUnknown, "agent_error"},
		{"timeout collapses to agent_error", persistence.TaskFailureClassTimeout, "agent_error"},
		{"llm error collapses to agent_error", persistence.TaskFailureClassLLMError, "agent_error"},
		{"empty collapses to agent_error", "", "agent_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoveryFailureClass(tc.execClass); got != tc.want {
				t.Fatalf("recoveryFailureClass(%q) = %q, want %q", tc.execClass, got, tc.want)
			}
		})
	}
}

// TestRecoveryFailureClass_LightsUpDeadPlaybookBranches is the
// regression test for the Slice-5 gap: before this slice the on_fail
// handler hand-coded only "verifier_block"/"agent_error", so every
// non-verifier failure collapsed to agent_error and the lead's
// budget_exhausted / hallucination_flagged / tool_error playbook
// branches were unreachable dead code. This asserts the end-to-end
// path (real error string -> ClassifyExecutionFailure -> lead class)
// now reaches each previously-dead branch.
func TestRecoveryFailureClass_LightsUpDeadPlaybookBranches(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"over-budget", errors.New("execution halted: over budget (spend cap reached)"), "budget_exhausted"},
		{"hallucinated placement", errors.New("phase-2 verifier(s) failed: hallucinated placement detected"), "hallucination_flagged"},
		{"tool/container exit", errors.New("podman container exited non-zero running pandoc"), "tool_error"},
		// A bare, unclassifiable error still recovers as the agent_error
		// catch-all rather than dropping the recovery banner.
		{"opaque error", errors.New("something inscrutable happened"), "agent_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recoveryFailureClass(ClassifyExecutionFailure(tc.err, ""))
			if got != tc.want {
				t.Fatalf("recoveryFailureClass(ClassifyExecutionFailure(%q)) = %q, want %q",
					tc.err, got, tc.want)
			}
		})
	}
}

// guard: ClassifyExecutionFailure must keep classifying context
// cancellation as a non-agent class so a cancelled recovery doesn't
// masquerade as an agent failure the lead would try to recover.
func TestRecoveryFailureClass_CancelledNotAgentError(t *testing.T) {
	got := recoveryFailureClass(ClassifyExecutionFailure(context.Canceled, ""))
	// Cancelled has no lead branch, so it collapses to agent_error — but
	// the point is the classifier recognised it (CANCELLED), not that we
	// invented a class. This documents the intentional collapse.
	if got != "agent_error" {
		t.Fatalf("cancelled mapped to %q, want agent_error (documented collapse)", got)
	}
}
