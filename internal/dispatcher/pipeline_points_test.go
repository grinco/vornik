package dispatcher

import (
	"reflect"
	"testing"
)

// TestAgentPoints_ParticipantsArePinned — the order is the spec
// (2026-09-04-pipeline-points-design.md §3): each dispatcher point carries
// exactly today's gate, by name, on every deployment. Adding or reordering a
// participant is a deliberate diff here, not a side effect of moving a block.
func TestAgentPoints_ParticipantsArePinned(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, nil)
	p := a.points()
	if got := p.preTool.Participants(); !reflect.DeepEqual(got, []string{"intent_judge"}) {
		t.Errorf("dispatcher.pre_tool participants = %v", got)
	}
	if got := p.postTool.Participants(); !reflect.DeepEqual(got, []string{"output_guard"}) {
		t.Errorf("dispatcher.post_tool participants = %v", got)
	}
	if got := p.continuation.Participants(); !reflect.DeepEqual(got, []string{"hallucination_retry"}) {
		t.Errorf("dispatcher.continuation participants = %v", got)
	}
	if a.points() != p {
		t.Error("points() must build the chains once")
	}
	// A bare Agent literal (as some tests build) still has its points.
	if bare := (&Agent{}).points(); bare == nil || len(bare.preTool.Participants()) != 1 {
		t.Error("a bare Agent must still get its chains lazily")
	}
}
