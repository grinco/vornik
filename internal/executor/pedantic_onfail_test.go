package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

func recoveryWorkflow() *registry.Workflow {
	return &registry.Workflow{
		ID:         "dev-pipeline-like",
		Entrypoint: "test",
		Steps: map[string]registry.WorkflowStep{
			"test":               {Type: "agent", OnFail: "recover-checkpoint"},
			"recover-checkpoint": {Type: "agent", OnSuccess: "checkpoint"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"checkpoint": {Status: "COMPLETED", Recovery: true},
			"failed":     {Status: "FAILED"},
		},
	}
}

// Regression, measured 2026-08-18. pedantic has always been DOCUMENTED as
// disabling recovery routing — the daemon announces "on_fail goes straight to
// terminal" at every pedantic execution start, and dev-pipeline, research and
// publish all say the same in their shipped config. None of it was implemented:
// pedantic only made buildStepFailureRecovery return nil, so the recovery step
// still ran, minus the context.recovery it exists to read.
//
// The measured consequence: dev-pipeline under pedantic: true routed
// test -> recover-checkpoint -> COMPLETED on all three runs with its contract
// unmet, so a benchmark reading task status scored 3/3 for a workflow whose
// verification step had failed every attempt.
func TestPedanticOnFail_RecoveryHopBecomesTheFailedTerminal(t *testing.T) {
	got, changed := pedanticOnFail(recoveryWorkflow(), "recover-checkpoint")
	if !changed || got != "failed" {
		t.Errorf("pedanticOnFail = (%q, %v), want (\"failed\", true) — a recovery hop must "+
			"become the failed terminal, which is what the daemon has always announced", got, changed)
	}
}

// An on_fail already naming a TERMINAL is untouched, so a workflow that simply
// routes its failures to its own failed terminal behaves identically under
// pedantic. Without this, pedantic would rewrite transitions it has no business
// touching.
func TestPedanticOnFail_TerminalTargetIsUntouched(t *testing.T) {
	got, changed := pedanticOnFail(recoveryWorkflow(), "failed")
	if changed || got != "failed" {
		t.Errorf("pedanticOnFail = (%q, %v), want (\"failed\", false)", got, changed)
	}
}

// No failed terminal means there is nothing honest to redirect to. Leave the
// hop alone rather than inventing a terminal or dropping the transition.
func TestPedanticOnFail_NoFailedTerminalLeavesTheHop(t *testing.T) {
	wf := recoveryWorkflow()
	delete(wf.Terminals, "failed")
	got, changed := pedanticOnFail(wf, "recover-checkpoint")
	if changed || got != "recover-checkpoint" {
		t.Errorf("pedanticOnFail = (%q, %v), want the hop unchanged", got, changed)
	}
}

func TestPedanticOnFail_EmptyAndNilAreSafe(t *testing.T) {
	if got, changed := pedanticOnFail(recoveryWorkflow(), ""); got != "" || changed {
		t.Errorf("empty on_fail must stay empty, got (%q,%v)", got, changed)
	}
	if got, changed := pedanticOnFail(nil, "recover-checkpoint"); got != "recover-checkpoint" || changed {
		t.Errorf("nil workflow must be a no-op, got (%q,%v)", got, changed)
	}
}
