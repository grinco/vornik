package agentbench

import (
	"math"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

func pinnedPolicy() *quality.ScoringPolicy {
	return &quality.ScoringPolicy{
		Kind: quality.ScoreKindPinnedCaseValidation, ProducerStep: "analyze", VerifierStep: "test",
	}
}

func pinnedSnapshot(statuses ...string) []byte {
	cases := make([]string, 0, len(statuses))
	ids := make([]string, 0, len(statuses))
	for i, status := range statuses {
		id := string(rune('a' + i))
		ids = append(ids, `"`+id+`"`)
		cases = append(cases, `{"id":"`+id+`","status":"`+status+`"}`)
	}
	return []byte(`{"stepResults":{"analyze":{"analysis":{"test_case_ids":[` +
		strings.Join(ids, ",") + `],"test_cases_pinned":` + string(rune('0'+len(statuses))) +
		`}},"test":{"testing":{"cases":[` + strings.Join(cases, ",") + `]}}}}`)
}

func TestScoreTask_JournalsOneAuditableVerdictForTheRepeat(t *testing.T) {
	got, err := ScoreTask("task-1", 2, pinnedPolicy(), []string{"root", "child"},
		pinnedSnapshot("passed", "failed", "manual"))
	if err != nil {
		t.Fatalf("ScoreTask: %v", err)
	}
	if got.TaskID != "task-1" || got.Repeat != 2 || got.Score != 2.0/3.0 ||
		got.PassedCaseCount != 2 || got.PinnedCaseCount != 3 || len(got.CaseEvidence) != 3 {
		t.Fatalf("task score = %#v", got)
	}
	if len(got.ExecutionIDs) != 2 || got.ExecutionIDs[0] != "root" {
		t.Fatalf("execution provenance = %v", got.ExecutionIDs)
	}
}

func TestScoreTask_CorruptLedgerSnapshotIsAHarnessError(t *testing.T) {
	if _, err := ScoreTask("task-1", 1, pinnedPolicy(), []string{"root"}, []byte(`{"stepResults":`)); err == nil {
		t.Fatal("corrupt ledger snapshot was charged to the measured system")
	}
}

func TestCompareTaskScores_AveragesRepeatsBeforePairing(t *testing.T) {
	a := []TaskScore{
		{TaskID: "a", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: .2},
		{TaskID: "a", Repeat: 2, Kind: quality.ScoreKindPinnedCaseValidation, Score: .6},
		{TaskID: "b", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: .9},
		{TaskID: "b", Repeat: 2, Kind: quality.ScoreKindPinnedCaseValidation, Score: .7},
	}
	b := []TaskScore{
		{TaskID: "a", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: .8},
		{TaskID: "a", Repeat: 2, Kind: quality.ScoreKindPinnedCaseValidation, Score: 1},
		{TaskID: "b", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: .6},
		{TaskID: "b", Repeat: 2, Kind: quality.ScoreKindPinnedCaseValidation, Score: .6},
	}
	got, err := CompareTaskScores(a, b, quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		t.Fatalf("CompareTaskScores: %v", err)
	}
	// Per-task deltas are +0.5 and -0.2. Repeats are not four pairs.
	if got.PairCount != 2 || math.Abs(got.MeanDelta-.15) > 1e-12 {
		t.Fatalf("comparison = %#v", got)
	}
	if math.Abs(got.SigmaD-0.4949747468305833) > 1e-12 {
		t.Fatalf("sample sigma_d = %.15f", got.SigmaD)
	}
}

func TestCompareTaskScores_RefusesMissingAndDuplicatePairs(t *testing.T) {
	one := []TaskScore{{TaskID: "a", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: .5}}
	missing := []TaskScore{{TaskID: "b", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: .5}}
	if _, err := CompareTaskScores(one, missing, quality.ScoreKindPinnedCaseValidation); err == nil {
		t.Fatal("silently intersected missing task pairs")
	}
	dup := append(append([]TaskScore{}, one...), one[0])
	if _, err := CompareTaskScores(dup, one, quality.ScoreKindPinnedCaseValidation); err == nil {
		t.Fatal("silently accepted duplicate task/repeat pairs")
	}
	outOfRange := []TaskScore{{TaskID: "a", Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: 1.1}}
	if _, err := CompareTaskScores(outOfRange, one, quality.ScoreKindPinnedCaseValidation); err == nil {
		t.Fatal("silently accepted a score outside [0,1]")
	}
}
