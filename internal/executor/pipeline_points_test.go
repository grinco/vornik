package executor

import (
	"reflect"
	"testing"
)

// TestStepOutcomePoint_ParticipantsArePinned — the order is the spec
// (2026-09-04-pipeline-points-design.md §3.4). Reordering is a deliberate
// diff here, with the inter-participant edges in pipeline_points.go's
// comment re-checked, not a side effect of moving a block.
func TestStepOutcomePoint_ParticipantsArePinned(t *testing.T) {
	e, _, _, _, _ := setup()
	want := []string{
		"output_file_contract",
		"tool_contract",
		"plausibility",
		"claimed_files",
		"role_claims",
		"hallucination_detector",
		"trading_floor",
		"outcome_verifiers",
	}
	if got := e.stepOutcomePoint().Participants(); !reflect.DeepEqual(got, want) {
		t.Fatalf("executor.step_outcome participants = %v, want %v", got, want)
	}
	if e.stepOutcomePoint() != e.stepOutcomeChain {
		t.Error("the chain is built once")
	}
	for _, name := range want {
		if stepOutcomeExitTier[name] && stepOutcomeRemoveMsg[name] != "" {
			t.Errorf("%s: a participant is in exactly one refusal tier", name)
		}
		if !stepOutcomeExitTier[name] && stepOutcomeRemoveMsg[name] == "" {
			t.Errorf("%s: a non-exit-tier participant needs its removal log line", name)
		}
	}
}
