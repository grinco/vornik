package workflowhealing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// Part 2 of deterministic-recipe live generation: the step-selection
// heuristic + the retry-budget candidate builder. The trigger carries no
// step id, so the offending step is inferred from per-step failure tallies
// across the trigger's evidence executions; the builder then runs
// RetryBudgetRecipe on it and synthesizes a promotable proposal+candidate,
// or returns ErrNoRecipeApplies so the caller falls back to the architect.

func TestFailuresByStep_CountsTerminalNonOKExcludingAuditCompanions(t *testing.T) {
	rows := []*persistence.ExecutionStepOutcome{
		{StepID: "impl", Outcome: string(stepoutcome.ParseError)},         // failure
		{StepID: "impl", Outcome: string(stepoutcome.DownstreamRejected)}, // failure
		{StepID: "impl", Outcome: string(stepoutcome.OK)},                 // not a failure
		{StepID: "impl", Outcome: string(stepoutcome.PendingValidation)},  // not terminal
		{StepID: "impl", Outcome: string(stepoutcome.Superseded)},         // audit companion
		{StepID: "impl", Outcome: string(stepoutcome.VerifierWarn)},       // advisory companion
		{StepID: "review", Outcome: string(stepoutcome.Failed)},           // failure
		nil,
	}
	got := FailuresByStep(rows)
	if got["impl"] != 2 {
		t.Errorf("impl failures = %d, want 2", got["impl"])
	}
	if got["review"] != 1 {
		t.Errorf("review failures = %d, want 1", got["review"])
	}
}

func TestSelectOffendingStep_PicksMaxFailures(t *testing.T) {
	step, ok := SelectOffendingStep(map[string]int{"a": 2, "impl": 7, "review": 3})
	if !ok {
		t.Fatal("expected a selection")
	}
	if step != "impl" {
		t.Errorf("offending step = %q, want impl (most failures)", step)
	}
}

func TestSelectOffendingStep_DeterministicTiebreak(t *testing.T) {
	// Two steps tied at the max → alphabetically smallest wins, regardless
	// of map iteration order.
	for i := 0; i < 20; i++ {
		step, ok := SelectOffendingStep(map[string]int{"zeta": 4, "alpha": 4, "mid": 1})
		if !ok || step != "alpha" {
			t.Fatalf("tie must resolve to alpha deterministically, got %q ok=%v", step, ok)
		}
	}
}

func TestSelectOffendingStep_EmptyOrAllZero(t *testing.T) {
	if _, ok := SelectOffendingStep(nil); ok {
		t.Error("nil tally must not select")
	}
	if _, ok := SelectOffendingStep(map[string]int{"a": 0, "b": 0}); ok {
		t.Error("all-zero tally must not select")
	}
}

// twoStepRetryWorkflow parses a validator-clean baseline (version +
// prompt bodies) where `impl` carries a retry budget of 4 and `review`
// none — mirrors how the production path loads a parsed workflow from the
// registry, so finalizeRecipe's WORKFLOW.md validation passes.
const twoStepRetryMD = `---
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
    status: "success"
  failed:
    status: "failed"
---

# Demo

## Prompts

### impl

Implement the change.

### review

Review the change.
`

func twoStepRetryWorkflow(t *testing.T) *registry.Workflow {
	t.Helper()
	wf, err := registry.ParseWorkflowMarkdown([]byte(twoStepRetryMD), "demo.md")
	if err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	return wf
}

func TestVerifierFailuresByStep_TalliesVerifierWarn(t *testing.T) {
	rows := []*persistence.ExecutionStepOutcome{
		{StepID: "write", Outcome: string(stepoutcome.VerifierWarn)},
		{StepID: "write", Outcome: string(stepoutcome.VerifierWarn)},
		{StepID: "write", Outcome: string(stepoutcome.OK)},      // not a verifier warn
		{StepID: "review", Outcome: string(stepoutcome.Failed)}, // a hard failure, not verifier
		nil,
	}
	got := VerifierFailuresByStep(rows)
	if got["write"] != 2 {
		t.Errorf("write verifier failures = %d, want 2", got["write"])
	}
	if _, ok := got["review"]; ok {
		t.Errorf("hard failures must not count as verifier failures: %v", got)
	}
}

func TestBuildVerifierInsertionCandidate(t *testing.T) {
	base := parseRecipeBaseline(t) // has write→review and a declared "verifier" role
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: base.ID, EvidenceExecutionIDs: []string{"e1"}}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	// No verifier failures → no recipe.
	if _, _, err := BuildVerifierInsertionCandidate(base, trg, map[string]int{}, "verifier", now); !errors.Is(err, ErrNoRecipeApplies) {
		t.Fatalf("empty failures: err = %v, want ErrNoRecipeApplies", err)
	}

	// Verifier failures on "write" (which has on_success) → a candidate.
	proposal, cand, err := BuildVerifierInsertionCandidate(base, trg, map[string]int{"write": 3}, "verifier", now)
	if err != nil {
		t.Fatalf("BuildVerifierInsertionCandidate: %v", err)
	}
	if proposal == nil || cand == nil {
		t.Fatal("expected non-nil proposal + candidate")
	}
	if cand.CandidateClass != persistence.HealingCandidateVerifierInsertion {
		t.Errorf("CandidateClass = %q, want verifier_insertion", cand.CandidateClass)
	}
	if cand.ProposalID != proposal.ID {
		t.Error("candidate must link the synthesized proposal")
	}

	// Empty verifier role → no recipe (caller falls back).
	if _, _, err := BuildVerifierInsertionCandidate(base, trg, map[string]int{"write": 3}, "", now); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("empty role: err = %v, want ErrNoRecipeApplies", err)
	}
}

func TestBuildRetryBudgetCandidate_LowersOffendingStep(t *testing.T) {
	base := twoStepRetryWorkflow(t)
	trg := &persistence.HealingTrigger{ID: "trg1", ProjectID: "projX", WorkflowID: "demo", EvidenceExecutionIDs: []string{"e1", "e2"}}
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	proposal, cand, err := BuildRetryBudgetCandidate(base, trg, map[string]int{"impl": 5, "review": 1}, now)
	if err != nil {
		t.Fatalf("expected a candidate, got err: %v", err)
	}
	if proposal == nil || cand == nil {
		t.Fatal("expected non-nil proposal and candidate")
	}
	if cand.CandidateClass != persistence.HealingCandidateRetryBudget {
		t.Errorf("CandidateClass = %q, want retry_budget", cand.CandidateClass)
	}
	if cand.ProposalID != proposal.ID {
		t.Errorf("candidate must link the synthesized proposal")
	}
	// The recipe targets the max-failure step ("impl").
	if !strings.Contains(cand.Motivation, "impl") {
		t.Errorf("motivation should name the offending step impl: %q", cand.Motivation)
	}
	// The candidate genome must actually carry impl's lowered budget (4/2=2).
	cw, err := registry.ParseWorkflowMarkdown([]byte(proposal.ProposalYAML), "demo.md")
	if err != nil {
		t.Fatalf("re-parse candidate genome: %v", err)
	}
	if got := cw.Steps["impl"].RetryPolicy.MaxRetries; got != 2 {
		t.Errorf("impl maxRetries = %d, want 2 (lowered from 4)", got)
	}
	// review (not the offending step) must be untouched.
	if got := cw.Steps["review"].RetryPolicy.MaxRetries; got != 0 {
		t.Errorf("review maxRetries = %d, want 0 (untouched)", got)
	}
}

func TestBuildRetryBudgetCandidate_NoRetriesFallsBack(t *testing.T) {
	base := twoStepRetryWorkflow(t)
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: "demo"}
	// "review" has MaxRetries=0 → nothing to lower → fall back to architect.
	_, _, err := BuildRetryBudgetCandidate(base, trg, map[string]int{"review": 9}, time.Now())
	if !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("err = %v, want ErrNoRecipeApplies", err)
	}
}

func TestBuildRetryBudgetCandidate_NoFailuresFallsBack(t *testing.T) {
	base := twoStepRetryWorkflow(t)
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: "demo"}
	if _, _, err := BuildRetryBudgetCandidate(base, trg, nil, time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("empty tally err = %v, want ErrNoRecipeApplies", err)
	}
}

func TestBuildRetryBudgetCandidate_NilBaseline(t *testing.T) {
	if _, _, err := BuildRetryBudgetCandidate(nil, &persistence.HealingTrigger{}, map[string]int{"x": 1}, time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("nil baseline err = %v, want ErrNoRecipeApplies", err)
	}
}
