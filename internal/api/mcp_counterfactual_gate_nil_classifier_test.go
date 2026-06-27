package api

// Tests for the nil-classifier nil-path in evaluateCounterfactualGate.
//
// CE/Community edition behavior: when no replay-safety classifier is wired
// (blackboxReplaySafety == nil), all tools are allowed during a replay —
// replay-safety enforcement is an EE capability. These tests pin that
// nil-means-allowed contract.
//
// NOTE: the OLD fail-closed test (TestApplyCounterfactualMCPGate_NoClassifierFailsClosed)
// was RENAMED + updated to TestApplyCounterfactualMCPGate_NoClassifier_AllowsAllTools
// in mcp_counterfactual_gate_test.go when the CE/EE decoupling (Task 2 of
// feat/ce-ee-phase1c-decoupling) made nil = enforcement-OFF. There is no
// remaining fail-closed test. SAFETY INVARIANT: nil-means-allowed is safe only
// because CE has blackboxEngine == nil, so no replay tasks can be created
// (replay creation 503s) — i.e. the nil gate is never exercised on a real
// replay. Task 5 must preserve that (CE engine nil ⇒ no replays).

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/counterfactual"
)

// fakeClassifier is a minimal contracts.ReplaySafetyClassifier for testing.
type fakeClassifier struct{ safe map[string]bool }

func (f *fakeClassifier) IsReplaySafe(tool string) bool { return f.safe[tool] }

// replayPayloadWithTool builds a counterfactual.Payload that looks like a
// replay task whose original trace included toolName. Used by gate tests that
// need a concrete Payload without going through JSON decoding.
func replayPayloadWithTool(toolName string) counterfactual.Payload {
	return counterfactual.Payload{
		IsReplay:              true,
		OriginalToolsRecorded: true,
		OriginalTools:         map[string]struct{}{toolName: {}},
	}
}

// TestCounterfactualGate_NilClassifier_AllowsAllTools — when no EE classifier
// is wired, the gate must allow every tool in a replay (CE nil-path). No panic.
func TestCounterfactualGate_NilClassifier_AllowsAllTools(t *testing.T) {
	s := NewServer() // blackboxReplaySafety is nil
	overrides := replayPayloadWithTool("mcp__broker__place_order")

	result, err := s.evaluateCounterfactualGate(overrides, "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("nil classifier should not return an error; got: %v", err)
	}
	if result.HandledLocally {
		t.Errorf("nil classifier should allow the tool (CE nil-path); got HandledLocally=true, text=%q", result.Text)
	}
}

// TestCounterfactualGate_NilClassifier_NoPanic — calling evaluateCounterfactualGate
// with a nil blackboxReplaySafety must not panic, and must return a zero result
// (no error, not handled locally).
func TestCounterfactualGate_NilClassifier_NoPanic(t *testing.T) {
	s := NewServer()
	// Use defer to catch any panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("evaluateCounterfactualGate panicked with nil classifier: %v", r)
		}
	}()

	overrides := replayPayloadWithTool("mcp__anything__read")
	result, err := s.evaluateCounterfactualGate(overrides, "mcp__anything__read")
	if err != nil {
		t.Errorf("nil classifier must not error; got: %v", err)
	}
	if result.HandledLocally {
		t.Errorf("nil classifier must not handle locally; got HandledLocally=true")
	}
}

// TestCounterfactualGate_NonNilClassifier_DeniesUnsafeTool — a wired classifier
// that denies "mcp__broker__place_order" must block the tool with a
// not_replay_safe response.
func TestCounterfactualGate_NonNilClassifier_DeniesUnsafeTool(t *testing.T) {
	classifier := &fakeClassifier{safe: map[string]bool{
		"mcp__broker__read_only_query": true,
		// mcp__broker__place_order is intentionally absent → not safe
	}}
	s := NewServer(WithBlackBoxReplaySafety(contracts.ReplaySafetyClassifier(classifier)))

	overrides := replayPayloadWithTool("mcp__broker__place_order")
	result, err := s.evaluateCounterfactualGate(overrides, "mcp__broker__place_order")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.HandledLocally {
		t.Fatal("non-replay-safe tool must be blocked; got HandledLocally=false")
	}
	if !strings.Contains(result.Text, `"skipped":"not_replay_safe"`) {
		t.Errorf("synthesized response must contain not_replay_safe; got %q", result.Text)
	}
}
