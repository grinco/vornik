package workflowhealing

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// These pin the synthesis keystone for deterministic-recipe live generation
// (Self-Healing Workflow Genome v1). The recipes (recipes.go) were shipped +
// unit-tested but ORPHANED: a RecipeResult is not promotable on its own
// because the promoter hard-requires a linked WorkflowProposal
// (promoter.go: ErrNoProposalLinked). ProposalFromRecipeResult turns a recipe
// result into a pending proposal that flows through the SAME approve/apply
// path as an architect proposal, and CandidateFromRecipeResult links a
// HealingCandidate to it — the sibling of CandidateFromArchitectProposal,
// carrying the recipe's real class/hash/risk rather than architect placeholders.

func retryRecipeFixture(t *testing.T) *RecipeResult {
	t.Helper()
	base := &registry.Workflow{
		ID:          "demo",
		Description: "demo workflow for recipe synthesis tests",
		Entrypoint:  "impl",
		Steps: map[string]registry.WorkflowStep{
			"impl": {
				Type:        "agent",
				Role:        "coder",
				Prompt:      "do the work",
				OnSuccess:   "done",
				RetryPolicy: registry.WorkflowRetryPolicy{MaxRetries: 3},
			},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
	r, err := RetryBudgetRecipe(base, "impl", 1, "failed", []string{"exec1", "exec2"})
	if err != nil {
		t.Fatalf("RetryBudgetRecipe: %v", err)
	}
	return r
}

func verifierRecipeFixture(t *testing.T) *RecipeResult {
	t.Helper()
	base := &registry.Workflow{
		ID:          "demo2",
		Description: "demo workflow for verifier recipe",
		Entrypoint:  "write",
		Steps: map[string]registry.WorkflowStep{
			"write":  {Type: "agent", Role: "writer", Prompt: "write", OnSuccess: "review"},
			"review": {Type: "agent", Role: "reviewer", Prompt: "review", OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
	r, err := VerifierInsertionRecipe(base, "write", "verifier", "", []string{"execA"})
	if err != nil {
		t.Fatalf("VerifierInsertionRecipe: %v", err)
	}
	return r
}

func TestProposalFromRecipeResult_RetryBudget(t *testing.T) {
	r := retryRecipeFixture(t)
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	p := ProposalFromRecipeResult("demo", r, now)
	if p == nil {
		t.Fatal("expected a proposal, got nil")
	}
	if !strings.HasPrefix(p.ID, "wpr_") {
		t.Errorf("proposal ID = %q, want wpr_ prefix", p.ID)
	}
	if p.WorkflowID != "demo" {
		t.Errorf("WorkflowID = %q, want demo", p.WorkflowID)
	}
	if p.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("Status = %q, want pending", p.Status)
	}
	if p.Kind != persistence.WorkflowProposalKindChangeRetryPolicy {
		t.Errorf("Kind = %q, want change_retry_policy", p.Kind)
	}
	if p.ProposalYAML != r.ProposalDiff {
		t.Errorf("ProposalYAML must be the recipe's candidate WORKFLOW.md")
	}
	if p.Motivation != r.Motivation {
		t.Errorf("Motivation = %q, want %q", p.Motivation, r.Motivation)
	}
	if strings.Join(p.EvidenceRunIDs, ",") != "exec1,exec2" {
		t.Errorf("EvidenceRunIDs = %v, want [exec1 exec2]", p.EvidenceRunIDs)
	}
	if p.ArchitectModel != RecipeProposalProvenance {
		t.Errorf("ArchitectModel = %q, want %q (deterministic provenance marker)", p.ArchitectModel, RecipeProposalProvenance)
	}
	if !p.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", p.CreatedAt, now)
	}
}

func TestProposalFromRecipeResult_VerifierInsertionIsAddStep(t *testing.T) {
	r := verifierRecipeFixture(t)
	p := ProposalFromRecipeResult("demo2", r, time.Now())
	if p == nil {
		t.Fatal("expected a proposal, got nil")
	}
	if p.Kind != persistence.WorkflowProposalKindAddStep {
		t.Errorf("Kind = %q, want add_step", p.Kind)
	}
}

func TestProposalFromRecipeResult_NilResult(t *testing.T) {
	if p := ProposalFromRecipeResult("demo", nil, time.Now()); p != nil {
		t.Errorf("nil recipe result must yield nil proposal, got %+v", p)
	}
}

func TestCandidateFromRecipeResult(t *testing.T) {
	r := retryRecipeFixture(t)
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	p := ProposalFromRecipeResult("demo", r, now)
	trg := &persistence.HealingTrigger{ID: "trg1", ProjectID: "projX", WorkflowID: "demo"}

	cand := CandidateFromRecipeResult(trg, p, r)
	if cand == nil {
		t.Fatal("expected a candidate, got nil")
	}
	if cand.TriggerID != "trg1" || cand.ProjectID != "projX" || cand.WorkflowID != "demo" {
		t.Errorf("trigger linkage wrong: %+v", cand)
	}
	if cand.ProposalID != p.ID {
		t.Errorf("ProposalID = %q, want %q (must link the synthesized proposal)", cand.ProposalID, p.ID)
	}
	// The recipe's real class must survive — NOT the architect placeholder.
	if cand.CandidateClass != persistence.HealingCandidateRetryBudget {
		t.Errorf("CandidateClass = %q, want retry_budget", cand.CandidateClass)
	}
	if cand.CandidateGenomeHash != r.CandidateGenomeHash {
		t.Errorf("CandidateGenomeHash = %q, want %q", cand.CandidateGenomeHash, r.CandidateGenomeHash)
	}
	if cand.RiskLevel != r.RiskLevel {
		t.Errorf("RiskLevel = %q, want %q", cand.RiskLevel, r.RiskLevel)
	}
	if cand.ExpectedEffect != r.ExpectedEffect {
		t.Errorf("ExpectedEffect not carried from recipe")
	}
	if cand.Status != persistence.HealingCandidateDraft {
		t.Errorf("Status = %q, want draft", cand.Status)
	}
}

func TestCandidateFromRecipeResult_NilArgs(t *testing.T) {
	r := retryRecipeFixture(t)
	if CandidateFromRecipeResult(nil, &persistence.WorkflowProposal{ID: "x"}, r) != nil {
		t.Error("nil trigger must yield nil candidate")
	}
	if CandidateFromRecipeResult(&persistence.HealingTrigger{ID: "t"}, nil, r) != nil {
		t.Error("nil proposal must yield nil candidate")
	}
	if CandidateFromRecipeResult(&persistence.HealingTrigger{ID: "t"}, &persistence.WorkflowProposal{ID: "x"}, nil) != nil {
		t.Error("nil recipe result must yield nil candidate")
	}
}
