package quality

import (
	"encoding/json"
	"strings"
	"testing"
)

func scoreSnapshot(t *testing.T, analystIDs []string, pinned int, cases []PinnedCaseEvidence) []byte {
	t.Helper()
	producer, err := json.Marshal(map[string]any{"analysis": map[string]any{
		"test_case_ids": analystIDs, "test_cases_pinned": pinned,
	}})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := json.Marshal(map[string]any{"testing": map[string]any{"cases": cases}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(map[string]any{"stepResults": map[string]json.RawMessage{
		"analyze": producer,
		"test":    verifier,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func pinnedPolicy() *ScoringPolicy {
	return &ScoringPolicy{
		Kind:         ScoreKindPinnedCaseValidation,
		ProducerStep: "analyze",
		VerifierStep: "test",
	}
}

func TestScoreExecution_NotApplicableHasNoNumericScore(t *testing.T) {
	got, err := ScoreExecution(nil, nil)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Status != ScoreStatusNotApplicable || got.Score != nil {
		t.Fatalf("not-applicable score = %+v, want nil numeric score", got)
	}
}

func TestScoreExecution_PinnedCaseFractions(t *testing.T) {
	tests := []struct {
		name       string
		cases      []PinnedCaseEvidence
		wantScore  float64
		wantPassed int
	}{
		{"all passed", []PinnedCaseEvidence{{ID: "a", Status: "passed"}, {ID: "b", Status: "passed"}}, 1, 2},
		{"manual earns credit", []PinnedCaseEvidence{{ID: "a", Status: "manual"}, {ID: "b", Status: "failed"}}, 0.5, 1},
		{"absent case earns zero", []PinnedCaseEvidence{{ID: "a", Status: "passed"}}, 0.5, 1},
		{"reported missing earns zero", []PinnedCaseEvidence{{ID: "a", Status: "missing"}, {ID: "b", Status: "failed"}}, 0, 0},
		{"duplicate verifier id earns once", []PinnedCaseEvidence{{ID: "a", Status: "passed"}, {ID: "a", Status: "passed"}, {ID: "b", Status: "manual"}}, 1, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, []string{"a", "b"}, 2, tc.cases))
			if err != nil {
				t.Fatalf("score: %v", err)
			}
			if got.Status != ScoreStatusScored || got.Score == nil || *got.Score != tc.wantScore ||
				got.PassedCaseCount != tc.wantPassed || got.PinnedCaseCount != 2 {
				t.Errorf("score = %+v, want %v (%d/2)", got, tc.wantScore, tc.wantPassed)
			}
			if got.Diagnostic != "" {
				t.Errorf("clean evidence diagnosed as %q", got.Diagnostic)
			}
		})
	}
}

// Invalid agent-emitted contracts remain visible as measured zeroes. They do
// not disappear from an aggregate's denominator.
func TestScoreExecution_InvalidContractsFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		ids      []string
		pinned   int
		cases    []PinnedCaseEvidence
		wantDiag string
	}{
		{"unknown id", []string{"a"}, 1, []PinnedCaseEvidence{{ID: "other", Status: "passed"}}, DiagnosticUnknownCaseID},
		{"unknown status", []string{"a"}, 1, []PinnedCaseEvidence{{ID: "a", Status: "skipped"}}, DiagnosticUnknownCaseStatus},
		{"duplicate analyst id", []string{"a", "a"}, 2, []PinnedCaseEvidence{{ID: "a", Status: "passed"}}, DiagnosticDuplicateAnalystCaseID},
		// "count mismatch" MOVED OUT of this fail-closed table 2026-08-19. It is
		// deliberately no longer fatal: test_cases_pinned is a redundant count of
		// test_case_ids ("Must equal the length of test_case_ids" per the analyst
		// schema), so a miscount voids nothing the ids do not already answer. It
		// now scores against the published ids and records the slip as a
		// diagnostic — see TestScoreExecution_CountMismatchScoresAgainstThePublishedIDs
		// in pinned_count_mismatch_test.go. Everything else here stays fatal.
		{"no pinned cases", []string{}, 0, nil, DiagnosticNoPinnedCases},
		{"conflicting duplicate verifier status", []string{"a"}, 1, []PinnedCaseEvidence{{ID: "a", Status: "passed"}, {ID: "a", Status: "failed"}}, DiagnosticConflictingCaseStatus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, tc.ids, tc.pinned, tc.cases))
			if err != nil {
				t.Fatalf("score: %v", err)
			}
			if got.Status != ScoreStatusInvalidEvidence || got.Score == nil || *got.Score != 0 || got.Diagnostic != tc.wantDiag {
				t.Errorf("got %+v, want invalid zero + %q", got, tc.wantDiag)
			}
		})
	}
}

func TestScoreExecution_MissingContractIsZeroButCorruptSnapshotIsError(t *testing.T) {
	missing, err := ScoreExecution(pinnedPolicy(), []byte(`{"stepResults":{}}`))
	if err != nil {
		t.Fatalf("missing contract: %v", err)
	}
	if missing.Status != ScoreStatusMissingContract || missing.Score == nil || *missing.Score != 0 ||
		missing.Diagnostic != DiagnosticMissingScoringContract {
		t.Errorf("missing contract = %+v, want agent-visible zero", missing)
	}

	_, err = ScoreExecution(pinnedPolicy(), []byte(`{"stepResults":`))
	if err == nil || !strings.Contains(err.Error(), "state snapshot") {
		t.Fatalf("corrupt ledger snapshot must be distinguishable by callers, got %v", err)
	}
}

func TestScoreExecution_MalformedStepBodiesAreInvalidEvidence(t *testing.T) {
	for name, snapshot := range map[string][]byte{
		"producer": []byte(`{"stepResults":{"analyze":{"analysis":{"test_case_ids":"a","test_cases_pinned":1}},"test":{"testing":{"cases":[]}}}}`),
		"verifier": []byte(`{"stepResults":{"analyze":{"analysis":{"test_case_ids":["a"],"test_cases_pinned":1}},"test":{"testing":{"cases":"passed"}}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ScoreExecution(pinnedPolicy(), snapshot)
			if err != nil {
				t.Fatalf("malformed agent evidence is a score verdict, not a ledger error: %v", err)
			}
			if got.Status != ScoreStatusInvalidEvidence || got.Diagnostic != DiagnosticMalformedEvidence {
				t.Fatalf("got %+v, want malformed-evidence zero", got)
			}
		})
	}
}

// Regression, measured 2026-08-18 against a live bench execution
// (exec_20260818134714_54764ae9f44c88dd): the analyst published 13 pinned ids
// and the tester never landed, and the verdict was
// missing_contract/missing_scoring_contract with PinnedCaseCount 0 — identical
// to an execution where NEITHER half produced anything.
//
// That collapse matters because those two states call for opposite responses.
// Neither half means the workflow does not really carry the contract; producer-
// only means the contract is live and the VERIFIER is the thing that failed,
// which is the state the operator asked the quality page to make loud. A single
// diagnostic and a zeroed denominator hid the distinction in both the journal
// and the UI.
func TestScoreExecution_MissingContractNamesTheHalfThatIsMissing(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   string
		wantDiag   string
		wantPinned int
	}{
		{
			name:     "neither step",
			snapshot: `{"stepResults":{}}`,
			wantDiag: DiagnosticMissingScoringContract,
		},
		{
			name:     "producer missing",
			snapshot: `{"stepResults":{"test":{"testing":{"cases":[{"id":"case_1","status":"passed"}]}}}}`,
			wantDiag: DiagnosticMissingProducerStep,
		},
		{
			name:       "verifier missing, producer pinned 13",
			snapshot:   `{"stepResults":{"analyze":{"analysis":{"test_case_ids":["c1","c2","c3","c4","c5","c6","c7","c8","c9","c10","c11","c12","c13"],"test_cases_pinned":13}}}}`,
			wantDiag:   DiagnosticMissingVerifierStep,
			wantPinned: 13,
		},
		{
			name:     "verifier missing and producer evidence unusable",
			snapshot: `{"stepResults":{"analyze":{"analysis":{"complexity":"standard"}}}}`,
			wantDiag: DiagnosticMissingVerifierStep,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScoreExecution(pinnedPolicy(), []byte(tc.snapshot))
			if err != nil {
				t.Fatalf("score: %v", err)
			}
			if got.Status != ScoreStatusMissingContract || got.Score == nil || *got.Score != 0 {
				t.Fatalf("got %+v, want a missing-contract zero", got)
			}
			if got.Diagnostic != tc.wantDiag {
				t.Errorf("diagnostic = %q, want %q", got.Diagnostic, tc.wantDiag)
			}
			if got.PinnedCaseCount != tc.wantPinned {
				t.Errorf("pinned = %d, want %d — the denominator the producer published "+
					"is what tells an operator the contract is live",
					got.PinnedCaseCount, tc.wantPinned)
			}
		})
	}
}
