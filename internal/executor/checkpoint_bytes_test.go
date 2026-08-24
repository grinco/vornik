package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// The same bug class as TestErrorResultJSON_SurvivesAByteThatGoQuotesAsHexEscape,
// at the sites that fix missed.
//
// 70aafb58 (2026-08-14) routed step ERROR text through resultJSON. It left the
// PASS-THROUGH assignments alone — the ones that take a producer's bytes and
// store them into a json.RawMessage verbatim:
//
//	state.LastResult = append(json.RawMessage(nil), resultBytes...)      // agent step
//	state.StepResults[id] = append(json.RawMessage(nil), resultBytes...) // outputs mirror
//	state.LastResult = append(json.RawMessage(nil), result...)           // plan step
//	state.LastResult = append(json.RawMessage(nil), sysResult.Result...) // system step
//
// Nothing between the producer and the snapshot column asked whether the bytes
// were JSON, so an invalid result did not fail where it was produced. It failed
// at the NEXT checkpoint, killing the execution several steps away:
//
//	failed to marshal execution checkpoint: json: error calling MarshalJSON for
//	type json.RawMessage: invalid character '[' in string escape code
//
// Measured 2026-08-22 over the 36 hours journald retained: 7 executions across 6
// tasks, every one the agent benchmark's dev-pipeline dying on the checkpoint
// after its `review` step. The guard belongs at the store,
// not at each producer: a per-producer fix has to be repeated for every future
// site, and this is the second time this class has been fixed one site at a
// time.

func TestStepResultJSON_PassesValidBytesThroughUnchanged(t *testing.T) {
	for _, valid := range []string{
		`{"summary":"done","files":3}`,
		`[1,2,3]`,
		`"a bare string"`,
		`null`,
	} {
		got := stepResultJSON(json.RawMessage(valid))
		if string(got) != valid {
			t.Errorf("valid result must survive byte-identical: %s -> %s", valid, got)
		}
	}
}

// The text is the diagnostic. Substituting a bare {"error":...} would satisfy
// the marshal and throw away the only evidence of what the step actually said,
// so the invalid bytes are PRESERVED as a properly-encoded JSON string.
func TestStepResultJSON_WrapsInvalidBytesAndKeepsTheText(t *testing.T) {
	bad := json.RawMessage(`{"stderr":"traceback \[31m at line 4"}`)
	if json.Valid(bad) {
		t.Fatal("fixture is supposed to be invalid JSON")
	}

	got := stepResultJSON(bad)

	if !json.Valid(got) {
		t.Fatalf("the guard produced invalid JSON: %s", got)
	}
	var decoded struct {
		Error string `json:"error"`
		Raw   string `json:"raw"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, got)
	}
	if decoded.Raw != string(bad) {
		t.Errorf("the producer's text must be preserved verbatim, got %q", decoded.Raw)
	}
	if decoded.Error == "" {
		t.Error("the substitution must say why it happened")
	}
	// And the whole point: it survives the checkpoint marshal.
	if _, err := json.Marshal(executionState{LastResult: got}); err != nil {
		t.Fatalf("checkpoint marshal still fails — the bug is not fixed: %v", err)
	}
}

// An empty result keeps meaning "nothing", not "a step failed to encode". Empty
// bytes marshal as null today and callers guard on len(), so wrapping them
// would invent a diagnostic where there is no defect.
func TestStepResultJSON_EmptyStaysEmpty(t *testing.T) {
	if got := stepResultJSON(nil); got != nil {
		t.Errorf("nil must stay nil, got %s", got)
	}
	if got := stepResultJSON(json.RawMessage{}); got != nil {
		t.Errorf("empty must stay nil, got %s", got)
	}
}

// A pathological blob must not become an unbounded snapshot column. The wrap is
// a diagnostic, so it is capped — and says so, rather than silently truncating.
func TestStepResultJSON_CapsAPathologicalBlob(t *testing.T) {
	bad := json.RawMessage(`{"x":"\[` + strings.Repeat("A", maxPreservedResultBytes*2) + `"}`)

	got := stepResultJSON(bad)

	if !json.Valid(got) {
		t.Fatalf("invalid JSON: %.200s", got)
	}
	if len(got) > maxPreservedResultBytes*2 {
		t.Errorf("the wrap must be bounded, got %d bytes", len(got))
	}
	var decoded struct {
		Raw       string `json:"raw"`
		Truncated bool   `json:"raw_truncated"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Truncated {
		t.Error("a truncated diagnostic must declare itself truncated")
	}
}

// THE BACKSTOP. Whatever a producer stores, the checkpoint write must not be
// the thing that dies. This is the guard that makes the class extinct rather
// than fixing today's four sites.
func TestSaveExecutionState_RepairsInvalidBytesRatherThanKillingTheExecution(t *testing.T) {
	repo := NewMockExecRepo()
	exec := &persistence.Execution{ID: "exec-1"}
	repo.execs[exec.ID] = exec
	e := &Executor{execRepo: repo, logger: zerolog.Nop(), config: DefaultConfig()}

	state := executionState{
		CurrentStepID: "step-2",
		LastResult:    json.RawMessage(`{"error":"agent said \[31mFAILED"}`),
		StepResults: map[string]json.RawMessage{
			"step-1": json.RawMessage(`{"out":"bad \x1b escape"}`),
			"step-0": json.RawMessage(`{"out":"fine"}`),
		},
	}

	if err := e.saveExecutionState(context.Background(), exec, state); err != nil {
		t.Fatalf("the checkpoint must survive an invalid step result: %v", err)
	}
	if !json.Valid(exec.StateSnapshot) {
		t.Fatalf("persisted an invalid snapshot: %s", exec.StateSnapshot)
	}

	var back executionState
	if err := json.Unmarshal(exec.StateSnapshot, &back); err != nil {
		t.Fatalf("snapshot does not round-trip: %v", err)
	}
	if !json.Valid(back.LastResult) {
		t.Errorf("LastResult was not repaired: %s", back.LastResult)
	}
	if !json.Valid(back.StepResults["step-1"]) {
		t.Errorf("StepResults[step-1] was not repaired: %s", back.StepResults["step-1"])
	}
	if string(back.StepResults["step-0"]) != `{"out":"fine"}` {
		t.Errorf("a valid step result must be left alone, got %s", back.StepResults["step-0"])
	}
}

// The repair must reach the map the workflow loop keeps interpolating from.
// Leaving the bad bytes in memory would fix the write and leave
// ${outputs.<step>.<field>} reading garbage for the rest of the run.
func TestSaveExecutionState_RepairIsVisibleToTheInterpolator(t *testing.T) {
	repo := NewMockExecRepo()
	exec := &persistence.Execution{ID: "exec-1"}
	repo.execs[exec.ID] = exec
	e := &Executor{execRepo: repo, logger: zerolog.Nop(), config: DefaultConfig()}

	results := map[string]json.RawMessage{"step-1": json.RawMessage(`{"out":"bad \[ escape"}`)}
	state := executionState{StepResults: results}

	if err := e.saveExecutionState(context.Background(), exec, state); err != nil {
		t.Fatalf("saveExecutionState: %v", err)
	}
	if !json.Valid(results["step-1"]) {
		t.Errorf("the caller's map still holds unusable bytes: %s", results["step-1"])
	}
}
