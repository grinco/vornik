package registry

import (
	"strings"
	"testing"
)

// These tests pin the "gates + on_success" footgun guard.
//
// Incident 2026-06-13 (issue-fix resume gate, https://docs.vornik.io
// incident-2026-06-13-issuefix-resume-gate.md): the `review` agent step
// set BOTH `on_success: failed` and inline gates. The executor's agent
// path (internal/executor/workflow.go) does `nextStepID := step.OnSuccess`
// and only evaluates gates when `nextStepID == "" && len(step.Gates) > 0`,
// so `on_success` silently shadowed the gates — the `approved → publish`
// gate became dead code and the task FAILED with no PR. The incident note
// flagged this as "a workflow-author footgun worth a future validation
// check"; this guard is that check.
//
// Scope: AGENT steps only. For `type: gate` steps the executor evaluates
// gates first and treats `on_success` as the legitimate default/fallback
// (see trading.md `maybe_execute`), so that combination must stay valid.

func TestValidate_AgentStepWithGatesAndOnSuccess_IsRejected(t *testing.T) {
	w := &Workflow{
		ID:         "issue-fix-like",
		Entrypoint: "review",
		Steps: map[string]WorkflowStep{
			"review": {
				Type:      "agent",
				Role:      "reviewer",
				OnSuccess: "failed", // shadows the gate below -> dead gate
				Gates: []WorkflowGate{
					{Condition: "review.approved == true", Target: "publish"},
				},
			},
			"publish": {Type: "agent", Role: "publisher", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}

	err := w.Validate("issue-fix-like.md")
	if err == nil {
		t.Fatal("expected validation error for agent step with both on_success and gates, got nil")
	}
	if !strings.Contains(err.Error(), "on_success") || !strings.Contains(err.Error(), "gates") {
		t.Errorf("error should name both on_success and gates, got: %v", err)
	}
	// The offending field should be identified for the operator.
	if verr, ok := err.(WorkflowValidationError); ok {
		if !strings.Contains(verr.Field, "review") {
			t.Errorf("expected field to reference the offending step 'review', got %q", verr.Field)
		}
	}
}

func TestValidate_GateStepWithGatesAndOnSuccess_IsAllowed(t *testing.T) {
	// Mirrors trading.md `maybe_execute`: a gate step routes via its gate
	// when the condition holds and falls back to on_success otherwise.
	w := &Workflow{
		ID:         "trading-like",
		Entrypoint: "maybe_execute",
		Steps: map[string]WorkflowStep{
			"maybe_execute": {
				Type:      "gate",
				OnSuccess: "done", // legitimate default/fallback for gate steps
				Gates: []WorkflowGate{
					{Condition: "has_approvals == true", Target: "execute"},
				},
			},
			"execute": {Type: "agent", Role: "executor", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}

	if err := w.Validate("trading-like.md"); err != nil {
		t.Fatalf("gate step with both gates and on_success must be valid, got: %v", err)
	}
}

func TestValidate_AgentStepWithGatesOnly_IsAllowed(t *testing.T) {
	// The post-incident issue-fix shape: agent step routes purely via
	// gates (+ on_fail catch-all), no on_success.
	w := &Workflow{
		ID:         "review-gates-only",
		Entrypoint: "review",
		Steps: map[string]WorkflowStep{
			"review": {
				Type:   "agent",
				Role:   "reviewer",
				OnFail: "failed",
				Gates: []WorkflowGate{
					{Condition: "review.approved == true", Target: "publish"},
					{Condition: "review.approved == false", Target: "failed"},
				},
			},
			"publish": {Type: "agent", Role: "publisher", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}

	if err := w.Validate("review-gates-only.md"); err != nil {
		t.Fatalf("agent step with gates only (no on_success) must be valid, got: %v", err)
	}
}
