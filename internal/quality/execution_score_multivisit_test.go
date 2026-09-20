package quality

import (
	"encoding/json"
	"testing"
)

// A repeated step keeps only its LAST visit in the snapshot's stepResults
// mirror (06-executor.md, 2026-09-19). Scoring that one visit against a
// whole-task denominator is a claim about the execution made from a fraction of
// it, and nothing in the record said which it was.
//
// The execution RECORD retains every visit — one execution_step_outcomes row
// per visit, each carrying result_hash — so this is a read-side gap and the
// scorer's job here is to refuse to make the claim, not to reconstruct it.
//
// None of this can fire on any arm to date: every scored execution in the
// 2026.9.4 arm visited every step exactly once (verified against
// executions.state_snapshot, 2026-09-19). That is the point — it fires the
// first time the rework loop is actually exercised.

func multiVisitSnapshot(t *testing.T, visits map[string]int) []byte {
	t.Helper()
	state := map[string]any{
		"stepResults": map[string]json.RawMessage{
			"analyze": json.RawMessage(`{"analysis":{"test_case_ids":["c1","c2"],"test_cases_pinned":2}}`),
			"test":    json.RawMessage(`{"testing":{"cases":[{"id":"c1","status":"passed"},{"id":"c2","status":"passed"}]}}`),
		},
	}
	if visits != nil {
		state["visitCounts"] = visits
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The control: single visits score exactly as they did before this change.
func TestScoreExecution_SingleVisitIsUnaffected(t *testing.T) {
	got, err := ScoreExecution(pinnedPolicy(), multiVisitSnapshot(t, map[string]int{
		"analyze": 1, "implement": 1, "test": 1, "review": 1, "report": 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusScored {
		t.Fatalf("want scored, got %s (%s)", got.Status, got.Diagnostic)
	}
	if got.Score == nil || *got.Score != 1 {
		t.Fatalf("want 1.000, got %v", got.Score)
	}
}

// A verifier visited twice: the mirror holds one visit and the scorer cannot
// tell which. It must say so rather than publish a fraction of the execution as
// the whole of it.
func TestScoreExecution_RepeatedVerifierIsUnscorable(t *testing.T) {
	got, err := ScoreExecution(pinnedPolicy(), multiVisitSnapshot(t, map[string]int{
		"analyze": 1, "implement": 3, "test": 3, "review": 2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusUnscorable {
		t.Fatalf("want unscorable, got %s", got.Status)
	}
	if got.Diagnostic != DiagnosticMultiVisitLastOnly {
		t.Fatalf("want %s, got %q", DiagnosticMultiVisitLastOnly, got.Diagnostic)
	}
	if got.VerifierVisits != 3 {
		t.Fatalf("the visit count must be carried so the refusal is auditable, got %d", got.VerifierVisits)
	}
}

// The producer half matters too: a re-entered analyst means the denominator
// itself is one visit's worth.
func TestScoreExecution_RepeatedProducerIsUnscorable(t *testing.T) {
	got, err := ScoreExecution(pinnedPolicy(), multiVisitSnapshot(t, map[string]int{"analyze": 2, "test": 1}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusUnscorable {
		t.Fatalf("want unscorable, got %s", got.Status)
	}
	if got.ProducerVisits != 2 {
		t.Fatalf("want producer visits carried, got %d", got.ProducerVisits)
	}
}

// A snapshot with NO visitCounts predates the field. Absence is not evidence of
// a single visit, but it is also not evidence of several — and turning every
// pre-field journal unscorable would rewrite history rather than describe it.
// Treated as single-visit, which is what those executions were.
func TestScoreExecution_AbsentVisitCountsScoresAsBefore(t *testing.T) {
	got, err := ScoreExecution(pinnedPolicy(), multiVisitSnapshot(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusScored {
		t.Fatalf("want scored for a pre-field snapshot, got %s (%s)", got.Status, got.Diagnostic)
	}
}

// A step this policy does not name may loop freely — dev-pipeline's implement
// step does — without making the verifier's report unreadable.
func TestScoreExecution_UnrelatedStepLoopingIsFine(t *testing.T) {
	got, err := ScoreExecution(pinnedPolicy(), multiVisitSnapshot(t, map[string]int{
		"analyze": 1, "implement": 7, "test": 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusScored {
		t.Fatalf("an unrelated looping step made the execution unscorable: %s", got.Status)
	}
}

// unscorable must not be read as "the agent produced nothing". A zero would say
// exactly that, and it is a different false statement from the one being fixed.
func TestScoreExecution_UnscorableCarriesNoZeroVerdict(t *testing.T) {
	got, _ := ScoreExecution(pinnedPolicy(), multiVisitSnapshot(t, map[string]int{"analyze": 1, "test": 2}))
	if got.Score == nil {
		t.Fatal("the journal needs a numeric field; it must be present")
	}
	if got.Status == ScoreStatusScored {
		t.Fatal("unscorable must not be reported as scored")
	}
	if got.PinnedCaseCount != 2 {
		t.Fatalf("the pinned denominator is readable and should be carried, got %d", got.PinnedCaseCount)
	}
}
