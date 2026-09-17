package configassist

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// Regression: audit 2026-09-15 CA-15 — "The Healing Comparison Is Neither
// Read-Only Nor Detached".
//
// The comparison adapter called ordinary Engine.Propose, which FILES a real,
// actionable control-plane proposal before any comparison happens. The
// bridge could then reject it and leave that proposal sitting in the
// operator's inbox with no healing-trial provenance behind it — approvable
// through the ordinary workflow, for a repair nothing ever trialled. The
// surrounding comments promised "writes nothing and acts on nothing".
//
// A comparison observes; it does not file.
func TestProposeForHealing_ShadowFilesNothing(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["workflows/w1.md"]`,
			call("c1", "file_edit", map[string]any{"path": "workflows/w1.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""})),
		textResp("glm-5.2:cloud", "Adjusted the workflow."),
	}
	f.scriptJudge("pass", "timeout 15m")

	res, err := f.engine.ProposeForHealing(context.Background(), HealingRequest{
		ProjectID: "assistant", WorkflowID: "w1", Shadow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil {
		t.Fatalf("unexpected refusal: %+v", res.Refusal)
	}
	// The comparison still needs the proposal's CONTENT — the genome bridge
	// reads its ops and diff — so the object must come back fully built.
	if res.Proposal == nil {
		t.Fatal("a shadow comparison still needs the proposal object to compare")
	}
	if res.Proposal.ApplyOps == "" || res.Proposal.Evidence == "" {
		t.Fatalf("the shadow proposal must be complete enough to compare: %+v", res.Proposal)
	}
	// ...but nothing may have reached the ledger.
	if n := f.store.count(); n != 0 {
		t.Fatalf("a shadow comparison filed %d proposal(s) into the operator's inbox", n)
	}
	if !res.Proposal.Shadow {
		t.Fatal("the proposal must mark itself as comparison-only so no path can mistake it for an actionable one")
	}
}

// The AUTHORITATIVE healing path is unchanged: it still files, because that
// proposal is the one an operator acts on.
func TestProposeForHealing_NonShadowStillFiles(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["workflows/w1.md"]`,
			call("c1", "file_edit", map[string]any{"path": "workflows/w1.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""})),
		textResp("glm-5.2:cloud", "Adjusted the workflow."),
	}
	f.scriptJudge("pass", "timeout 15m")
	res, err := f.engine.ProposeForHealing(context.Background(), HealingRequest{ProjectID: "assistant", WorkflowID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil || res.Proposal == nil {
		t.Fatalf("the authoritative path must still file: %+v %+v", res.Refusal, res.Proposal)
	}
	if f.store.count() != 1 {
		t.Fatalf("filed %d proposals, want 1", f.store.count())
	}
}
