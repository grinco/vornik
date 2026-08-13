package executor

import (
	"strings"
	"testing"
)

// Backport (a) of LLD 09 §13.5: verifyRoleClaims is enforced on every deployment,
// and was explained to agents on only two of six shipped presets.

func TestClaimVerificationPrompt_AppendedAfterRoleIdentity(t *testing.T) {
	got := composeSystemPromptWithClaimVerification("You are the tester.")

	if !strings.HasPrefix(got, "You are the tester.") {
		t.Errorf("role identity is no longer first: %q", got)
	}
	if !strings.Contains(got, "REPORTING INTEGRITY") {
		t.Error("integrity block missing; on four of six presets the agent then meets " +
			"verifyRoleClaims as an unexplained hard failure")
	}
}

// TestClaimVerificationPrompt_NeedsNoCapabilityGate pins the deliberate absence of a
// capability gate: every other injected block gates on a tool or context being
// present, while verifyRoleClaims is not configurable. The caller's own guard (a
// system prompt must already be under composition) is asserted separately, in
// TestBuildAgentInput_NoCanonicalContextKeys.
func TestClaimVerificationPrompt_NeedsNoCapabilityGate(t *testing.T) {
	if got := composeSystemPromptWithClaimVerification(""); !strings.Contains(got, "REPORTING INTEGRITY") {
		t.Error("a role with no prompt of its own lost the invariant")
	}
}

// TestClaimVerificationPrompt_DoesNotLeakTheMechanism is the security-shaped one.
//
// Naming the fields and tool classes the gate inspects would tell an agent the
// cheapest way to satisfy it — emit one trivial execution-class call and the
// "did you actually run anything" check passes — converting a deception check into
// a bypass recipe. The block must state the standard, not the detector.
func TestClaimVerificationPrompt_DoesNotLeakTheMechanism(t *testing.T) {
	mechanism := []string{
		"toolAudit", "tool_audit", "testing.passed", "files_changed",
		"test_run", "lint_run", "typecheck_run", "run_shell",
		"verifyRoleClaims", "git diff", "HEAD",
	}
	for _, m := range mechanism {
		if strings.Contains(claimVerificationSystemPromptBlock, m) {
			t.Errorf("block names %q — describing HOW the check works tells an agent the "+
				"cheapest way to satisfy it instead of doing the work; state the norm, "+
				"not the detector", m)
		}
	}
}

// TestClaimVerificationPrompt_OffersTheHonestWayOut: a rule with no accepted
// alternative pushes a cornered agent toward the lie it is meant to prevent.
func TestClaimVerificationPrompt_OffersTheHonestWayOut(t *testing.T) {
	lower := strings.ToLower(claimVerificationSystemPromptBlock)
	if !strings.Contains(lower, "could not be run") && !strings.Contains(lower, "did not") {
		t.Error("block states the prohibition without the accepted alternative; an agent " +
			"that cannot run a check needs to know reporting that fact is ACCEPTED, or the " +
			"rule itself pressures it to fabricate")
	}
}

func TestClaimVerificationPrompt_StaysCheap(t *testing.T) {
	const maxBytes = 600 // ~150 tokens, paid on every step of every role
	if n := len(claimVerificationSystemPromptBlock); n > maxBytes {
		t.Errorf("integrity block is %d bytes, over the %d-byte budget; trim it rather "+
			"than raising this bound (LLD 09 §13.3)", n, maxBytes)
	}
}
