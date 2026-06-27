package api

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestScanWorkflowOnFail_DirectMaskingDetected — the canonical case:
// a step's on_fail points straight at a COMPLETED terminal. That's
// the research.yaml `on_fail: done` shape that motivated the check.
func TestScanWorkflowOnFail_DirectMaskingDetected(t *testing.T) {
	wf := &registry.Workflow{
		ID: "leaky",
		Steps: map[string]registry.WorkflowStep{
			"write": {OnSuccess: "done", OnFail: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
	got := scanWorkflowOnFail(wf)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %v", len(got), got)
	}
	for _, want := range []string{`workflow "leaky"`, `step "write"`, "COMPLETED", "masked"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("finding missing %q: %s", want, got[0])
		}
	}
}

// TestScanWorkflowOnFail_FailedTerminalAccepted — on_fail pointing
// at a FAILED terminal is the correct contract; must NOT be flagged.
func TestScanWorkflowOnFail_FailedTerminalAccepted(t *testing.T) {
	wf := &registry.Workflow{
		ID: "tight",
		Steps: map[string]registry.WorkflowStep{
			"write": {OnSuccess: "done", OnFail: "failed"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
	if got := scanWorkflowOnFail(wf); len(got) != 0 {
		t.Fatalf("expected 0 findings, got %d: %v", len(got), got)
	}
}

// TestScanWorkflowOnFail_TransitiveMaskingDetected — on_fail routes
// to another step which then on_success-s into a COMPLETED terminal.
// The original failure is still masked; flag it.
func TestScanWorkflowOnFail_TransitiveMaskingDetected(t *testing.T) {
	wf := &registry.Workflow{
		ID: "indirect",
		Steps: map[string]registry.WorkflowStep{
			"plan":  {OnSuccess: "write", OnFail: "write"},
			"write": {OnSuccess: "done", OnFail: "failed"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
	got := scanWorkflowOnFail(wf)
	if len(got) != 1 {
		t.Fatalf("expected plan→write→done masking flagged, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], `step "plan"`) {
		t.Errorf("expected plan to be flagged, got: %s", got[0])
	}
}

// TestScanWorkflowOnFail_RecoveryTerminalExempt — a COMPLETED terminal
// marked recovery:true reached via on_fail (through a recovery step) is
// an INTENTIONAL graceful-recovery exit (dev-pipeline's checkpoint), not
// masking — it must NOT be flagged, even though an unmarked COMPLETED
// terminal in the same shape would be.
func TestScanWorkflowOnFail_RecoveryTerminalExempt(t *testing.T) {
	wf := &registry.Workflow{
		ID: "dev-pipeline-like",
		Steps: map[string]registry.WorkflowStep{
			"implement":          {OnSuccess: "review", OnFail: "recover-checkpoint"},
			"recover-checkpoint": {OnSuccess: "checkpoint", OnFail: "failed"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"checkpoint": {Status: "COMPLETED", Recovery: true},
			"failed":     {Status: "FAILED"},
		},
	}
	if got := scanWorkflowOnFail(wf); len(got) != 0 {
		t.Errorf("recovery-marked terminal must be exempt, got: %v", got)
	}

	// Sanity: the SAME shape WITHOUT the recovery marker is still flagged.
	wf.Terminals["checkpoint"] = registry.WorkflowTerminal{Status: "COMPLETED"}
	if got := scanWorkflowOnFail(wf); len(got) != 1 {
		t.Errorf("unmarked COMPLETED terminal should still be flagged, got %d: %v", len(got), got)
	}
}

// TestScanWorkflowOnFail_NoOnFailIgnored — steps without an on_fail
// declaration are unaffected (the executor's default failure path
// kicks in, which propagates as a hard error).
func TestScanWorkflowOnFail_NoOnFailIgnored(t *testing.T) {
	wf := &registry.Workflow{
		ID: "implicit",
		Steps: map[string]registry.WorkflowStep{
			"write": {OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
	if got := scanWorkflowOnFail(wf); len(got) != 0 {
		t.Fatalf("absent on_fail should not be flagged, got: %v", got)
	}
}

// TestScanWorkflowOnFail_CycleSafe — pathological case: a cycle in
// on_success transitions. The hop counter must terminate without
// crashing or false-positive.
func TestScanWorkflowOnFail_CycleSafe(t *testing.T) {
	wf := &registry.Workflow{
		ID: "loop",
		Steps: map[string]registry.WorkflowStep{
			"a": {OnSuccess: "b", OnFail: "b"},
			"b": {OnSuccess: "a", OnFail: "a"},
		},
		Terminals: map[string]registry.WorkflowTerminal{},
	}
	// No COMPLETED terminal reachable; just verify it doesn't loop.
	got := scanWorkflowOnFail(wf)
	if len(got) != 0 {
		t.Fatalf("cycle without success terminal should produce no findings, got: %v", got)
	}
}
