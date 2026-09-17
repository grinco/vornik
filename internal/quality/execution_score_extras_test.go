package quality

import "testing"

// An id the analyst did not pin used to void the WHOLE report:
// decodeVerifierCases returned DiagnosticUnknownCaseID on the first one and the
// task scored a measured zero.
//
// Measured on the 2026.9.4 scored arm, dp-01-nilguard: the analyst pinned 14
// ids, the verifier reported all 14 as `passed` AND three more that also
// passed, and the task recorded 0.000. A run that validated every pinned case
// scored identically to one that validated none. Across 14 scored tasks, not a
// single case anywhere was reported `failed` or `missing` — every point of
// score loss was id bookkeeping.
//
// Extras are now IGNORED for the score and counted for the operator. Omissions
// are deliberately unchanged: the pinned set is the contract of what must be
// validated, and a verifier that cannot validate a case has `missing` to say so.
func TestScoreExecution_ExtraCasesDoNotVoidTheReport(t *testing.T) {
	// 14/14 pinned passed, plus 3 extras that also passed.
	cases := []PinnedCaseEvidence{
		{ID: "a", Status: "passed"},
		{ID: "b", Status: "passed"},
		{ID: "x1", Status: "passed"},
		{ID: "x2", Status: "passed"},
	}
	got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, []string{"a", "b"}, 2, cases))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Status != ScoreStatusScored {
		t.Fatalf("status = %q, want scored — extras must not invalidate evidence", got.Status)
	}
	if got.Score == nil || *got.Score != 1 {
		t.Errorf("score = %v, want 1.0: every pinned case passed", got.Score)
	}
	if got.PassedCaseCount != 2 || got.PinnedCaseCount != 2 {
		t.Errorf("counts = %d/%d, want 2/2", got.PassedCaseCount, got.PinnedCaseCount)
	}
	if got.ExtraCaseCount != 2 {
		t.Errorf("ExtraCaseCount = %d, want 2 — the extras must stay VISIBLE, not silently dropped", got.ExtraCaseCount)
	}
	if got.Diagnostic != DiagnosticUnknownCaseID {
		t.Errorf("diagnostic = %q, want %q kept as a soft note", got.Diagnostic, DiagnosticUnknownCaseID)
	}
}

// The renumbering case: the verifier reported a wholly disjoint id set
// (dp-09-atomic-write reported case_1..case_6 against pinned s1_case_1..).
// Nothing pinned was validated, so this must STILL score zero — but as a
// scored zero with its extras visible, not as unusable evidence.
func TestScoreExecution_DisjointIDsStillScoreZero(t *testing.T) {
	cases := []PinnedCaseEvidence{{ID: "case_1", Status: "passed"}, {ID: "case_2", Status: "passed"}}
	got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, []string{"a", "b"}, 2, cases))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Score == nil || *got.Score != 0 {
		t.Errorf("score = %v, want 0: no pinned case was validated", got.Score)
	}
	if got.ExtraCaseCount != 2 {
		t.Errorf("ExtraCaseCount = %d, want 2", got.ExtraCaseCount)
	}
	if got.Status != ScoreStatusScored {
		t.Errorf("status = %q, want scored", got.Status)
	}
}

// Omissions keep costing, unchanged: 1 of 2 pinned reported is 0.5, not 1.0.
// Scoring only what was reported would let a verifier that checks one case and
// passes it claim a perfect task.
func TestScoreExecution_OmissionsStillCount(t *testing.T) {
	got, err := ScoreExecution(pinnedPolicy(), scoreSnapshot(t, []string{"a", "b"}, 2,
		[]PinnedCaseEvidence{{ID: "a", Status: "passed"}}))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Score == nil || *got.Score != 0.5 {
		t.Errorf("score = %v, want 0.5 — an unreported pinned case is an unvalidated one", got.Score)
	}
	if got.ExtraCaseCount != 0 {
		t.Errorf("ExtraCaseCount = %d, want 0", got.ExtraCaseCount)
	}
}
