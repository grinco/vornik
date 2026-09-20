package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

// snapTraces is an ExecutionStateStore whose snapshots the test controls, so a
// re-score can be driven end to end without a database.
type snapTraces struct{ snaps map[string][]byte }

func (snapTraces) AssembleTraces(context.Context, string) ([]Trace, error) { return nil, nil }
func (s snapTraces) StateSnapshot(_ context.Context, id string) ([]byte, error) {
	b, ok := s.snaps[id]
	if !ok {
		return nil, fmt.Errorf("no snapshot for %s", id)
	}
	return b, nil
}

func extrasSnapshot(t *testing.T, ids []string, reported map[string]string) []byte {
	t.Helper()
	cases := make([]map[string]string, 0, len(reported))
	for id, st := range reported {
		cases = append(cases, map[string]string{"id": id, "status": st})
	}
	analyze, _ := json.Marshal(map[string]any{"analysis": map[string]any{
		"test_case_ids": ids, "test_cases_pinned": len(ids)}})
	test, _ := json.Marshal(map[string]any{"testing": map[string]any{"cases": cases}})
	snap, _ := json.Marshal(map[string]any{"stepResults": map[string]json.RawMessage{
		"analyze": analyze, "test": test}})
	return snap
}

// Rescore must recompute TASK SCORES, not only probe verdicts. Without this a
// task-score contract change — like the 2026-09-17 extras rule — cannot be
// applied to a completed arm at all, and the command that exists to make a
// HarnessVersion bump cheap is useless exactly when the bump is a scoring one.
//
// The shape is dp-01-nilguard's: every pinned case reported passed, plus extras.
// Under harness 5 that scored 0.000/invalid_evidence.
//
// DRIVEN THROUGH THE RECOMPUTE DIRECTLY, not through Rescore, since D3'.
// Harness 7 changed the scorer's INPUTS rather than its rule, so a journal
// carrying task scores from before v7 is refused at the door
// (TestRescore_RefusesTaskScoresFromBeforeTheInputChange) — its per-visit
// bodies are not recoverable. The capability this test exists for is
// unaffected and becomes reachable again at the next rule-only bump, so the
// coverage moves down one level rather than being deleted: deleting it would
// mean the next such bump ships with the recompute untested.
func TestRescore_RecomputesTaskScores(t *testing.T) {
	ids := []string{"a", "b"}
	snap := extrasSnapshot(t, ids, map[string]string{"a": "passed", "b": "passed", "x1": "passed"})

	j := Journal{}
	j.Manifest.RunID = "r1"
	j.Manifest.Arm.HarnessVersion = "5"
	j.TaskScores = []TaskScore{{
		TaskID: "dp-01", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation,
		Status: quality.ScoreStatusInvalidEvidence, Score: 0,
		PinnedCaseCount: 2, ExecutionIDs: []string{"exec-1"},
	}}
	tasks := []TaskSpec{{ID: "dp-01", Scoring: &quality.ScoringPolicy{
		Kind: quality.ScoreKindPinnedCaseValidation, ProducerStep: "analyze", VerifierStep: "test"}}}

	scores, err := rescoreTaskScores(context.Background(), j,
		snapTraces{snaps: map[string][]byte{"exec-1": snap}}, tasks)
	if err != nil {
		t.Fatalf("rescore: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("task scores = %d, want 1", len(scores))
	}
	ts := scores[0]
	if ts.Score != 1 {
		t.Errorf("score = %v, want 1.0 — every pinned case passed", ts.Score)
	}
	if ts.Status != quality.ScoreStatusScored {
		t.Errorf("status = %q, want scored", ts.Status)
	}
	if ts.ExtraCaseCount != 1 {
		t.Errorf("ExtraCaseCount = %d, want 1 — the count must reach the JOURNAL, "+
			"not stop at the scorer's return value", ts.ExtraCaseCount)
	}
}

// The D3' door: harness 7 assembles the pinned-case numerator from per-visit
// result bodies that live in the ledger, not in the journal, so a journal
// carrying task scores from before that change cannot be re-stamped. Producing
// a harness-7 number from inputs harness 7 never saw would be worse than
// refusing, because the figure would look comparable with real harness-7
// figures and would not be.
func TestRescore_RefusesTaskScoresFromBeforeTheInputChange(t *testing.T) {
	j := Journal{}
	j.Manifest.RunID = "r1"
	j.Manifest.Arm.HarnessVersion = "5"
	j.TaskScores = []TaskScore{{TaskID: "dp-01", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation}}
	tasks := []TaskSpec{{ID: "dp-01", Scoring: &quality.ScoringPolicy{
		Kind: quality.ScoreKindPinnedCaseValidation, ProducerStep: "analyze", VerifierStep: "test"}}}

	_, err := RescoreWithTasks(context.Background(), j, snapTraces{}, []Probe{SchemaProbe{}}, nil, tasks)
	if err == nil {
		t.Fatal("RescoreWithTasks() = nil error, want the input-gap refusal")
	}
	for _, want := range []string{"per-visit result bodies", "run a fresh arm"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to say %q", err, want)
		}
	}
}

// A journal carrying task scores with no task set supplied must still REFUSE,
// rather than silently pass them through under a new stamp.
func TestRescore_RefusesTaskScoresWithoutTaskSet(t *testing.T) {
	j := Journal{}
	j.Manifest.RunID = "r2"
	j.Manifest.Arm.HarnessVersion = "5"
	j.TaskScores = []TaskScore{{TaskID: "dp-01", Repeat: 1, ExecutionIDs: []string{"e"}}}

	if _, err := RescoreWithTasks(context.Background(), j, snapTraces{}, []Probe{SchemaProbe{}}, nil, nil); err == nil {
		t.Fatal("rescored a journal carrying task scores without the task set that defines their policy")
	}
}
