package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The incident these tests pin (agent-quality-benchmark design, amendment
// 2026-09-18 and its RETRACTION the same day): on the 2026.9.4 arm the tester
// reported case ids that were not the ones it was given — inventing 3 on
// dp-01, renumbering all 15 on dp-09, reporting a subset on dp-02/06/07 — and
// `pinned_case_validation` scored those tasks 0.000 to 0.42 with ZERO cases
// ever reported failed. Every lost point was id bookkeeping, not bad work.
//
// Checked against tool_audit_log: the analyst wrote the SAME ids into both
// result.json and CURRENT_TASK.md on every losing task, and the tester read
// that file every time. So it deviated from a contract it demonstrably held.
//
// These tests pin the mechanism that hands the verifier the scorer's own list
// through the existing ${outputs.<step>.<field>} resolver instead of a prose
// file. That removes a fetch-and-parse step and makes the contract checkable
// at load time; it is NOT by itself demonstrated to fix the losses above,
// since the list was already available. The binding fix is the schema enum
// (D5) that makes a wrong id undecodable.

// TestInterpolatePromptRefs_ResolvesProducerList is test 39: an agent step
// prompt carrying a reference to a prior step's structured output has that
// value substituted before the container ever sees the prompt.
func TestInterpolatePromptRefs_ResolvesProducerList(t *testing.T) {
	steps := map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1","s1_case_2","s1_case_3"],"test_cases_pinned":3}}`),
	}
	prompt := "The analyst pinned exactly these case ids: ${outputs.analyze.analysis.test_case_ids}\nReport one entry per id."

	got, err := interpolatePromptRefs(prompt, steps)
	if err != nil {
		t.Fatalf("interpolatePromptRefs returned error: %v", err)
	}
	for _, id := range []string{"s1_case_1", "s1_case_2", "s1_case_3"} {
		if !strings.Contains(got, id) {
			t.Errorf("prompt is missing pinned id %q; got:\n%s", id, got)
		}
	}
	if strings.Contains(got, "${outputs.") {
		t.Errorf("reference left unresolved in prompt:\n%s", got)
	}
	if !strings.Contains(got, "Report one entry per id.") {
		t.Errorf("surrounding prompt text was lost:\n%s", got)
	}
}

// TestInterpolatePromptRefs_ListRendersReadably is test 40. A model reads the
// prompt, so an embedded list must render as JSON rather than Go's default
// slice formatting — `[s1_case_1 s1_case_2]` would strip the quotes and commas
// the tester is expected to echo back verbatim.
func TestInterpolatePromptRefs_ListRendersReadably(t *testing.T) {
	steps := map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1","s1_case_2"]}}`),
	}

	got, err := interpolatePromptRefs("ids: ${outputs.analyze.analysis.test_case_ids}", steps)
	if err != nil {
		t.Fatalf("interpolatePromptRefs returned error: %v", err)
	}
	const want = `ids: ["s1_case_1","s1_case_2"]`
	if got != want {
		t.Errorf("rendered list = %q, want %q", got, want)
	}
}

// TestInterpolatePromptRefs_UnresolvedIsAnError is test 43, and it is the one
// that matters most. The payload path resolves a missing reference to "" by
// design. Carrying that into a prompt would hand the tester "pinned exactly
// these case ids: " — an empty contract it satisfies by reporting nothing,
// scoring 0.000, looking exactly like the defect this change fixes. §4 of
// CLAUDE.md: a control that cannot distinguish "examined and clean" from
// "never examined" reports the first and means the second.
func TestInterpolatePromptRefs_UnresolvedIsAnError(t *testing.T) {
	cases := []struct {
		name  string
		steps map[string]json.RawMessage
	}{
		{"step absent from state", map[string]json.RawMessage{}},
		{"step present, field absent", map[string]json.RawMessage{
			"analyze": json.RawMessage(`{"analysis":{"something_else":1}}`),
		}},
		{"step present, body unparseable", map[string]json.RawMessage{
			"analyze": json.RawMessage(`not json`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := interpolatePromptRefs("ids: ${outputs.analyze.analysis.test_case_ids}", tc.steps)
			if err == nil {
				t.Fatalf("unresolved reference silently substituted; got %q, want an error", got)
			}
			if !errors.Is(err, errPromptRefUnresolved) {
				t.Errorf("error = %v, want errPromptRefUnresolved", err)
			}
			if !strings.Contains(err.Error(), "outputs.analyze.analysis.test_case_ids") {
				t.Errorf("error does not name the reference: %v", err)
			}
		})
	}
}

// TestInterpolatePromptRefs_NoReferenceIsUntouched keeps the common case free:
// the overwhelming majority of step prompts carry no reference at all and must
// pass through byte-identical, erroring on nothing.
func TestInterpolatePromptRefs_NoReferenceIsUntouched(t *testing.T) {
	const prompt = "Read CURRENT_TASK.md and implement the next unchecked subtask."

	got, err := interpolatePromptRefs(prompt, nil)
	if err != nil {
		t.Fatalf("prompt without references returned error: %v", err)
	}
	if got != prompt {
		t.Errorf("prompt mutated: %q", got)
	}
}

// Test 41: the reference resolves AFTER the gate suffix, the operator-hint
// prefix and the fork override have been composed, so a reference is usable
// from any of them and none of them can mask one.
func TestAssembleStepPrompt_InterpolatesAfterComposition(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	step := registry.WorkflowStep{
		Type:   "agent",
		Role:   "tester",
		Prompt: "ids: ${outputs.analyze.analysis.test_case_ids}",
		Gates: []registry.WorkflowGate{
			{Condition: "testing.passed == true", Target: "review"},
		},
	}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["s1_case_1","s1_case_2"]}}`),
	}}
	applied := false

	got, err := e.assembleStepPrompt(&persistence.Execution{ID: "e1"}, "test", step,
		"<operator-hint>go carefully</operator-hint>\n", &applied, state)
	if err != nil {
		t.Fatalf("assembleStepPrompt returned error: %v", err)
	}
	if !strings.Contains(got, `["s1_case_1","s1_case_2"]`) {
		t.Errorf("reference not resolved:\n%s", got)
	}
	if !strings.Contains(got, "operator-hint") {
		t.Errorf("hint prefix lost:\n%s", got)
	}
	if !strings.Contains(got, "testing.passed") {
		t.Errorf("gate suffix lost:\n%s", got)
	}
}

// An unresolved reference must fail the step rather than reach the model as an
// empty contract — the runtime half of D2.
func TestAssembleStepPrompt_UnresolvedReferenceFailsTheStep(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	step := registry.WorkflowStep{
		Type:   "agent",
		Role:   "tester",
		Prompt: "ids: ${outputs.analyze.analysis.test_case_ids}",
	}
	applied := false

	_, err := e.assembleStepPrompt(&persistence.Execution{ID: "e1"}, "test", step, "", &applied,
		&executionState{StepResults: map[string]json.RawMessage{}})
	if !errors.Is(err, errPromptRefUnresolved) {
		t.Fatalf("error = %v, want errPromptRefUnresolved", err)
	}
}
