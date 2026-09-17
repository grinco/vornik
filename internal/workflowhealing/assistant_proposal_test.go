package workflowhealing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func assistantCP(t *testing.T, workflowID, genome string) *persistence.ControlPlaneProposal {
	t.Helper()
	ops, err := json.Marshal([]map[string]string{
		{"op": "replace", "path": "configs/workflows/" + workflowID + ".md", "content": genome},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &persistence.ControlPlaneProposal{
		ID: "cpp_1", ProjectID: "assistant", ApplyOps: string(ops),
		Rationale: "the step times out under load", Title: "[assistant/C] heal w1",
	}
}

// WP9 step 3 (cutover, 2026-09-16): the assistant replaces the memetic
// architect as the healing candidate producer.
//
// The genome has to arrive as a WorkflowProposal because that is what the
// trial runner, the promotion gate and the proposals UI read — exactly as the
// deterministic recipe path already synthesizes one
// (ProposalFromRecipeResult). workflow_proposals is the genome table, not a
// memetic-owned table; memetic was one producer feeding it.
//
// The provenance field is the load-bearing part: a row must say which
// producer made it, or an operator reading the inbox after the cutover cannot
// tell an assistant genome from an architect one, and the two have different
// review histories.
func TestProposalFromAssistantProposal_CarriesGenomeAndProvenance(t *testing.T) {
	genome := "---\nworkflowId: w1\n---\n# healed\n"
	cp := assistantCP(t, "w1", genome)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	p, err := ProposalFromAssistantProposal("w1", cp, genome, []string{"exec_1", "exec_2"}, now)
	if err != nil {
		t.Fatalf("ProposalFromAssistantProposal: %v", err)
	}
	if p.WorkflowID != "w1" {
		t.Errorf("workflow id = %q", p.WorkflowID)
	}
	if p.ProposalYAML != genome {
		t.Errorf("the genome must survive BYTE-FOR-BYTE — the trial runner applies these bytes:\n%q", p.ProposalYAML)
	}
	if p.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("status = %q, want pending: nothing auto-promotes", p.Status)
	}
	if p.ArchitectModel != AssistantProposalProvenance {
		t.Errorf("provenance = %q, want %q — an operator must be able to tell which producer made this row",
			p.ArchitectModel, AssistantProposalProvenance)
	}
	if len(p.EvidenceRunIDs) != 2 {
		t.Errorf("evidence run ids lost: %v", p.EvidenceRunIDs)
	}
	if !strings.Contains(p.Motivation, "times out") {
		t.Errorf("the assistant's reasoning must reach the operator: %q", p.Motivation)
	}
}

// A proposal the bridge refuses must not become a WorkflowProposal. After the
// cutover there is no architect to fall back to, so the refusal has to be a
// refusal — producing an empty or partial genome here would hand the trial
// runner something nobody proposed.
func TestProposalFromAssistantProposal_RefusesWhatTheBridgeRefuses(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		ops  string
		wfID string
	}{
		{"two files", `[{"op":"replace","path":"configs/workflows/w1.md","content":"a"},{"op":"replace","path":"configs/workflows/w2.md","content":"b"}]`, "w1"},
		{"zero ops", `[]`, "w1"},
		{"wrong file", `[{"op":"replace","path":"configs/projects/p.yaml","content":"a"}]`, "w1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &persistence.ControlPlaneProposal{ID: "cpp_1", ApplyOps: tc.ops}
			if _, err := ProposalFromAssistantProposal(tc.wfID, cp, "", nil, now); err == nil {
				t.Fatalf("%s must be refused, not synthesized into a proposal", tc.name)
			}
		})
	}
}

// Regression: the first cutover deploy filed a real candidate labelled
// candidate_class="architect" for a genome the ASSISTANT produced (observed
// 2026-09-16 on whc_20260916112745_8822fea46aa1a1f0). The label is what an
// operator reads when deciding whether to trust a candidate, and it named a
// producer that no longer exists.
//
// The column is descriptive free text, not CHECK-constrained, so the correct
// class needed no migration — only the honesty to use it.
func TestCandidateFromAssistantProposal_IsClassedAsAssistant(t *testing.T) {
	genome := "---\nworkflowId: w1\n---\n# healed\n"
	cp := assistantCP(t, "w1", genome)
	trigger := &persistence.HealingTrigger{ID: "trg1", ProjectID: "p", WorkflowID: "w1"}

	cand := CandidateFromAssistantProposal(trigger, cp, genome)
	if cand == nil {
		t.Fatal("no candidate built")
	}
	if cand.CandidateClass != persistence.HealingCandidateAssistant {
		t.Fatalf("candidate_class = %q, want %q — the label must name the producer that actually made it",
			cand.CandidateClass, persistence.HealingCandidateAssistant)
	}
}
