package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvaluateExpectation_EmptyDegradesToCompleted — no predicate
// declared means "task ran cleanly to COMPLETED". A FAILED or
// CANCELLED task with no expectation must still count as a fail —
// otherwise smoke tests are useless.
func TestEvaluateExpectation_EmptyDegradesToCompleted(t *testing.T) {
	cases := []struct {
		name   string
		status string
		passed bool
	}{
		{"completed", "COMPLETED", true},
		{"failed", "FAILED", false},
		{"cancelled", "CANCELLED", false},
		{"timed-out (no terminal status)", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := EvaluateExpectation(EvalExpectation{}, EvalEvidence{TerminalStatus: tc.status})
			assert.Equal(t, tc.passed, v.Passed,
				"%s: empty expectation must require COMPLETED — anything else is a fail",
				tc.name)
		})
	}
}

// TestEvaluateExpectation_OutcomePredicate — Outcome="FAILED"
// catches the negative-test pattern: workflow X must fail when
// given input Y. COMPLETED on a FAILED-expected case is a fail.
func TestEvaluateExpectation_OutcomePredicate(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Outcome: "FAILED"},
		EvalEvidence{TerminalStatus: "FAILED"},
	)
	assert.True(t, v.Passed)

	v = EvaluateExpectation(
		EvalExpectation{Outcome: "FAILED"},
		EvalEvidence{TerminalStatus: "COMPLETED"},
	)
	assert.False(t, v.Passed, "negative test must reject a successful run")
	assert.Contains(t, v.Reason, "FAILED")
	assert.Contains(t, v.Reason, "COMPLETED")
}

// TestEvaluateExpectation_OutcomeIsCaseInsensitive — operators
// might write "completed" or "Completed" in the corpus. Match both.
func TestEvaluateExpectation_OutcomeIsCaseInsensitive(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Outcome: "completed"},
		EvalEvidence{TerminalStatus: "COMPLETED"},
	)
	assert.True(t, v.Passed)
}

// TestEvaluateExpectation_EqualsDeepMatches — the result.json must
// deep-equal the expected JSON. JSON whitespace, key order, and
// trailing newlines are normalized by Unmarshal — operators don't
// see false-fails on cosmetic diffs.
func TestEvaluateExpectation_EqualsDeepMatches(t *testing.T) {
	expected := json.RawMessage(`{"approved": true, "score": 100}`)
	// Same content, different key order + whitespace.
	actual := json.RawMessage(`{
		"score": 100,
		"approved": true
	}`)
	v := EvaluateExpectation(
		EvalExpectation{Outcome: "COMPLETED", Equals: expected},
		EvalEvidence{TerminalStatus: "COMPLETED", Result: actual},
	)
	assert.True(t, v.Passed, "deep-equal must normalize key order + whitespace")
}

// TestEvaluateExpectation_EqualsRejectsMismatch — different
// values fail with a reason that includes the actual body so the
// operator can see the diff at a glance.
func TestEvaluateExpectation_EqualsRejectsMismatch(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Equals: json.RawMessage(`{"approved": true}`)},
		EvalEvidence{TerminalStatus: "COMPLETED", Result: json.RawMessage(`{"approved": false}`)},
	)
	assert.False(t, v.Passed)
	assert.Contains(t, v.Reason, "equals")
}

// TestEvaluateExpectation_ContainsTopLevelSubsetMatches — Contains
// is a subset filter on top-level keys. Operators use it when the
// workflow emits more fields than the test cares about.
func TestEvaluateExpectation_ContainsTopLevelSubsetMatches(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Contains: json.RawMessage(`{"approved": true}`)},
		EvalEvidence{
			TerminalStatus: "COMPLETED",
			Result:         json.RawMessage(`{"approved": true, "score": 100, "reviewer": "alice"}`),
		},
	)
	assert.True(t, v.Passed)
}

// TestEvaluateExpectation_ContainsRejectsMissingKey — when a
// declared key isn't present the reason names that key so the
// operator knows which assertion broke.
func TestEvaluateExpectation_ContainsRejectsMissingKey(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Contains: json.RawMessage(`{"approved": true, "feedback": "ok"}`)},
		EvalEvidence{
			TerminalStatus: "COMPLETED",
			Result:         json.RawMessage(`{"approved": true}`),
		},
	)
	assert.False(t, v.Passed)
	assert.Contains(t, v.Reason, "feedback")
}

// TestEvaluateExpectation_RegexMatchesAcrossEntireResult — the
// regex matches the JSON-encoded result, not a specific field.
// Lower fidelity than equals/contains but useful when the field of
// interest is nested or when the value form varies.
func TestEvaluateExpectation_RegexMatchesAcrossEntireResult(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Regex: `"score":\s*100`},
		EvalEvidence{
			TerminalStatus: "COMPLETED",
			Result:         json.RawMessage(`{"score": 100}`),
		},
	)
	assert.True(t, v.Passed)

	v = EvaluateExpectation(
		EvalExpectation{Regex: `"score":\s*100`},
		EvalEvidence{
			TerminalStatus: "COMPLETED",
			Result:         json.RawMessage(`{"score": 50}`),
		},
	)
	assert.False(t, v.Passed)
}

// TestEvaluateExpectation_BadRegexSurfacesError — operator typo in
// the corpus must produce a clear message, not a panic or a silent
// pass.
func TestEvaluateExpectation_BadRegexSurfacesError(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{Regex: `[invalid`},
		EvalEvidence{TerminalStatus: "COMPLETED", Result: json.RawMessage(`{}`)},
	)
	assert.False(t, v.Passed)
	assert.Contains(t, v.Reason, "regex")
}

// TestEvaluateExpectation_PredicateOrderingFavorsClearError —
// when the task didn't even reach COMPLETED, the operator wants
// "outcome=FAILED, expected COMPLETED" not "regex didn't match".
// Covers the common case of an operator running a predicate corpus
// against a broken workflow.
func TestEvaluateExpectation_PredicateOrderingFavorsClearError(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{
			Outcome: "COMPLETED",
			Regex:   `approved`,
		},
		EvalEvidence{TerminalStatus: "FAILED"},
	)
	assert.False(t, v.Passed)
	assert.Contains(t, v.Reason, "outcome",
		"the most informative error wins — outcome mismatch is the root cause, regex failure is downstream")
}

// TestEvaluateExpectation_MultiplePredicatesAreAnded — when the
// operator declares Outcome AND Contains AND Regex, every predicate
// must pass for the case to pass.
func TestEvaluateExpectation_MultiplePredicatesAreAnded(t *testing.T) {
	v := EvaluateExpectation(
		EvalExpectation{
			Outcome:  "COMPLETED",
			Contains: json.RawMessage(`{"approved": true}`),
			Regex:    `"score":\s*\d+`,
		},
		EvalEvidence{
			TerminalStatus: "COMPLETED",
			Result:         json.RawMessage(`{"approved": true, "score": 95}`),
		},
	)
	assert.True(t, v.Passed)

	// Same evidence with a regex that won't match → fail.
	v = EvaluateExpectation(
		EvalExpectation{
			Outcome:  "COMPLETED",
			Contains: json.RawMessage(`{"approved": true}`),
			Regex:    `nonexistent`,
		},
		EvalEvidence{
			TerminalStatus: "COMPLETED",
			Result:         json.RawMessage(`{"approved": true, "score": 95}`),
		},
	)
	assert.False(t, v.Passed)
}

// TestExpectationNeedsResult — controls whether the runner pays
// for an extra HTTP fetch per case. Outcome-only predicates must
// short-circuit; equals/contains/regex must fetch.
func TestExpectationNeedsResult(t *testing.T) {
	cases := []struct {
		name string
		e    EvalExpectation
		want bool
	}{
		{"empty", EvalExpectation{}, false},
		{"outcome only", EvalExpectation{Outcome: "COMPLETED"}, false},
		{"equals", EvalExpectation{Equals: json.RawMessage(`{}`)}, true},
		{"contains", EvalExpectation{Contains: json.RawMessage(`{}`)}, true},
		{"regex", EvalExpectation{Regex: "x"}, true},
		{"outcome + equals", EvalExpectation{Outcome: "COMPLETED", Equals: json.RawMessage(`{}`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expectationNeedsResult(tc.e)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDiffEvalRuns_RegressedAndRecovered — the regression compare
// is what makes the harness actually useful for prompt iteration.
// passed→failed is a regression; failed→passed is a recovery.
// Cases unique to either side are corpus changes and don't show
// up in either list.
func TestDiffEvalRuns_RegressedAndRecovered(t *testing.T) {
	prev := &evalRunSummary{
		Cases: map[string]evalCaseLast{
			"a-was-passing-now-fails":  {Passed: true},
			"b-was-failing-now-passes": {Passed: false},
			"c-still-passing":          {Passed: true},
			"d-still-failing":          {Passed: false},
			"e-only-in-prev":           {Passed: true},
		},
	}
	current := evalRunSummary{
		Cases: map[string]evalCaseLast{
			"a-was-passing-now-fails":  {Passed: false, Reason: "score=80, expected 100"},
			"b-was-failing-now-passes": {Passed: true},
			"c-still-passing":          {Passed: true},
			"d-still-failing":          {Passed: false},
			"f-only-in-current":        {Passed: false},
		},
	}
	regressed, recovered := diffEvalRuns(prev, current)
	assert.Equal(t, []string{"a-was-passing-now-fails"}, regressed)
	assert.Equal(t, []string{"b-was-failing-now-passes"}, recovered)
}

// TestDiffEvalRuns_NilPrev — the first run after a deployment has
// no history; diff must return empty without panicking. The CLI
// then prints no regression block, which is the correct UX (no
// data to diff against).
func TestDiffEvalRuns_NilPrev(t *testing.T) {
	regressed, recovered := diffEvalRuns(nil, evalRunSummary{
		Cases: map[string]evalCaseLast{"a": {Passed: true}},
	})
	assert.Nil(t, regressed)
	assert.Nil(t, recovered)
}

// TestSaveAndLoadEvalLastRun — round-trips the summary through the
// state-file path. Operators with a fresh deployment have no file;
// the helper must return nil rather than erroring.
func TestSaveAndLoadEvalLastRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	swarm := "test-swarm"

	require.Nil(t, loadEvalLastRun(swarm), "fresh state dir must yield nil, not an error")

	summary := evalRunSummary{
		Swarm:   swarm,
		Project: "p1",
		Cases: map[string]evalCaseLast{
			"alpha": {Passed: true},
			"bravo": {Passed: false, Reason: "regex didn't match"},
		},
	}
	require.NoError(t, saveEvalLastRun(swarm, summary))

	loaded := loadEvalLastRun(swarm)
	require.NotNil(t, loaded)
	assert.Equal(t, swarm, loaded.Swarm)
	assert.Equal(t, "p1", loaded.Project)
	assert.True(t, loaded.Cases["alpha"].Passed)
	assert.False(t, loaded.Cases["bravo"].Passed)
	assert.Equal(t, "regex didn't match", loaded.Cases["bravo"].Reason)
}
