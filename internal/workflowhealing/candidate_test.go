package workflowhealing

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestCandidateFromArchitectProposal(t *testing.T) {
	trig := &persistence.HealingTrigger{
		ID:         "hb_1",
		ProjectID:  "proj",
		WorkflowID: "research",
	}
	prop := &persistence.WorkflowProposal{
		ID:           "wpr_1",
		WorkflowID:   "research",
		ProposalYAML: "---\nworkflow_id: research\n---\nbody",
		Motivation:   "because telemetry",
	}
	c := CandidateFromArchitectProposal(trig, prop)
	if c == nil {
		t.Fatal("nil candidate from valid inputs")
	}
	if c.TriggerID != "hb_1" || c.ProjectID != "proj" || c.WorkflowID != "research" || c.ProposalID != "wpr_1" {
		t.Errorf("link fields wrong: %+v", c)
	}
	if c.CandidateClass != persistence.HealingCandidateArchitect {
		t.Errorf("class = %q, want architect", c.CandidateClass)
	}
	if c.Status != persistence.HealingCandidateDraft {
		t.Errorf("status = %q, want draft", c.Status)
	}
	if c.RiskLevel != persistence.HealingRiskMedium {
		t.Errorf("risk = %q, want medium", c.RiskLevel)
	}
	if c.ProposalDiff != prop.ProposalYAML || c.Motivation != prop.Motivation {
		t.Error("denormalised proposal fields not copied")
	}
	if c.CandidateGenomeHash == "" {
		t.Error("genome hash not derived from proposal YAML")
	}
}

func TestCandidateFromArchitectProposal_NilInputs(t *testing.T) {
	if CandidateFromArchitectProposal(nil, &persistence.WorkflowProposal{}) != nil {
		t.Error("nil trigger must yield nil")
	}
	if CandidateFromArchitectProposal(&persistence.HealingTrigger{}, nil) != nil {
		t.Error("nil proposal must yield nil")
	}
}
