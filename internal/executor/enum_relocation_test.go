package executor

import (
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/quality"
)

// Test 58 — the relocation D6.4 claims. Before the backstop, a surplus id
// reached the scorer and showed up as ExtraCaseCount; after it, the step is
// refused at receipt so the result never lands in stepResults and the scorer
// counts nothing. The signal does not vanish — it moves to the counter — and
// that is the equivalence this asserts, because "relocates rather than
// destroys" is otherwise a claim nothing checks.

func snapshotWith(t *testing.T, analyze, test string) []byte {
	t.Helper()
	snap := map[string]any{
		"stepResults": map[string]json.RawMessage{
			"analyze": json.RawMessage(analyze),
			"test":    json.RawMessage(test),
		},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return b
}

func TestSurplusIDRelocatesFromExtraCaseCountToTheCounter(t *testing.T) {
	policy := &quality.ScoringPolicy{
		Kind:         quality.ScoreKindPinnedCaseValidation,
		ProducerStep: "analyze",
		VerifierStep: "test",
	}
	analyze := `{"analysis":{"test_case_ids":["s1_case_1"],"test_cases_pinned":1}}`
	surplus := `{"testing":{"cases":[{"id":"s1_case_1","status":"passed"},{"id":"s2_case_9","status":"passed"}],"pinned_cases_validated":true,"passed":true}}`

	// BEFORE the backstop: the surplus report lands, and the scorer is where
	// the slip is visible.
	before, err := quality.ScoreExecution(policy, snapshotWith(t, analyze, surplus))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if before.ExtraCaseCount != 1 {
		t.Fatalf("pre-backstop ExtraCaseCount = %d, want 1 — the premise of the relocation claim",
			before.ExtraCaseCount)
	}

	// AFTER: the backstop refuses the same report at receipt.
	e := &Executor{metrics: NewMetrics(prometheus.NewRegistry())}
	msg := e.checkOutputContract([]byte(surplus), "tester", nil, testerSchema("s1_case_1"))
	if msg == "" {
		t.Fatal("the backstop admitted a surplus id; nothing was intercepted")
	}
	if got := counterValue(t, e.metrics.OutputEnumViolationTotal, "tester", "testing.cases[1].id"); got != 1 {
		t.Fatalf("violation counter = %v, want 1 — the signal must land in its new home", got)
	}

	// And the scorer's view of a step that never landed: no surplus to count.
	after, err := quality.ScoreExecution(policy, snapshotWith(t, analyze,
		`{"testing":{"cases":[{"id":"s1_case_1","status":"passed"}],"pinned_cases_validated":true,"passed":true}}`))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if after.ExtraCaseCount != 0 {
		t.Fatalf("post-backstop ExtraCaseCount = %d, want 0 — with the backstop active this number "+
			"is zero by construction and is no longer evidence about anything", after.ExtraCaseCount)
	}
}
