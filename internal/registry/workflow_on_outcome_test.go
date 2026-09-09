package registry

import (
	"strings"
	"testing"
)

// on_outcome routing and the inert-gates guard (design
// 2026-09-01-forge-rereview-triggers-design.md §17.3).

func outcomeWorkflow(steps map[string]WorkflowStep) *Workflow {
	return &Workflow{
		ID: "wf", DisplayName: "wf", Description: "d", Entrypoint: "fetch",
		Steps: steps,
		Terminals: map[string]WorkflowTerminal{
			"done":              {Status: "COMPLETED"},
			"nothing_to_review": {Status: "COMPLETED"},
			"failed":            {Status: "FAILED"},
		},
	}
}

func TestValidate_OnOutcomeAcceptsAKnownTarget(t *testing.T) {
	w := outcomeWorkflow(map[string]WorkflowStep{
		"fetch": {
			Type: "system", Handler: "forge.fetch_diff",
			OnSuccess: "done", OnFail: "failed",
			OnOutcome: map[string]string{"no-change": "nothing_to_review"},
		},
	})
	if err := w.Validate("wf.md"); err != nil {
		t.Fatalf("a valid on_outcome must load: %v", err)
	}
}

// An unknown target must fail the LOAD. Dead-ending at runtime is how a
// routing typo becomes a stuck task instead of a config error.
func TestValidate_OnOutcomeRejectsAnUnknownTarget(t *testing.T) {
	w := outcomeWorkflow(map[string]WorkflowStep{
		"fetch": {
			Type: "system", Handler: "forge.fetch_diff",
			OnSuccess: "done", OnFail: "failed",
			OnOutcome: map[string]string{"no-change": "typo_terminal"},
		},
	})
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "typo_terminal") {
		t.Fatalf("an unknown on_outcome target must fail the load, naming it: %v", err)
	}
}

func TestValidate_OnOutcomeRequiresASystemStep(t *testing.T) {
	w := outcomeWorkflow(map[string]WorkflowStep{
		"fetch": {
			Type: "agent", Role: "reviewer", Prompt: "go",
			OnSuccess: "done", OnFail: "failed",
			OnOutcome: map[string]string{"no-change": "nothing_to_review"},
		},
	})
	if err := w.Validate("wf.md"); err == nil {
		t.Fatal("on_outcome on an agent step must be rejected — there is no handler to report an outcome")
	}
}

// THE P1's SILENT KEY (§16.3). Two attempts to gate a system step parsed,
// validated, reloaded clean and did nothing, and the review the gate existed to
// prevent was posted both times. It must fail the load, and the message must
// name what to use instead.
func TestValidate_GatesOnASystemStepAreRejectedNotIgnored(t *testing.T) {
	w := outcomeWorkflow(map[string]WorkflowStep{
		"fetch": {
			Type: "system", Handler: "forge.fetch_diff", OnFail: "failed",
			Gates: []WorkflowGate{{Condition: "scope != 'no-change'", Target: "done"}},
		},
	})
	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal("a gates block on a system step must be REJECTED; accepting it in silence is the P1")
	}
	if !strings.Contains(err.Error(), "on_outcome") {
		t.Errorf("the error must point at on_outcome, or the author's next attempt is another dead one: %v", err)
	}
}

// Agent and gate steps are untouched: gates are what they are for.
func TestValidate_GatesStillWorkWhereTheyAreEvaluated(t *testing.T) {
	for _, typ := range []string{"agent", "gate"} {
		step := WorkflowStep{
			Type: typ, OnFail: "failed",
			Gates: []WorkflowGate{{Condition: "approved == true", Target: "done"}},
		}
		if typ == "agent" {
			step.Role, step.Prompt = "reviewer", "go"
		}
		if err := outcomeWorkflow(map[string]WorkflowStep{"fetch": step}).Validate("wf.md"); err != nil {
			t.Errorf("gates on a %q step must still validate: %v", typ, err)
		}
	}
}
