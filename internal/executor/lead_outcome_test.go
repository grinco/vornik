package executor

import (
	"strings"
	"testing"
)

func TestParseLeadOutcome_LegacyShape(t *testing.T) {
	in := []byte(`{"plan":{"steps":["researcher","writer"],"rationale":"r"},"message":"go"}`)
	out, ok, err := ParseLeadOutcome(in)
	if err != nil || !ok {
		t.Fatalf("legacy shape should parse: ok=%v err=%v", ok, err)
	}
	if out.Outcome != LeadOutcomeContinue {
		t.Errorf("legacy → expected continue, got %s", out.Outcome)
	}
	if len(out.Plan.Steps) != 2 {
		t.Errorf("steps got %v", out.Plan.Steps)
	}
}

func TestParseLeadOutcome_ContinueExplicit(t *testing.T) {
	in := []byte(`{"outcome":"continue","plan":{"steps":["a"],"phase":"research"},"message":"x","scratchpad_update":{"summary":"sum"}}`)
	out, ok, err := ParseLeadOutcome(in)
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	if out.Outcome != LeadOutcomeContinue {
		t.Errorf("got %s", out.Outcome)
	}
	if out.Plan.Phase != "research" {
		t.Errorf("phase got %q", out.Plan.Phase)
	}
	if out.ScratchpadUpdate == nil || out.ScratchpadUpdate.Summary != "sum" {
		t.Errorf("scratchpad missing")
	}
}

func TestParseLeadOutcome_CheckpointDecision(t *testing.T) {
	in := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"pick one","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}}`)
	out, ok, err := ParseLeadOutcome(in)
	if err != nil || !ok {
		t.Fatalf("err=%v", err)
	}
	if out.Outcome != LeadOutcomeCheckpoint {
		t.Fatalf("got %s", out.Outcome)
	}
	if out.Checkpoint.Kind != CheckpointKindDecision {
		t.Errorf("kind got %q", out.Checkpoint.Kind)
	}
}

func TestParseLeadOutcome_CheckpointDecisionTooFewOptions(t *testing.T) {
	in := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"q","options":[{"id":"a","label":"A"}]}}`)
	_, _, err := ParseLeadOutcome(in)
	if err == nil {
		t.Fatal("expected validation failure for single-option decision")
	}
	if !strings.Contains(err.Error(), "≥2") {
		t.Errorf("error should mention ≥2 options, got %v", err)
	}
}

func TestParseLeadOutcome_ActionRequired(t *testing.T) {
	in := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"action_required","task_for_human":"measure windows"}}`)
	out, _, err := ParseLeadOutcome(in)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.Checkpoint.TaskForHuman != "measure windows" {
		t.Errorf("got %q", out.Checkpoint.TaskForHuman)
	}
}

func TestParseLeadOutcome_ActionRequired_MissingBody(t *testing.T) {
	in := []byte(`{"outcome":"checkpoint","checkpoint":{"kind":"action_required"}}`)
	_, _, err := ParseLeadOutcome(in)
	if err == nil {
		t.Fatal("expected error for action_required without task_for_human")
	}
}

func TestParseLeadOutcome_ExternalWait(t *testing.T) {
	in := []byte(`{"outcome":"external_wait","external_wait":{"expected_by":"2026-06-01T12:00:00Z","reason":"vendor will reply"}}`)
	out, _, err := ParseLeadOutcome(in)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.ExternalWait == nil || out.ExternalWait.ExpectedBy == nil {
		t.Fatal("expected_by missing")
	}
}

func TestParseLeadOutcome_ExternalWait_NoDeadline(t *testing.T) {
	in := []byte(`{"outcome":"external_wait","external_wait":{"reason":"x"}}`)
	_, _, err := ParseLeadOutcome(in)
	if err == nil {
		t.Fatal("external_wait without expected_by should fail")
	}
}

func TestParseLeadOutcome_ClosureRequest(t *testing.T) {
	in := []byte(`{"outcome":"closure_request","closure_request":{"summary":"installation done"}}`)
	out, _, err := ParseLeadOutcome(in)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.ClosureRequest.Summary != "installation done" {
		t.Errorf("got %q", out.ClosureRequest.Summary)
	}
}

func TestParseLeadOutcome_ClosureRequest_NoSummary(t *testing.T) {
	in := []byte(`{"outcome":"closure_request","closure_request":{}}`)
	_, _, err := ParseLeadOutcome(in)
	if err == nil {
		t.Fatal("closure_request without summary should fail")
	}
}

func TestParseLeadOutcome_UnknownOutcome(t *testing.T) {
	in := []byte(`{"outcome":"abandon"}`)
	_, _, err := ParseLeadOutcome(in)
	if err == nil {
		t.Fatal("unknown outcome should fail")
	}
}

func TestParseLeadOutcome_MissingBoth(t *testing.T) {
	in := []byte(`{"message":"x"}`)
	_, _, err := ParseLeadOutcome(in)
	if err == nil {
		t.Fatal("envelope without outcome or plan should fail")
	}
}

func TestParseLeadOutcome_PhaseTransitions(t *testing.T) {
	in := []byte(`{"outcome":"continue","plan":{"steps":["a"]},"phase_transitions":[{"phase":"research","status":"exit"},{"phase":"measure","status":"enter"}]}`)
	out, _, err := ParseLeadOutcome(in)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out.PhaseTransitions) != 2 {
		t.Errorf("got %d transitions", len(out.PhaseTransitions))
	}
}

func TestSerializeCheckpointMetadata_RoundTrip(t *testing.T) {
	cp := &CheckpointPayload{
		Kind:     CheckpointKindDecision,
		Question: "pick",
		Options:  []CheckpointOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
	}
	b, err := SerializeCheckpointMetadata(cp)
	if err != nil {
		t.Fatalf("serialize err=%v", err)
	}
	if !strings.Contains(string(b), `"kind":"decision"`) {
		t.Errorf("serialized form missing kind: %s", b)
	}
}
