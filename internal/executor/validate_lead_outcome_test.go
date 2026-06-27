package executor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestValidateLeadOutcome covers every per-outcome required-field
// branch. The contract is: the executor refuses malformed envelopes
// at this gate so downstream handlers don't have to.
func TestValidateLeadOutcome(t *testing.T) {
	t.Run("continue requires plan.steps", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{Outcome: LeadOutcomeContinue})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "plan.steps")
	})
	t.Run("continue with empty steps errors", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{
			Outcome: LeadOutcomeContinue,
			Plan:    &PlanShape{Steps: nil},
		})
		assert.Error(t, err)
	})
	t.Run("continue with steps passes", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{
			Outcome: LeadOutcomeContinue,
			Plan:    &PlanShape{Steps: []string{"research"}},
		})
		assert.NoError(t, err)
	})

	t.Run("checkpoint requires payload", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{Outcome: LeadOutcomeCheckpoint})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "checkpoint payload")
	})
	t.Run("checkpoint decision requires question + ≥2 options", func(t *testing.T) {
		// Question missing.
		err := validateLeadOutcome(&LeadOutcome{
			Outcome:    LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindDecision, Options: []CheckpointOption{{ID: "a"}, {ID: "b"}}},
		})
		assert.Error(t, err)
		// Only 1 option.
		err = validateLeadOutcome(&LeadOutcome{
			Outcome: LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindDecision,
				Question: "pick", Options: []CheckpointOption{{ID: "a"}}},
		})
		assert.Error(t, err)
		// OK.
		err = validateLeadOutcome(&LeadOutcome{
			Outcome: LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindDecision,
				Question: "pick", Options: []CheckpointOption{{ID: "a"}, {ID: "b"}}},
		})
		assert.NoError(t, err)
	})
	t.Run("checkpoint action_required needs task_for_human", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{
			Outcome:    LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindActionRequired},
		})
		assert.Error(t, err)
		err = validateLeadOutcome(&LeadOutcome{
			Outcome: LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{
				Kind:         CheckpointKindActionRequired,
				TaskForHuman: "review and approve",
			},
		})
		assert.NoError(t, err)
	})
	t.Run("checkpoint review needs draft", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{
			Outcome:    LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindReview},
		})
		assert.Error(t, err)
		err = validateLeadOutcome(&LeadOutcome{
			Outcome: LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{
				Kind:  CheckpointKindReview,
				Draft: "draft body",
			},
		})
		assert.NoError(t, err)
	})
	t.Run("unknown checkpoint kind rejected", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{
			Outcome:    LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: "made-up"},
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown checkpoint.kind")
	})

	t.Run("external_wait requires expected_by", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{Outcome: LeadOutcomeExternalWait})
		assert.Error(t, err)
		// Nil ExternalWait.ExpectedBy.
		err = validateLeadOutcome(&LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			ExternalWait: &ExternalWaitPayload{},
		})
		assert.Error(t, err)
		// With expected_by.
		when := time.Now().Add(time.Hour)
		err = validateLeadOutcome(&LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			ExternalWait: &ExternalWaitPayload{ExpectedBy: &when},
		})
		assert.NoError(t, err)
	})

	t.Run("closure_request requires summary", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{Outcome: LeadOutcomeClosureRequest})
		assert.Error(t, err)
		// Empty summary.
		err = validateLeadOutcome(&LeadOutcome{
			Outcome:        LeadOutcomeClosureRequest,
			ClosureRequest: &ClosureRequestPayload{},
		})
		assert.Error(t, err)
		err = validateLeadOutcome(&LeadOutcome{
			Outcome:        LeadOutcomeClosureRequest,
			ClosureRequest: &ClosureRequestPayload{Summary: "wrap-up"},
		})
		assert.NoError(t, err)
	})

	t.Run("unknown outcome passes (validator is field-required focused)", func(t *testing.T) {
		err := validateLeadOutcome(&LeadOutcome{Outcome: LeadOutcomeKind("future-shape")})
		// The switch falls through to nil for unrecognised outcomes —
		// caller's handleLeadHandoff rejects them with a clear error.
		assert.NoError(t, err)
	})
}
