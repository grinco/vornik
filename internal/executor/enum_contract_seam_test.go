package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// The receipt seam: both checks in one place so the two enforcement sites
// cannot drift, and nil EffectiveSchema suppressing the ENUM half only.

func TestCheckOutputContract_NilSchemaStillEnforcesRequiredKeys(t *testing.T) {
	// Test 55 — the half that would have been a silent regression. Gating the
	// whole composite validator on a nil dialect tree retires requiredOutputKeys
	// on every recovery hop, under a "status quo" framing.
	e := &Executor{}
	msg := e.checkOutputContract([]byte(`{"testing":{"passed":true}}`), "tester",
		[]string{"testing.cases"}, nil)
	if msg == "" {
		t.Fatal("nil EffectiveSchema suppressed the requiredOutputKeys check; it must suppress the enum check only")
	}
	if !strings.Contains(msg, "missing required keys") {
		t.Fatalf("expected a missing-keys diagnostic, got %q", msg)
	}
}

func TestCheckOutputContract_NilSchemaSkipsTheEnumCheck(t *testing.T) {
	e := &Executor{}
	if msg := e.checkOutputContract([]byte(`{"testing":{"cases":[{"id":"invented"}]}}`), "tester", nil, nil); msg != "" {
		t.Fatalf("a nil schema enforced an enum: %q", msg)
	}
}

func TestCheckOutputContract_MissingKeyWinsAndEnumIsAppended(t *testing.T) {
	// Test 60 — one message, both defects. Otherwise the retry fixes the key
	// and discovers the enum violation on the NEXT attempt, burning a rung of
	// the ladder per defect.
	e := &Executor{}
	result := []byte(`{"testing":{"cases":[{"id":"case_1","status":"passed"}]}}`)
	msg := e.checkOutputContract(result, "tester", []string{"testing.pinned_cases_validated"}, testerSchema("s1_case_1"))
	if !strings.Contains(msg, "missing required keys") {
		t.Errorf("missing-key diagnostic absent: %q", msg)
	}
	if !strings.Contains(msg, "case_1") {
		t.Errorf("enum violation was not appended: %q", msg)
	}
	if strings.Index(msg, "missing required keys") > strings.Index(msg, "outside the declared set") {
		t.Errorf("precedence inverted; the missing key must lead: %q", msg)
	}
}

func TestCheckOutputContract_WarnViolationDoesNotFailTheStep(t *testing.T) {
	// Test 54a at the seam: counted and logged, but the step survives.
	e := &Executor{}
	schema := &registry.OutputSchema{
		Type: "object",
		Properties: map[string]*registry.OutputSchema{
			"analysis": {
				Type: "object",
				Properties: map[string]*registry.OutputSchema{
					"complexity": {Type: "string", Enum: []any{"trivial", "standard"}, OnViolation: "warn"},
				},
			},
		},
	}
	if msg := e.checkOutputContract([]byte(`{"analysis":{"complexity":"medium"}}`), "analyst", nil, schema); msg != "" {
		t.Fatalf("a warn-mode violation failed the step: %q — dynamic-tool-budget-design.md §4 resolves it to 1.0x", msg)
	}
}

func TestCheckOutputContract_CleanResultPasses(t *testing.T) {
	e := &Executor{}
	result := []byte(`{"testing":{"cases":[{"id":"s1_case_1","status":"passed"}],"pinned_cases_validated":true}}`)
	if msg := e.checkOutputContract(result, "tester", []string{"testing.pinned_cases_validated"}, testerSchema("s1_case_1")); msg != "" {
		t.Fatalf("a conforming result was rejected: %q", msg)
	}
}
