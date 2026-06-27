package executor

import (
	"strings"
	"testing"
)

// When the recovery violation is a wrong-kind checkpoint (not a
// continue+plan), the corrective hint must explain that recovery
// checkpoints have to be kind=decision with selectable options — and must
// NOT mis-describe the lead's response as a plan to spawn role steps.
func TestRecoveryContractCorrectiveHint_WrongCheckpointKind(t *testing.T) {
	rc := &RecoveryContext{FailedStep: "review_risk", FailureClass: "context_timeout"}
	hint := recoveryContractCorrectiveHint(rc, "checkpoint:review")

	for _, want := range []string{
		"CORRECTION",
		"checkpoint:review",
		"kind=decision",
		"options",
		"review_risk",
		"context_timeout",
		`"outcome":"checkpoint"`,
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("checkpoint-kind hint missing %q\nhint = %s", want, hint)
		}
	}
	if strings.Contains(hint, "plan to spawn role steps") {
		t.Errorf("checkpoint-kind hint must not describe the response as a plan to spawn role steps\nhint = %s", hint)
	}
}

// Regression: task_20260618143127_a917b9aa76e81a8b (2026-06-18). In recovery
// mode the lead emitted a kind="review" checkpoint with its alternatives
// written as prose inside `draft` instead of a kind="decision" checkpoint
// with structured options[]. validateLeadOutcome accepted it (a review
// checkpoint only needs a non-empty draft), and the recovery contract only
// rejected continue+plan — so the wrong-kind checkpoint slipped straight
// through to the operator, who saw the prose "(1)(2)(3)" but no selectable
// radios (the UI renders options only for kind=decision). recoveryContractViolated
// closes that gap: in recovery mode a checkpoint MUST be kind=decision so the
// operator can actually pick an alternative.
func TestRecoveryContractViolated(t *testing.T) {
	tests := []struct {
		name     string
		parsedOK bool
		outcome  *LeadOutcome
		want     bool
	}{
		{
			name:     "decision checkpoint satisfies the contract",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeCheckpoint, Checkpoint: &CheckpointPayload{Kind: CheckpointKindDecision}},
			want:     false,
		},
		{
			name:     "review checkpoint violates (the e81a8b bug)",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeCheckpoint, Checkpoint: &CheckpointPayload{Kind: CheckpointKindReview}},
			want:     true,
		},
		{
			name:     "action_required checkpoint violates",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeCheckpoint, Checkpoint: &CheckpointPayload{Kind: CheckpointKindActionRequired}},
			want:     true,
		},
		{
			name:     "checkpoint with nil payload violates",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeCheckpoint},
			want:     true,
		},
		{
			name:     "continue violates (re-spawns the failed role)",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeContinue},
			want:     true,
		},
		{
			name:     "external_wait is a legitimate recovery terminal",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeExternalWait},
			want:     false,
		},
		{
			name:     "closure_request is a legitimate recovery terminal",
			parsedOK: true,
			outcome:  &LeadOutcome{Outcome: LeadOutcomeClosureRequest},
			want:     false,
		},
		{
			name:     "unparseable outcome violates",
			parsedOK: false,
			outcome:  nil,
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recoveryContractViolated(tt.parsedOK, tt.outcome); got != tt.want {
				t.Errorf("recoveryContractViolated(%v, %+v) = %v, want %v", tt.parsedOK, tt.outcome, got, tt.want)
			}
		})
	}
}
