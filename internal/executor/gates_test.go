package executor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/registry"
)

// TestEvaluateGateStepTraced_NoGatesErrors — a gate step without any
// gates is a config error.
func TestEvaluateGateStepTraced_NoGatesErrors(t *testing.T) {
	target, trace, err := evaluateGateStepTraced(registry.WorkflowStep{}, json.RawMessage(`{}`))
	assert.Empty(t, target)
	assert.Empty(t, trace.Entries)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no gates")
}

// TestEvaluateGateStepTraced_InvalidJSONErrors — the gate input is
// not valid JSON → return the parse error so the caller can route
// to step.OnFail.
func TestEvaluateGateStepTraced_InvalidJSONErrors(t *testing.T) {
	step := registry.WorkflowStep{
		Gates: []registry.WorkflowGate{
			{Condition: "approved == true", Target: "ok"},
		},
	}
	target, _, err := evaluateGateStepTraced(step, json.RawMessage(`{not-json`))
	assert.Empty(t, target)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse gate input")
}

// TestEvaluateGateStepTraced_MatchedFirstGate — first gate matches,
// returns its target without evaluating subsequent gates.
func TestEvaluateGateStepTraced_MatchedFirstGate(t *testing.T) {
	step := registry.WorkflowStep{
		Gates: []registry.WorkflowGate{
			{Condition: "approved == true", Target: "ok"},
			{Condition: "approved == false", Target: "rejected"},
		},
	}
	target, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"approved":true}`))
	require.NoError(t, err)
	assert.Equal(t, "ok", target)
	// Only first gate evaluated (second is the unmatched path).
	require.Len(t, trace.Entries, 1)
	assert.True(t, trace.Entries[0].Matched)
}

// TestEvaluateGateStepTraced_FallsThroughToSecondGate — first gate
// doesn't match, second one does.
func TestEvaluateGateStepTraced_FallsThroughToSecondGate(t *testing.T) {
	step := registry.WorkflowStep{
		Gates: []registry.WorkflowGate{
			{Condition: "approved == true", Target: "ok"},
			{Condition: "approved == false", Target: "rejected"},
		},
	}
	target, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"approved":false}`))
	require.NoError(t, err)
	assert.Equal(t, "rejected", target)
	require.Len(t, trace.Entries, 2)
	assert.False(t, trace.Entries[0].Matched)
	assert.True(t, trace.Entries[1].Matched)
}

// TestEvaluateGateStepTraced_NoMatchListsAllConditions — when nothing
// matches, the error mentions every condition (operator diagnostic).
func TestEvaluateGateStepTraced_NoMatchListsAllConditions(t *testing.T) {
	step := registry.WorkflowStep{
		Gates: []registry.WorkflowGate{
			{Condition: "approved == true", Target: "ok"},
			{Condition: "approved == false", Target: "rejected"},
		},
	}
	target, _, err := evaluateGateStepTraced(step, json.RawMessage(`{"approved":"maybe"}`))
	assert.Empty(t, target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approved == true")
	assert.Contains(t, err.Error(), "approved == false")
}

// TestEvaluateGateStepTraced_BadConditionErrors — a malformed gate
// condition surfaces the parse error and entry.Err.
func TestEvaluateGateStepTraced_BadConditionErrors(t *testing.T) {
	step := registry.WorkflowStep{
		Gates: []registry.WorkflowGate{
			{Condition: "malformed gate string", Target: "x"},
		},
	}
	target, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"approved":true}`))
	assert.Empty(t, target)
	require.Error(t, err)
	require.Len(t, trace.Entries, 1)
	assert.NotEmpty(t, trace.Entries[0].Err)
}
