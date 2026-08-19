package executor

import "testing"

// The exact bytes the lead emitted on a recovery hop, 2026-08-19, taken from
// recover_lead_lead-response-…17dc.md. This is a good answer: it names the
// failure, offers three labelled alternatives, and states a default.
//
// It was rejected. recoveryContractViolated requires a NESTED checkpoint object
// (`o.Checkpoint == nil || o.Checkpoint.Kind != decision`), and the model put
// the kind and its fields FLAT, as siblings of `outcome`. So o.Checkpoint was
// nil, every recovery hop was a contract violation, and the corrective retry
// fired every time — while the response artifact looked perfect, which is what
// made this survive three fixes aimed at other layers.
//
// The model is not wrong to do this. recoveryModeResponseSchema keeps the nested
// payloads "permissive (additionalProperties: true)", so nothing told it the
// nesting was load-bearing. And the codebase already decided this question for
// gates: lookupJSONPath accepts nested AND flat keys precisely because "LLMs
// frequently produce flat keys". The recovery contract should be no stricter.
const flatDecisionCheckpoint = `{
  "outcome": "checkpoint",
  "checkpoint_kind": "decision",
  "question": "The doomed step failed with a container error — how should we proceed?",
  "options": [
    {"id": "retry", "label": "Retry the same step once"},
    {"id": "abort", "label": "Abort with explanation"}
  ],
  "default_if_no_response": "abort",
  "default_reason": "container runtime failure is outside the task's control"
}`

func TestParseLeadOutcome_AcceptsFlatDecisionCheckpoint(t *testing.T) {
	out, ok, err := ParseLeadOutcome([]byte(flatDecisionCheckpoint))
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if out.Outcome != LeadOutcomeCheckpoint {
		t.Fatalf("outcome = %q, want checkpoint", out.Outcome)
	}
	if out.Checkpoint == nil {
		t.Fatal("checkpoint payload is nil — the flat `checkpoint_kind` shape was not " +
			"lifted into it, so recoveryContractViolated sees no decision and every " +
			"recovery hop is a contract violation")
	}
	if out.Checkpoint.Kind != CheckpointKindDecision {
		t.Errorf("kind = %q, want decision", out.Checkpoint.Kind)
	}
	if len(out.Checkpoint.Options) != 2 {
		t.Errorf("options = %d, want 2 — the operator needs something selectable",
			len(out.Checkpoint.Options))
	}
	if out.Checkpoint.Question == "" {
		t.Error("question lost in the lift")
	}
	if out.Checkpoint.DefaultIfNoResponse != "abort" {
		t.Errorf("default_if_no_response = %q, want abort", out.Checkpoint.DefaultIfNoResponse)
	}
}

// And the contract must actually be satisfied by it — that is the whole point.
func TestRecoveryContract_SatisfiedByFlatDecisionCheckpoint(t *testing.T) {
	out, ok, err := ParseLeadOutcome([]byte(flatDecisionCheckpoint))
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if recoveryContractViolated(ok, out) {
		t.Error("a flat decision checkpoint must satisfy the recovery contract")
	}
}

// The nested shape must keep working, and must win when both are present — a
// model that nests is being explicit, so its structure is authoritative.
func TestParseLeadOutcome_NestedCheckpointStillWinsOverFlat(t *testing.T) {
	both := `{"outcome":"checkpoint","checkpoint_kind":"review",
	          "checkpoint":{"kind":"decision","question":"nested wins",
	                        "options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}}`
	out, ok, err := ParseLeadOutcome([]byte(both))
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if out.Checkpoint == nil || out.Checkpoint.Kind != CheckpointKindDecision {
		t.Fatalf("nested payload must be preserved verbatim, got %+v", out.Checkpoint)
	}
	if out.Checkpoint.Question != "nested wins" {
		t.Errorf("question = %q, want the nested one", out.Checkpoint.Question)
	}
}

// A flat NON-decision kind must still violate the contract: the point of the
// rule is that the operator gets selectable options, and a review/action_required
// checkpoint carries its alternatives as prose.
func TestRecoveryContract_FlatNonDecisionKindStillViolates(t *testing.T) {
	out, ok, err := ParseLeadOutcome([]byte(`{"outcome":"checkpoint","checkpoint_kind":"review","draft":"x"}`))
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if !recoveryContractViolated(ok, out) {
		t.Error("a flat review checkpoint must still violate the recovery contract")
	}
}
