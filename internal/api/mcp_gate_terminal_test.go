package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/registry"
)

// The MCP fail-open census reported "no_step_role" for three different states:
// a step that exists and names no role, an execution parked at a TERMINAL, and
// a step id that is neither.
//
// Measured on the reference deployment 2026-09-04, the whole distribution — 12
// of 12 across both the call and advertise paths — was the middle one:
// executions sitting at `delegated` or `done`, which are adaptive's TERMINALS.
// Under one reason that reads as "the gate could not resolve a role twelve
// times", which is not what happened — and it is the bucket the container-
// enforcement decision was supposed to be sized from.
func TestStepForGate_SeparatesTerminalFromUnresolved(t *testing.T) {
	wf := &registry.Workflow{
		ID:         "adaptive",
		Entrypoint: "route",
		Steps: map[string]registry.WorkflowStep{
			"route": {Type: "agent", Role: "lead"},
			"index": {Type: "system", Handler: "rag.index"}, // no role, by design
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"delegated": {Status: "COMPLETED"},
			"failed":    {Status: "FAILED"},
		},
	}

	for _, tc := range []struct {
		name     string
		stepID   string
		want     mcpGapReason
		wantRole string
	}{
		{"a terminal means the execution finished, not that a role was unresolved", "delegated", mcpGapExecutionTerminal, ""},
		{"the failed terminal too", "failed", mcpGapExecutionTerminal, ""},
		{"a real step with no role is a system step", "index", mcpGapNoStepRole, ""},
		{"neither step nor terminal is the state worth alerting on", "route_infra_retry1", mcpGapStepNotFound, ""},
		{"a resolvable agent step yields the step and no gap", "route", mcpGapNone, "lead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step, reason := stepForGate(wf, tc.stepID)
			require.Equal(t, tc.want, reason)
			require.Equal(t, tc.wantRole, step.Role)
		})
	}
}

// Every reason the gate can return must be enumerated, or an alert wired from
// the enumeration silently never fires for it.
func TestAllMCPGapReasons_CoversTheNewSplit(t *testing.T) {
	got := map[mcpGapReason]bool{}
	for _, r := range allMCPGapReasons() {
		got[r] = true
	}
	require.True(t, got[mcpGapExecutionTerminal], "execution_terminal must be enumerated")
	require.True(t, got[mcpGapStepNotFound], "step_not_found must be enumerated")
}
