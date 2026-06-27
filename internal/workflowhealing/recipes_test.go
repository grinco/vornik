package workflowhealing

import (
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// recipeWorkflowMD is a validator-clean WORKFLOW.md (carries the
// required description + version) with a retry-bearing writer step and
// a writer→reviewer chain — the two shapes the recipes target.
const recipeWorkflowMD = `---
workflowId: "dev-pipeline"
displayName: "Dev Pipeline"
description: "Writes then reviews a change, then deploys."
version: "1.0"
entrypoint: "write"
maxStepVisits: 3
maxWallClock: "1h"
steps:
  write:
    type: "agent"
    role: "writer"
    on_success: "review"
    retryPolicy:
      maxRetries: 5
  review:
    type: "agent"
    role: "reviewer"
    on_success: "deploy"
    on_fail: "failed"
  deploy:
    type: "agent"
    role: "deployer"
    on_success: "complete"
    on_fail: "failed"
terminals:
  complete:
    status: "success"
  failed:
    status: "failed"
---

# Dev Pipeline

## Prompts

### write

Write the change.

### review

Review the change.

### deploy

Deploy the change.
`

func parseRecipeBaseline(t *testing.T) *registry.Workflow {
	t.Helper()
	wf, err := registry.ParseWorkflowMarkdown([]byte(recipeWorkflowMD), "dev-pipeline.md")
	if err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	return wf
}

func TestRetryBudgetRecipe_LowersBudgetAndKeepsValid(t *testing.T) {
	base := parseRecipeBaseline(t)
	res, err := RetryBudgetRecipe(base, "write", 1, "failed", []string{"exec_a", "exec_b", "exec_c"})
	if err != nil {
		t.Fatalf("RetryBudgetRecipe: %v", err)
	}
	if res.CandidateClass != persistence.HealingCandidateRetryBudget {
		t.Errorf("class = %q, want retry_budget", res.CandidateClass)
	}
	if !res.Valid {
		t.Fatalf("candidate should be valid; findings: %v", res.ValidationFindings)
	}
	if got := res.CandidateWorkflow.Steps["write"].RetryPolicy.MaxRetries; got != 1 {
		t.Errorf("maxRetries = %d, want 1", got)
	}
	// on_fail should now be set (write had none).
	if got := res.CandidateWorkflow.Steps["write"].OnFail; got != "failed" {
		t.Errorf("write.on_fail = %q, want failed", got)
	}
	if res.BaselineGenomeHash == res.CandidateGenomeHash {
		t.Error("genome hash should change after the budget cut")
	}
	if res.RiskLevel != persistence.HealingRiskLow {
		t.Errorf("risk = %q, want low", res.RiskLevel)
	}
	if len(res.EvidenceExecutionIDs) != 3 {
		t.Errorf("evidence len = %d, want 3", len(res.EvidenceExecutionIDs))
	}
	// The baseline must be untouched (recipe operates on a clone).
	if base.Steps["write"].RetryPolicy.MaxRetries != 5 {
		t.Error("recipe mutated the baseline workflow")
	}
	// The serialized diff must round-trip back to the candidate hash.
	h, err := GenomeHashFromMarkdown([]byte(res.ProposalDiff), "dev-pipeline.md")
	if err != nil {
		t.Fatalf("re-parse diff: %v", err)
	}
	if h != res.CandidateGenomeHash {
		t.Errorf("diff re-parse hash %q != candidate hash %q", h, res.CandidateGenomeHash)
	}
}

func TestRetryBudgetRecipe_LowerOnlyClamp(t *testing.T) {
	base := parseRecipeBaseline(t)
	// Ask for a HIGHER budget than the current 5 — must be clamped to 5,
	// so the only structural change is the on_fail transition.
	res, err := RetryBudgetRecipe(base, "write", 99, "failed", nil)
	if err != nil {
		t.Fatalf("RetryBudgetRecipe: %v", err)
	}
	if got := res.CandidateWorkflow.Steps["write"].RetryPolicy.MaxRetries; got != 5 {
		t.Errorf("maxRetries = %d, want clamped to 5 (never raised)", got)
	}
}

func TestRetryBudgetRecipe_NoChangeIsError(t *testing.T) {
	base := parseRecipeBaseline(t)
	// review already has on_fail=failed and maxRetries=0; asking to keep
	// budget at its current 0 with the existing transition is a no-op.
	_, err := RetryBudgetRecipe(base, "review", 0, "failed", nil)
	if !errors.Is(err, ErrRecipeNoChange) {
		t.Errorf("err = %v, want ErrRecipeNoChange", err)
	}
}

func TestRetryBudgetRecipe_MissingStep(t *testing.T) {
	base := parseRecipeBaseline(t)
	_, err := RetryBudgetRecipe(base, "ghost", 1, "failed", nil)
	if !errors.Is(err, ErrRecipeStepNotFound) {
		t.Errorf("err = %v, want ErrRecipeStepNotFound", err)
	}
}

func TestRetryBudgetRecipe_OnFailTargetMustExist(t *testing.T) {
	base := parseRecipeBaseline(t)
	_, err := RetryBudgetRecipe(base, "write", 1, "nowhere", nil)
	if !errors.Is(err, ErrRecipeStepNotFound) {
		t.Errorf("err = %v, want ErrRecipeStepNotFound for bogus on_fail target", err)
	}
}

func TestRetryBudgetRecipe_NilWorkflow(t *testing.T) {
	if _, err := RetryBudgetRecipe(nil, "write", 1, "", nil); !errors.Is(err, ErrRecipeNilWorkflow) {
		t.Errorf("err = %v, want ErrRecipeNilWorkflow", err)
	}
}

func TestVerifierInsertionRecipe_InsertsAndRewires(t *testing.T) {
	base := parseRecipeBaseline(t)
	res, err := VerifierInsertionRecipe(base, "write", "verifier", "", []string{"exec_a", "exec_b", "exec_c"})
	if err != nil {
		t.Fatalf("VerifierInsertionRecipe: %v", err)
	}
	if res.CandidateClass != persistence.HealingCandidateVerifierInsertion {
		t.Errorf("class = %q, want verifier_insertion", res.CandidateClass)
	}
	if !res.Valid {
		t.Fatalf("candidate should be valid; findings: %v", res.ValidationFindings)
	}
	// Default verifier step id is "<afterStep>_verify".
	v, ok := res.CandidateWorkflow.Steps["write_verify"]
	if !ok {
		t.Fatal("verifier step write_verify was not inserted")
	}
	if v.Role != "verifier" {
		t.Errorf("verifier role = %q, want verifier", v.Role)
	}
	// Verifier inherits the writer's original success target.
	if v.OnSuccess != "review" {
		t.Errorf("verifier.on_success = %q, want review", v.OnSuccess)
	}
	// write is rewired to gate through the verifier.
	if got := res.CandidateWorkflow.Steps["write"].OnSuccess; got != "write_verify" {
		t.Errorf("write.on_success = %q, want write_verify", got)
	}
	if res.RiskLevel != persistence.HealingRiskMedium {
		t.Errorf("risk = %q, want medium", res.RiskLevel)
	}
	if res.BaselineGenomeHash == res.CandidateGenomeHash {
		t.Error("genome hash should change after insertion")
	}
	// No prompt-body rewriting: the writer's prompt is untouched.
	if res.CandidateWorkflow.Steps["write"].Prompt != base.Steps["write"].Prompt {
		t.Error("recipe altered the writer's prompt body (prohibited)")
	}
	// Baseline untouched.
	if _, leaked := base.Steps["write_verify"]; leaked {
		t.Error("recipe mutated the baseline (inserted step leaked)")
	}
}

func TestVerifierInsertionRecipe_RequiresOnSuccessTarget(t *testing.T) {
	base := parseRecipeBaseline(t)
	// deploy -> complete is a terminal, but on_success is set; instead
	// fabricate a step with no on_success by clearing it.
	s := base.Steps["deploy"]
	s.OnSuccess = ""
	base.Steps["deploy"] = s
	_, err := VerifierInsertionRecipe(base, "deploy", "verifier", "", nil)
	if !errors.Is(err, ErrRecipeNoChange) {
		t.Errorf("err = %v, want ErrRecipeNoChange when anchor has no on_success", err)
	}
}

func TestVerifierInsertionRecipe_MissingAnchor(t *testing.T) {
	base := parseRecipeBaseline(t)
	_, err := VerifierInsertionRecipe(base, "ghost", "verifier", "", nil)
	if !errors.Is(err, ErrRecipeStepNotFound) {
		t.Errorf("err = %v, want ErrRecipeStepNotFound", err)
	}
}

func TestVerifierInsertionRecipe_RequiresRole(t *testing.T) {
	base := parseRecipeBaseline(t)
	if _, err := VerifierInsertionRecipe(base, "write", "", "", nil); err == nil {
		t.Error("expected error when verifierRole is empty")
	}
}

func TestVerifierInsertionRecipe_StepIDClash(t *testing.T) {
	base := parseRecipeBaseline(t)
	if _, err := VerifierInsertionRecipe(base, "write", "verifier", "review", nil); err == nil {
		t.Error("expected error when verifier step id collides with an existing step")
	}
}

func TestVerifierInsertionRecipe_NilWorkflow(t *testing.T) {
	if _, err := VerifierInsertionRecipe(nil, "write", "verifier", "", nil); !errors.Is(err, ErrRecipeNilWorkflow) {
		t.Errorf("err = %v, want ErrRecipeNilWorkflow", err)
	}
}

// TestRecipe_InvalidGenomeRecordsFindings asserts a recipe that
// produces a structurally-invalid genome returns the result with
// Valid=false and findings rather than a Go error. We force this by
// targeting a baseline missing its description (validator ERROR).
func TestRecipe_InvalidGenomeRecordsFindings(t *testing.T) {
	wf, err := registry.ParseWorkflowMarkdown([]byte(validWorkflowMD), "heal.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// validWorkflowMD (from genome_test.go) has no description, so the
	// serialized candidate trips description_missing.
	wf.Steps["plan"] = registry.WorkflowStep{
		Type: "agent", Role: "lead", OnSuccess: "complete", OnFail: "failed",
		Prompt:      "Plan the work and hand off.",
		RetryPolicy: registry.WorkflowRetryPolicy{MaxRetries: 3},
	}
	res, err := RetryBudgetRecipe(wf, "plan", 0, "", nil)
	if err != nil {
		t.Fatalf("RetryBudgetRecipe: %v", err)
	}
	if res.Valid {
		t.Fatal("expected Valid=false for a description-less genome")
	}
	if len(res.ValidationFindings) == 0 {
		t.Error("expected validation findings to be populated")
	}
	joined := strings.Join(res.ValidationFindings, " ")
	if !strings.Contains(joined, "description") {
		t.Errorf("findings %q should mention the missing description", joined)
	}
}
