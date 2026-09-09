package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/stepoutcome"
)

// The residual unclassified step failures were the claim verifiers' own
// refusals. Measured 2026-09-02, after MODEL_UNHEALTHY was named: 65 rows
// all-time, 7 of 7 in the trailing 30 days, every one either
// "agent fabrication detected: …" (verifyRoleClaims) or
// "agent claimed N file(s) but verification failed: …" (verifyClaimedFiles).
// Both are a verifier refusing a claim — a corroboration failure with a class
// that already exists — and the plan-step path already records them as such
// (plan_step.go finalizes verifyRoleClaims as verify_claims_failed). Only the
// workflow-step pipeline path lost the class, because the refiner had no
// typed arm for either error.
//
// Backlog 2026-09-02 "The residual unclassified failures are agent-fabrication
// detections, which have a class already". Classified from the TYPE, never the
// message — the MODEL_UNHEALTHY pattern (2026-09-02 design D1).

// TestClaimRefusal_RoleClaimsFabricationIsVerifyClaimsFailed — the dominant
// shape (6 of the 7 rows): a reviewer citing a commit it never inspected, a
// tester claiming a pass with nothing in the audit.
func TestClaimRefusal_RoleClaimsFabricationIsVerifyClaimsFailed(t *testing.T) {
	// testing.passed:true with no toolAudit at all is a fabrication the
	// verifier refuses without touching git — the cheapest real trigger.
	err := (&Executor{}).verifyRoleClaims(context.Background(),
		[]byte(`{"testing":{"passed":true}}`), "", "", "")
	if err == nil {
		t.Fatal("fixture drift: testing.passed:true with no tool audit must be refused")
	}
	for name, e := range map[string]error{
		"bare":    err,
		"wrapped": fmt.Errorf("plan role 2 (tester) %w", err),
	} {
		t.Run(name, func(t *testing.T) {
			outcome, class := refineAgentFailureOutcomeErr(e)
			if class != stepoutcome.ClassVerifyFailed {
				t.Errorf("class = %q, want %q — the verifier refused a claim, and that has a name", class, stepoutcome.ClassVerifyFailed)
			}
			if outcome != stepoutcome.SchemaViolation {
				t.Errorf("outcome = %q, want %q — the plan-step path records the same refusal as a schema violation; the two paths must agree", outcome, stepoutcome.SchemaViolation)
			}
		})
	}
	// The message contract is load-bearing elsewhere: classifyShapeFailure
	// routes the corrective retry on this prefix, and migration 170's history
	// rewrite keyed on the sibling. The type is ADDED to the error, never
	// substituted for its text.
	if got := err.Error(); len(got) < len("agent fabrication detected: ") || got[:len("agent fabrication detected: ")] != "agent fabrication detected: " {
		t.Errorf("message contract broken: %q", got)
	}
}

// TestClaimRefusal_ClaimedFilesIsMissingDeclaredOutput — the other shape: a
// produced_files entry that is not on disk. missing_declared_output's own doc
// describes exactly this ("the agent declared an outputArtifact … but the file
// it named isn't on disk"), and migration 170 already routed history there;
// nothing LIVE wrote the class until now.
func TestClaimRefusal_ClaimedFilesIsMissingDeclaredOutput(t *testing.T) {
	err := (&Executor{}).verifyClaimedFiles(
		[]byte(`{"produced_files":["artifacts/out/never-written.md"]}`),
		t.TempDir(), "", time.Now())
	if err == nil {
		t.Fatal("fixture drift: a claimed file that does not exist must be refused")
	}
	outcome, class := refineAgentFailureOutcomeErr(fmt.Errorf("step failed: %w", err))
	if class != stepoutcome.ClassMissingOutput {
		t.Errorf("class = %q, want %q", class, stepoutcome.ClassMissingOutput)
	}
	if outcome != stepoutcome.SchemaViolation {
		t.Errorf("outcome = %q, want %q", outcome, stepoutcome.SchemaViolation)
	}
	var refusal *claimRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the verifier's error must carry the type the classifier reads, got %T", err)
	}
}

// A refusal that reaches the refiner as TEXT ONLY — the shape the ledger holds
// today — still lands in the catch-all. The fix is the type, not a new string
// arm; this pins that no message match was added, so the two disciplines do
// not drift into each other (design D1: never recover a daemon-generated
// condition by parsing the sentence the daemon wrote about it).
func TestClaimRefusal_TextAloneIsStillUnclassified(t *testing.T) {
	_, class := refineAgentFailureOutcomeErr(errors.New("agent fabrication detected: review.checked_commit:abc claimed but that object does not exist"))
	if class != stepoutcome.ClassUnclassified {
		t.Errorf("a string-only arm was added; class = %q", class)
	}
}
