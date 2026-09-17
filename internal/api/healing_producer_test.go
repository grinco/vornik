package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubHealingAssistant drives the producer leg.
type stubHealingAssistant struct {
	proposal *persistence.ControlPlaneProposal
	err      error
	calls    int
}

func (s *stubHealingAssistant) ProposeForHealing(context.Context, string, string, string, []string) (*persistence.ControlPlaneProposal, error) {
	s.calls++
	return s.proposal, s.err
}

// recordingWorkflowProposals captures what the producer filed.
type recordingWorkflowProposals struct {
	persistence.WorkflowProposalRepository
	rows []*persistence.WorkflowProposal
}

func (r *recordingWorkflowProposals) Insert(_ context.Context, p *persistence.WorkflowProposal) error {
	r.rows = append(r.rows, p)
	return nil
}

func assistantHealingProposal(t *testing.T, workflowID, genome string) *persistence.ControlPlaneProposal {
	t.Helper()
	ops, err := json.Marshal([]map[string]string{
		{"op": "replace", "path": "configs/workflows/" + workflowID + ".md", "content": genome},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &persistence.ControlPlaneProposal{
		ID: "cpp_1", ProjectID: "assistant", ApplyOps: string(ops),
		Rationale: "the plan step times out under load",
	}
}

// WP9 step 3, the cutover (2026-09-16): the config assistant replaces the
// memetic architect as the healing candidate producer.
//
// The genome must arrive as a WorkflowProposal, because that is what the
// trial runner, the promotion gate and the proposals UI read — and it must
// say WHICH producer made it, or an operator reading the inbox after the
// cutover cannot tell an assistant row from a legacy architect one.
func TestGenerateAssistantCandidate_FilesAWorkflowProposal(t *testing.T) {
	genome := "---\nworkflowId: w1\n---\n# healed\n"
	rows := &recordingWorkflowProposals{}
	s := NewServer()
	s.SetHealingAssistant(&stubHealingAssistant{proposal: assistantHealingProposal(t, "w1", genome)})
	s.workflowProposals = rows

	trigger := &persistence.HealingTrigger{ID: "trg1", ProjectID: "assistant", WorkflowID: "w1"}
	p, err := s.generateAssistantCandidate(context.Background(), trigger)
	if err != nil {
		t.Fatalf("generateAssistantCandidate: %v", err)
	}
	if len(rows.rows) != 1 {
		t.Fatalf("the proposal must be FILED — the trial runner reads it from there; got %d rows", len(rows.rows))
	}
	if p.ProposalYAML != genome {
		t.Errorf("the genome must survive byte-for-byte: %q", p.ProposalYAML)
	}
	if p.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("status = %q, want pending — nothing auto-promotes", p.Status)
	}
	if p.ArchitectModel != "config-assistant" {
		t.Errorf("provenance = %q; an operator must be able to tell this from a legacy architect row", p.ArchitectModel)
	}
}

// THE SEAM THE CUTOVER CREATES: before it, a bridge refusal fell back to the
// architect. There is no fallback now, so a refusal has to BE a refusal —
// filing a partial or empty genome would hand the trial runner something
// nobody proposed, which is the one trade §6.3.4 refuses to make for
// availability.
func TestGenerateAssistantCandidate_RefusalFilesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  string
	}{
		{"two files", `[{"op":"replace","path":"configs/workflows/w1.md","content":"a"},{"op":"replace","path":"configs/workflows/w2.md","content":"b"}]`},
		{"wrong file", `[{"op":"replace","path":"configs/projects/p.yaml","content":"a"}]`},
		{"no ops", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := &recordingWorkflowProposals{}
			s := NewServer()
			s.SetHealingAssistant(&stubHealingAssistant{
				proposal: &persistence.ControlPlaneProposal{ID: "cpp_1", ApplyOps: tc.ops},
			})
			s.workflowProposals = rows

			trigger := &persistence.HealingTrigger{ID: "trg1", ProjectID: "assistant", WorkflowID: "w1"}
			if _, err := s.generateAssistantCandidate(context.Background(), trigger); err == nil {
				t.Fatal("a proposal the bridge refuses must not become a healing candidate")
			}
			if len(rows.rows) != 0 {
				t.Fatalf("a refused proposal must file NOTHING, got %d rows", len(rows.rows))
			}
		})
	}
}

// An assistant that declines is distinct from one that is not wired, and both
// are distinct from a bridge refusal — the operator's next action differs.
func TestGenerateAssistantCandidate_DistinguishesFailureKinds(t *testing.T) {
	s := NewServer()
	trigger := &persistence.HealingTrigger{ID: "trg1", ProjectID: "assistant", WorkflowID: "w1"}

	if _, err := s.generateAssistantCandidate(context.Background(), trigger); !errors.Is(err, errHealingProducerUnavailable) {
		t.Fatalf("an unwired producer must say so, got %v", err)
	}

	s.SetHealingAssistant(&stubHealingAssistant{err: errors.New("model refused")})
	s.workflowProposals = &recordingWorkflowProposals{}
	if _, err := s.generateAssistantCandidate(context.Background(), trigger); err == nil ||
		errors.Is(err, errHealingProducerUnavailable) {
		t.Fatalf("a producer that RAN and declined is not an unwired producer, got %v", err)
	}
}
