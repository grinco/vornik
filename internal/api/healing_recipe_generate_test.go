package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// stubStepOutcomeRepo is a minimal ExecutionStepOutcomeRepository for the
// recipe-generation handler test: only List is meaningful (returns a fixed
// failure set), the rest satisfy the interface.
type stubStepOutcomeRepo struct {
	rows []*persistence.ExecutionStepOutcome
}

func (s *stubStepOutcomeRepo) Record(context.Context, *persistence.ExecutionStepOutcome) error {
	return nil
}
func (s *stubStepOutcomeRepo) Finalize(context.Context, string, string, string, string, *string) error {
	return nil
}
func (s *stubStepOutcomeRepo) FinalizePending(context.Context, string, string, string, string, string, *string) (string, string, error) {
	return "", "", nil
}
func (s *stubStepOutcomeRepo) SweepPending(context.Context, string, string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (s *stubStepOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return s.rows, nil
}
func (s *stubStepOutcomeRepo) SupersedeAfter(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}
func (s *stubStepOutcomeRepo) CountByRoleModelOutcome(context.Context, string, time.Time, time.Time, string) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, nil
}

const recipeGenDemoMD = `---
workflowId: "demo"
displayName: "Demo"
description: "Implements then reviews a change."
version: "1.0"
entrypoint: "impl"
maxStepVisits: 3
maxWallClock: "1h"
steps:
  impl:
    type: "agent"
    role: "coder"
    on_success: "review"
    retryPolicy:
      maxRetries: 4
  review:
    type: "agent"
    role: "reviewer"
    on_success: "done"
    on_fail: "failed"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
---

# Demo

## Prompts

### impl

Implement the change.

### review

Review the change.
`

// TestHealingTriggerGenerateCandidate_RecipeFirst proves the deterministic
// recipe path: with the registry + step-outcome + proposal repos wired and the
// offending step (impl) showing failures, generate-candidate produces a
// retry_budget candidate WITHOUT calling the architect (none is wired).
func TestHealingTriggerGenerateCandidate_RecipeFirst(t *testing.T) {
	reg := registry.New()
	wf, err := registry.ParseWorkflowMarkdown([]byte(recipeGenDemoMD), "demo.md")
	if err != nil {
		t.Fatalf("parse demo workflow: %v", err)
	}
	if err := reg.RegisterTransient("demo", wf); err != nil {
		t.Fatalf("register demo workflow: %v", err)
	}

	triggerRepo := newAPIStubHealingTriggerRepo()
	trg := apiOpenTrigger("t-1")
	trg.WorkflowID = "demo"
	trg.EvidenceExecutionIDs = []string{"e1"}
	_ = triggerRepo.Insert(context.Background(), trg)

	outcomes := &stubStepOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
		{ExecutionID: "e1", StepID: "impl", Outcome: string(stepoutcome.ParseError)},
		{ExecutionID: "e1", StepID: "impl", Outcome: string(stepoutcome.DownstreamRejected)},
		{ExecutionID: "e1", StepID: "review", Outcome: string(stepoutcome.OK)},
	}}
	candRepo := newAPIStubHealingCandidateRepo()

	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(triggerRepo),
		WithHealingCandidateRepository(candRepo),
		WithProjectRegistry(reg),
		WithWorkflowProposals(&stubProposalRepo{}),
		WithExecutionStepOutcomeRepository(outcomes),
		// Deliberately NO WithWorkflowArchitect — the recipe path must
		// satisfy the request on its own.
	)
	s := NewServer(opts...)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The trigger was stamped with the recipe-synthesized proposal id.
	if triggerRepo.lastMarkGen.id != "t-1" {
		t.Errorf("MarkGenerated trigger id = %q, want t-1", triggerRepo.lastMarkGen.id)
	}
	if pid := triggerRepo.lastMarkGen.proposalID; len(pid) < 4 || pid[:4] != "wpr_" {
		t.Errorf("proposal id = %q, want wpr_ prefix (recipe-synthesized)", pid)
	}
	// A retry_budget candidate was persisted (NOT architect).
	if len(candRepo.inserted) != 1 {
		t.Fatalf("candidate inserts = %d, want 1", len(candRepo.inserted))
	}
	if got := candRepo.inserted[0].CandidateClass; got != persistence.HealingCandidateRetryBudget {
		t.Errorf("candidate class = %q, want retry_budget", got)
	}
	if candRepo.inserted[0].ProposalID != triggerRepo.lastMarkGen.proposalID {
		t.Errorf("candidate must link the stamped proposal")
	}
}

// TestHealingTriggerGenerateCandidate_NoArchitectNoRecipe — with neither a
// recipe (no step-outcome repo) nor an architect wired, the handler returns
// 503 rather than silently doing nothing.
func TestHealingTriggerGenerateCandidate_NoArchitectNoRecipe(t *testing.T) {
	triggerRepo := newAPIStubHealingTriggerRepo()
	_ = triggerRepo.Insert(context.Background(), apiOpenTrigger("t-1"))

	opts := append(adminAuthOpts(), WithHealingTriggerRepository(triggerRepo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no recipe, no architect)", rec.Code)
	}
}
