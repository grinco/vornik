package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// Observed 2026-09-15 on task_20260915020853_5cdd2cf3345dfa84: a
// companion-architectural-review with 248 KB of staged artifacts ran four
// iterations, spent about $0.27, hit `prompt-token budget stop`, and then
// failed with
//
//	schema violation: output contract for step "review_model_fallback" not met
//	— no file matching artifacts/out/review.md was written
//
// The message names the missing file, which reads as a model-compliance
// problem — "you MUST write the declared output file" — when the log two lines
// up says the agent never had the budget to reach a final answer. The caller
// sees the consequence and not the cause, and can only find the cause by
// reading the container log.
//
// The daemon holds the cause at the moment it raises the violation: the agent
// writes agentOutcome=prompt_token_budget with its detail into result.json.

func contractStep() *registry.WorkflowStep {
	return &registry.WorkflowStep{RequireOutputGlob: "artifacts/out/review.md"}
}

func TestOutputFileContract_LeadsWithTheBudgetStop(t *testing.T) {
	e := &Executor{}
	in := &StepOutcome{
		StepID:    "review_model_fallback",
		Step:      contractStep(),
		StepStart: time.Now(),
		ResultBytes: []byte(`{"agentOutcome":"prompt_token_budget",` +
			`"agentOutcomeDetail":"step prompt-token budget 100000 would be exceeded again before a final answer; cumulative_prompt_tokens=93346"}`),
		WorkspaceDir: t.TempDir(),
		ProjectDir:   t.TempDir(),
	}

	v := e.outputFileContractParticipant(context.Background(), in)
	if !v.Refused {
		t.Fatal("a missing declared output must still refuse")
	}
	// The cause leads.
	if !strings.HasPrefix(v.Reason, "prompt-token budget exhausted") {
		t.Fatalf("the reason does not lead with the cause: %q", v.Reason)
	}
	// The consequence is still there — a caller needs to know which file is
	// missing to act on it.
	if !strings.Contains(v.Reason, "artifacts/out/review.md") {
		t.Fatalf("the reason dropped the missing file: %q", v.Reason)
	}
	// The agent's own numbers travel with it, so the caller can size the split
	// without opening a container log.
	if !strings.Contains(v.Reason, "93346") {
		t.Fatalf("the reason dropped the agent's detail: %q", v.Reason)
	}
	// And the compliance instruction must NOT be there: telling a model to try
	// harder is wrong advice when it ran out of budget.
	if strings.Contains(v.Reason, "You MUST write") {
		t.Fatalf("a budget stop still carries the compliance instruction: %q", v.Reason)
	}
}

// The other two bails are the same shape and get the same treatment.
func TestOutputFileContract_LeadsWithOtherAgentBails(t *testing.T) {
	for _, tc := range []struct{ outcome, want string }{
		{"budget_tripwire", "cost budget"},
		{"iteration_exhausted", "iteration cap"},
	} {
		e := &Executor{}
		in := &StepOutcome{
			StepID:       "review",
			Step:         contractStep(),
			StepStart:    time.Now(),
			ResultBytes:  []byte(`{"agentOutcome":"` + tc.outcome + `","agentOutcomeDetail":"d"}`),
			WorkspaceDir: t.TempDir(),
			ProjectDir:   t.TempDir(),
		}
		v := e.outputFileContractParticipant(context.Background(), in)
		if !v.Refused || !strings.Contains(v.Reason, tc.want) {
			t.Fatalf("%s: want a reason naming %q, got %q", tc.outcome, tc.want, v.Reason)
		}
	}
}

// A step that simply did not write the file — no bail recorded — keeps the
// original message, instruction and all. That case IS a compliance problem,
// and softening it would lose the thing the contract exists to say.
func TestOutputFileContract_PlainMissIsUnchanged(t *testing.T) {
	e := &Executor{}
	in := &StepOutcome{
		StepID:       "review",
		Step:         contractStep(),
		StepStart:    time.Now(),
		ResultBytes:  []byte(`{"response":"all done"}`),
		WorkspaceDir: t.TempDir(),
		ProjectDir:   t.TempDir(),
	}
	v := e.outputFileContractParticipant(context.Background(), in)
	if !v.Refused {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(v.Reason, "You MUST write the declared output file") {
		t.Fatalf("the plain miss lost its instruction: %q", v.Reason)
	}
	if strings.Contains(v.Reason, "budget") {
		t.Fatalf("a plain miss invented a budget cause: %q", v.Reason)
	}
}
