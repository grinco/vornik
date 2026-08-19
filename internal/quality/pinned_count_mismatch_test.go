package quality

import (
	"strings"
	"testing"
)

// Measured on bench 2026-08-19: a dev-pipeline run scored `invalid_evidence` /
// `pinned_case_count_mismatch` because the analyst emitted
// test_case_ids = [case_1..case_4, s2_case_1..s2_case_3] (seven) alongside
// test_cases_pinned = 8. One miscounted integer voided the whole measurement.
//
// The schema settles which field is authoritative. test_cases_pinned is
// documented as "How many cases you pinned. Must equal the length of
// test_case_ids" — a count of a list the scorer can already see. Redundant
// state, contributing no information and one failure mode.
//
// So the published id list is the denominator. A case the analyst failed to
// publish cannot be validated by the tester, by the scorer, or by anyone else,
// whatever the count claims — and floring the score to zero over the
// bookkeeping loses a run that is otherwise perfectly gradeable.
//
// The mismatch is still recorded, because an analyst that cannot count its own
// list is worth knowing about. It just no longer destroys the evidence.
func TestScoreExecution_CountMismatchScoresAgainstThePublishedIDs(t *testing.T) {
	ids := []string{"case_1", "case_2", "case_3"}
	got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, ids, 4, []PinnedCaseEvidence{
		{ID: "case_1", Status: "passed"},
		{ID: "case_2", Status: "passed"},
		{ID: "case_3", Status: "failed"},
	}))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Status != ScoreStatusScored {
		t.Fatalf("status = %q, want scored — a miscounted integer must not void a run "+
			"whose per-case evidence is complete", got.Status)
	}
	if got.PinnedCaseCount != 3 {
		t.Errorf("denominator = %d, want 3 — the PUBLISHED ids are authoritative, not the "+
			"claimed count", got.PinnedCaseCount)
	}
	if got.PassedCaseCount != 2 || got.Score == nil || *got.Score != 2.0/3.0 {
		t.Errorf("got %d/%d score=%v, want 2/3", got.PassedCaseCount, got.PinnedCaseCount, got.Score)
	}
	// The analyst's slip must remain visible.
	if !strings.Contains(got.Diagnostic, DiagnosticPinnedCaseCountMismatch) {
		t.Errorf("diagnostic %q must still name the mismatch — an analyst that cannot "+
			"count its own list is worth knowing about", got.Diagnostic)
	}
}

// The genuinely unscorable contracts must stay fail-closed. Only the redundant
// count is being demoted; nothing else.
func TestScoreExecution_OtherInvalidContractsStillFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		ids      []string
		pinned   int
		cases    []PinnedCaseEvidence
		wantDiag string
	}{
		"unknown id":      {[]string{"a"}, 1, []PinnedCaseEvidence{{ID: "other", Status: "passed"}}, DiagnosticUnknownCaseID},
		"duplicate id":    {[]string{"a", "a"}, 2, []PinnedCaseEvidence{{ID: "a", Status: "passed"}}, DiagnosticDuplicateAnalystCaseID},
		"no pinned cases": {[]string{}, 0, nil, DiagnosticNoPinnedCases},
		"unknown status":  {[]string{"a"}, 1, []PinnedCaseEvidence{{ID: "a", Status: "skipped"}}, DiagnosticUnknownCaseStatus},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, tc.ids, tc.pinned, tc.cases))
			if err != nil {
				t.Fatalf("score: %v", err)
			}
			if got.Status != ScoreStatusInvalidEvidence || got.Diagnostic != tc.wantDiag {
				t.Errorf("got %+v, want invalid_evidence + %q", got, tc.wantDiag)
			}
		})
	}
}
