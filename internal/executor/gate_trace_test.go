package executor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestEvaluateGateStepTraced_NoGatesConfigured — a gate step with
// an empty Gates slice is a config error. Returns "" + err and a
// zero-trace (no per-condition entries).
func TestEvaluateGateStepTraced_NoGates(t *testing.T) {
	step := registry.WorkflowStep{Gates: nil}
	target, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"x":1}`))
	require.Error(t, err)
	assert.Equal(t, "", target)
	assert.Empty(t, trace.Entries)
}

// TestEvaluateGateStepTraced_FirstGateMatches — happy path: the
// first gate matches and the loop short-circuits. The trace
// contains exactly one entry (for the matching gate).
func TestEvaluateGateStepTraced_FirstMatch(t *testing.T) {
	step := registry.WorkflowStep{Gates: []registry.WorkflowGate{
		{Condition: `status == "ok"`, Target: "happy"},
		{Condition: `status == "fail"`, Target: "sad"}, // never reached
	}}
	target, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"status":"ok"}`))
	require.NoError(t, err)
	assert.Equal(t, "happy", target)
	require.Len(t, trace.Entries, 1, "matching gate must short-circuit; only one trace entry")
	assert.True(t, trace.Entries[0].Matched)
	assert.Equal(t, "happy", trace.Entries[0].Target)
}

// TestEvaluateGateStepTraced_NoneMatch — walks every gate, none
// match. The diagnostic error names the conditions and the raw
// preview so the operator can see what the producer actually
// emitted vs. what the gate expected.
func TestEvaluateGateStepTraced_NoneMatch(t *testing.T) {
	step := registry.WorkflowStep{Gates: []registry.WorkflowGate{
		{Condition: `status == "ok"`, Target: "happy"},
		{Condition: `status == "fail"`, Target: "sad"},
	}}
	target, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"status":"weird"}`))
	require.Error(t, err)
	assert.Equal(t, "", target)
	assert.Contains(t, err.Error(), `status == "ok"`)
	assert.Contains(t, err.Error(), `status == "fail"`)
	assert.Contains(t, err.Error(), "weird")
	require.Len(t, trace.Entries, 2, "non-matching gates produce one trace entry each")
	for _, ent := range trace.Entries {
		assert.False(t, ent.Matched)
	}
}

// TestEvaluateGateStepTraced_InvalidJSON — the input isn't JSON
// at all. Returns a structured error so the operator sees
// "agent's output wasn't JSON" not "no gate matched".
func TestEvaluateGateStepTraced_InvalidJSON(t *testing.T) {
	step := registry.WorkflowStep{Gates: []registry.WorkflowGate{
		{Condition: `status == "ok"`, Target: "happy"},
	}}
	_, _, err := evaluateGateStepTraced(step, json.RawMessage(`not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse gate input as JSON")
}

// TestEvaluateGateStepTraced_EmptyResultPayload — when the
// producer emitted no JSON body, the gate still evaluates
// against a nil payload (every "X == true" condition resolves
// to false because nothing's there). Used by the
// "agent didn't produce result.json" path.
func TestEvaluateGateStepTraced_EmptyResult(t *testing.T) {
	step := registry.WorkflowStep{Gates: []registry.WorkflowGate{
		{Condition: `approved == true`, Target: "done"},
	}}
	target, _, err := evaluateGateStepTraced(step, nil)
	require.Error(t, err, "no gate matches against nil payload — error surfaces 'no condition matched'")
	assert.Equal(t, "", target)
}

// TestEvaluateGateStepTraced_BadConditionSyntax — gate carries a
// condition the evaluator can't parse (unsupported operator).
// Returns the evaluator's error directly so the operator sees
// "unsupported" not "no gate matched".
func TestEvaluateGateStepTraced_BadConditionSyntax(t *testing.T) {
	step := registry.WorkflowStep{Gates: []registry.WorkflowGate{
		{Condition: `score > 5`, Target: "high"},
	}}
	_, trace, err := evaluateGateStepTraced(step, json.RawMessage(`{"score":10}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
	require.Len(t, trace.Entries, 1)
	assert.NotEmpty(t, trace.Entries[0].Err)
}

// TestLogGateTrace_NoEntriesNoError_Skipped — when there are no
// trace entries AND no error, the logger short-circuits without
// emitting anything (avoids zero-content log spam on workflows
// without gate steps).
func TestLogGateTrace_NoEntriesNoError(t *testing.T) {
	buf := &writableBuffer{}
	e := &Executor{logger: zerolog.New(buf)}
	e.logGateTrace(context.Background(),
		&persistence.Task{ID: "t"},
		&persistence.Execution{ID: "x"},
		"step",
		GateEvalTrace{},
		nil,
		"next")
	assert.Empty(t, buf.String(),
		"empty trace + no error must produce zero log lines")
}

// TestLogGateTrace_SuccessEmitsInfo — successful gate match
// emits an Info-level "gate_matched" line carrying gate count,
// task/exec ids, the matched target, and the per-entry trace.
func TestLogGateTrace_Success(t *testing.T) {
	buf := &writableBuffer{}
	e := &Executor{logger: zerolog.New(buf)}
	trace := GateEvalTrace{
		RawPreview: `{"status":"ok"}`,
		Entries: []GateEvalEntry{
			{
				Condition: `status == "ok"`,
				Target:    "happy",
				Matched:   true,
				Observed:  "ok",
				Found:     true,
				Wanted:    "ok",
			},
		},
	}
	e.logGateTrace(context.Background(),
		&persistence.Task{ID: "tk"},
		&persistence.Execution{ID: "ex"},
		"gate_step",
		trace, nil, "next_step")

	out := buf.String()
	assert.Contains(t, out, "gate_matched")
	assert.Contains(t, out, "tk")
	assert.Contains(t, out, "ex")
	assert.Contains(t, out, "gate_step")
	assert.Contains(t, out, "next_step")
}

// TestLogGateTrace_FailureEmitsWarn — when the caller passes a
// non-nil error, the line uses Warn level + "gate_failed" msg
// so dashboards group it as a routing failure.
func TestLogGateTrace_Failure(t *testing.T) {
	buf := &writableBuffer{}
	e := &Executor{logger: zerolog.New(buf)}
	trace := GateEvalTrace{
		Entries: []GateEvalEntry{
			{Condition: `x == 1`, Target: "t", Matched: false, Found: false},
		},
	}
	e.logGateTrace(context.Background(),
		&persistence.Task{ID: "tk"},
		&persistence.Execution{ID: "ex"},
		"gate_step",
		trace, assertErrf("no gate matched"), "")
	out := buf.String()
	assert.Contains(t, out, "gate_failed")
	assert.Contains(t, out, "warn", "level must be warn for failure path")
}

// TestLogGateTrace_EntryMissingObserved — when entry.Found is
// false AND there's no internal error, the per-entry record
// carries observed_missing:true rather than a (zero, zero) pair
// that could be confused with "value is zero".
func TestLogGateTrace_MissingObservedField(t *testing.T) {
	buf := &writableBuffer{}
	e := &Executor{logger: zerolog.New(buf)}
	trace := GateEvalTrace{
		Entries: []GateEvalEntry{
			{Condition: `missing.key == "x"`, Target: "t", Matched: false, Found: false},
		},
	}
	e.logGateTrace(context.Background(),
		&persistence.Task{ID: "tk"},
		&persistence.Execution{ID: "ex"},
		"gate_step", trace,
		assertErrf("no gate matched"), "")
	out := buf.String()
	assert.Contains(t, out, "observed_missing",
		"missing-observed entries must include observed_missing flag")
}

// TestLogGateTrace_EntryWithError — per-entry evaluator
// errors (bad syntax) surface in the trace's error field so
// the operator sees what broke.
func TestLogGateTrace_EntryError(t *testing.T) {
	buf := &writableBuffer{}
	e := &Executor{logger: zerolog.New(buf)}
	trace := GateEvalTrace{
		Entries: []GateEvalEntry{
			{Condition: `bad >>>`, Err: "parse error: unsupported operator"},
		},
	}
	e.logGateTrace(context.Background(),
		&persistence.Task{ID: "tk"},
		&persistence.Execution{ID: "ex"},
		"step", trace, assertErrf("evaluation failed"), "")
	out := buf.String()
	assert.Contains(t, out, "parse error")
}
