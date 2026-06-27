package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// TestEvaluatePlausibility_RuleNotMatchedIsNoop — When clause
// doesn't fire on this result; required-field check must not run.
// Catches a regression where an unconditional rule would fire on
// every result regardless of intent.
func TestEvaluatePlausibility_RuleNotMatchedIsNoop(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{
			Name:    "feedback-on-rejection",
			When:    map[string]any{"approved": false},
			Require: []string{"feedback"},
		},
	}
	// approved=true → rule shouldn't fire even though feedback is empty
	body := []byte(`{"approved": true, "feedback": ""}`)
	got := EvaluatePlausibility(body, rules)
	assert.Empty(t, got, "When clause not met → rule must not fire")
}

// TestEvaluatePlausibility_RuleMatchedAndPasses — When clause
// fires, required fields are present and non-empty. No violation.
func TestEvaluatePlausibility_RuleMatchedAndPasses(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{
			Name:    "feedback-on-rejection",
			When:    map[string]any{"approved": false},
			Require: []string{"feedback"},
		},
	}
	body := []byte(`{"approved": false, "feedback": "needs more tests"}`)
	got := EvaluatePlausibility(body, rules)
	assert.Empty(t, got, "When matched and required field is non-empty → no violation")
}

// TestEvaluatePlausibility_MissingFieldFires — required field
// absent under matched When clause is the headline failure mode.
func TestEvaluatePlausibility_MissingFieldFires(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{
			Name:    "feedback-on-rejection",
			When:    map[string]any{"approved": false},
			Require: []string{"feedback"},
		},
	}
	body := []byte(`{"approved": false}`) // feedback absent entirely
	got := EvaluatePlausibility(body, rules)
	require.Len(t, got, 1)
	assert.Equal(t, "feedback-on-rejection", got[0].RuleName)
	assert.Contains(t, got[0].Detail, "feedback")
	assert.Contains(t, got[0].Detail, "approved=false")
	assert.False(t, got[0].WarnOnly, "non-warn-only rule must report WarnOnly=false so the caller gates")
}

// TestEvaluatePlausibility_EmptyValueFires — the case
// requiredOutputKeys can't catch: field is present but empty.
// Covers the "approved=true, feedback=”" pattern AND the
// "modified_files=[]" pattern.
func TestEvaluatePlausibility_EmptyValueFires(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		require []string
	}{
		{"empty string", `{"approved": false, "feedback": ""}`, []string{"feedback"}},
		{"whitespace string", `{"approved": false, "feedback": "   \n"}`, []string{"feedback"}},
		{"empty array", `{"approved": false, "modified_files": []}`, []string{"modified_files"}},
		{"empty object", `{"approved": false, "metadata": {}}`, []string{"metadata"}},
		{"null value", `{"approved": false, "feedback": null}`, []string{"feedback"}},
		{"numeric zero", `{"approved": false, "feedback": 0}`, []string{"feedback"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := []registry.PlausibilityRule{
				{Name: "fb", When: map[string]any{"approved": false}, Require: tc.require},
			}
			got := EvaluatePlausibility([]byte(tc.body), rules)
			require.Len(t, got, 1, "%s: must fire — %s", tc.name, tc.body)
		})
	}
}

// TestEvaluatePlausibility_WarnOnlyDoesNotGate — warn-only rules
// are still reported so the caller can log them, but the WarnOnly
// flag flows through. Operator stages new rules in this mode
// before promoting to a real gate.
func TestEvaluatePlausibility_WarnOnlyDoesNotGate(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "soft", When: map[string]any{"approved": false}, Require: []string{"feedback"}, WarnOnly: true},
	}
	body := []byte(`{"approved": false}`)
	got := EvaluatePlausibility(body, rules)
	require.Len(t, got, 1)
	assert.True(t, got[0].WarnOnly, "WarnOnly flag must propagate to the violation so the caller can branch")
}

// TestEvaluatePlausibility_UnconditionalRule — empty When fires
// whenever required fields are missing. Operators use this for
// "always require" semantics — stricter than requiredOutputKeys
// because non-empty is enforced.
func TestEvaluatePlausibility_UnconditionalRule(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "always-message", Require: []string{"message"}},
	}
	got := EvaluatePlausibility([]byte(`{"message": ""}`), rules)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, "message")
	assert.NotContains(t, got[0].Detail, "under condition", "unconditional rule must not mention conditions in the detail string")
}

// TestEvaluatePlausibility_NumericConditionMatchesAcrossTypes —
// YAML decodes integers as int; JSON decodes as float64. The
// equality check must bridge the types so a rule like
// "when: {iterations: 0}" matches a result.json with
// "iterations": 0.0.
func TestEvaluatePlausibility_NumericConditionMatchesAcrossTypes(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "explain-zero-iters", When: map[string]any{"iterations": 0}, Require: []string{"explanation"}},
	}
	body := []byte(`{"iterations": 0, "explanation": ""}`)
	got := EvaluatePlausibility(body, rules)
	require.Len(t, got, 1, "YAML int 0 must match JSON float 0 — operators expect predictable equality")
}

// TestEvaluatePlausibility_MultipleRulesEvaluatedIndependently —
// each rule is its own pass; one rule's failure doesn't suppress
// another's. Operator dashboard wants the full picture in one
// failure record.
func TestEvaluatePlausibility_MultipleRulesEvaluatedIndependently(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "a", When: map[string]any{"x": true}, Require: []string{"y"}},
		{Name: "b", Require: []string{"z"}},
	}
	body := []byte(`{"x": true, "y": "", "z": null}`)
	got := EvaluatePlausibility(body, rules)
	assert.Len(t, got, 2, "both rules must fire independently — short-circuiting hides the second violation from operators")
	names := []string{got[0].RuleName, got[1].RuleName}
	assert.Contains(t, names, "a")
	assert.Contains(t, names, "b")
}

// TestEvaluatePlausibility_NoRulesIsNoop — unrelated to the rule
// itself, but a defense against a regression where an empty rules
// slice would still cause a JSON parse on every step. Lightweight
// guard, lightweight test.
func TestEvaluatePlausibility_NoRulesIsNoop(t *testing.T) {
	got := EvaluatePlausibility([]byte(`{"x":1}`), nil)
	assert.Empty(t, got)
	got = EvaluatePlausibility([]byte(`{"x":1}`), []registry.PlausibilityRule{})
	assert.Empty(t, got)
}

// TestEvaluatePlausibility_UnparseableBodyIsNoop — when result.json
// is malformed, the upstream validateRequiredOutputKeys path
// already classifies as INVALID_OUTPUT. Plausibility must not
// double-fire — return empty so the upstream signal is the only
// one the operator sees.
func TestEvaluatePlausibility_UnparseableBodyIsNoop(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "always", Require: []string{"x"}},
	}
	got := EvaluatePlausibility([]byte(`not json`), rules)
	assert.Empty(t, got, "malformed body is the upstream path's problem; plausibility stays silent")
}

// TestEvaluatePlausibility_FalseBoolIsNotEmpty — a deliberate
// quirk: bool false is a legitimate value, not absence. Operators
// express "feedback must accompany approved=false" via the When
// clause rather than expecting Require to treat false as missing.
func TestEvaluatePlausibility_FalseBoolIsNotEmpty(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "always-approved", Require: []string{"approved"}},
	}
	got := EvaluatePlausibility([]byte(`{"approved": false}`), rules)
	assert.Empty(t, got, "approved=false is a real answer; bool false must not count as empty for Require purposes")
}

// TestEvaluatePlausibility_RuleNameFallsBackToIndex — operator
// didn't name the rule; the helper synthesises rule[i] so log
// triage isn't ambiguous.
func TestEvaluatePlausibility_RuleNameFallsBackToIndex(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Require: []string{"x"}}, // no Name
	}
	got := EvaluatePlausibility([]byte(`{}`), rules)
	require.Len(t, got, 1)
	assert.Equal(t, "rule[0]", got[0].RuleName)
}

// TestEvaluatePlausibility_NestedDottedPathInWhen — when clauses
// keyed on dotted paths must walk nested objects, not look up
// flat keys. Item 12's replay corpus surfaced this on first run:
// the schema-derived rules from the outputSchema migration
// ("when: writing.written: true") were silently never matching on
// nested payloads, so the schema's intent was inert at runtime.
func TestEvaluatePlausibility_NestedDottedPathInWhen(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{
			Name:    "written_implies_path",
			When:    map[string]any{"writing.written": true},
			Require: []string{"writing.path"},
		},
	}
	t.Run("matches nested object", func(t *testing.T) {
		body := []byte(`{"writing": {"written": true, "path": ""}}`)
		got := EvaluatePlausibility(body, rules)
		require.Len(t, got, 1, "nested when=true with empty path should fire the rule")
		assert.Contains(t, got[0].Detail, "writing.path")
	})
	t.Run("matches flat-key form too (LLM emits dotted-as-key)", func(t *testing.T) {
		body := []byte(`{"writing.written": true, "writing.path": ""}`)
		got := EvaluatePlausibility(body, rules)
		require.Len(t, got, 1, "flat-key payload should also trip the rule")
	})
	t.Run("no fire when nested path absent", func(t *testing.T) {
		body := []byte(`{"writing": {"written": false}}`)
		got := EvaluatePlausibility(body, rules)
		assert.Empty(t, got, "when=false → rule shouldn't fire")
	})
}

// TestEvaluatePlausibility_NestedDottedPathInRequire — Require
// fields keyed on dotted paths must walk nested objects too. The
// writer schema's `min_length_message` rule and similar
// auto-generated rules from minLength:1 declarations rely on this.
func TestEvaluatePlausibility_NestedDottedPathInRequire(t *testing.T) {
	rules := []registry.PlausibilityRule{
		{Name: "always_writing_path", Require: []string{"writing.path"}},
	}
	t.Run("nested non-empty passes", func(t *testing.T) {
		body := []byte(`{"writing": {"path": "artifacts/out/x.md"}}`)
		got := EvaluatePlausibility(body, rules)
		assert.Empty(t, got)
	})
	t.Run("nested empty string fires", func(t *testing.T) {
		body := []byte(`{"writing": {"path": ""}}`)
		got := EvaluatePlausibility(body, rules)
		require.Len(t, got, 1)
	})
	t.Run("nested path missing fires", func(t *testing.T) {
		body := []byte(`{"writing": {}}`)
		got := EvaluatePlausibility(body, rules)
		require.Len(t, got, 1)
	})
}
