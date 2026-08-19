package ui

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func qs(workflow, status, diagnostic string, score *float64, pinned int) *persistence.ExecutionQualityScore {
	return &persistence.ExecutionQualityScore{
		ExecutionID: "exec_" + workflow + "_" + status, WorkflowID: workflow,
		Status: status, Diagnostic: diagnostic, Score: score, PinnedCaseCount: pinned,
	}
}

func f(v float64) *float64 { return &v }

// Measured 2026-08-17: execution_quality_scores held 7599 rows for 7599
// terminal executions and every single one was not_applicable with a NULL
// score, because no production workflow declared a scoring policy. The page
// rendered a bare "no rows" notice for a state that was not empty at all — the
// rows existed and were unanimously inapplicable.
//
// The three states have to be told apart because they call for three different
// responses: nothing in scope declares a policy (a configuration fact), scored
// (read the numbers), and declared-but-unproven (a defect — the state that
// consumed 2026-08-17 and that an "empty page" would have hidden).
func TestSummarizeExecutionQuality_SeparatesTheThreeStates(t *testing.T) {
	t.Run("nothing declares a policy", func(t *testing.T) {
		view := summarizeExecutionQuality([]*persistence.ExecutionQualityScore{
			qs("research", "not_applicable", "", nil, 0),
			qs("trading", "not_applicable", "", nil, 0),
			qs("research", "not_applicable", "", nil, 0),
		})
		if len(view.Declaring) != 0 {
			t.Errorf("Declaring = %+v, want empty", view.Declaring)
		}
		if len(view.Silent) != 2 {
			t.Errorf("Silent = %+v, want research and trading", view.Silent)
		}
		if view.Coverage.Workflows != 2 || view.Coverage.Declaring != 0 || view.Coverage.NotApplicable != 3 {
			t.Errorf("coverage = %+v, want 2 workflows, 0 declaring, 3 n/a", view.Coverage)
		}
		if !view.NoPolicyInScope {
			t.Error("NoPolicyInScope must be set so the page can say WHY it is empty " +
				"instead of implying the data is missing")
		}
	})

	t.Run("declared but unproven is not hidden behind a mean", func(t *testing.T) {
		view := summarizeExecutionQuality([]*persistence.ExecutionQualityScore{
			qs("dev-pipeline", "scored", "", f(0.8), 5),
			qs("dev-pipeline", "missing_contract", "missing_verifier_step", f(0), 13),
			qs("dev-pipeline", "missing_contract", "missing_verifier_step", f(0), 11),
			qs("dev-pipeline", "invalid_evidence", "unknown_case_id", f(0), 13),
			qs("research", "not_applicable", "", nil, 0),
		})
		if view.NoPolicyInScope {
			t.Error("dev-pipeline declares a policy; the page is not in the no-policy state")
		}
		if len(view.Declaring) != 1 || view.Declaring[0].WorkflowID != "dev-pipeline" {
			t.Fatalf("Declaring = %+v, want dev-pipeline only", view.Declaring)
		}
		got := view.Declaring[0]
		if got.Scored != 1 || got.MeanPercent != 80 {
			t.Errorf("scored=%d mean=%d, want 1 and 80 — a zeroed evidence gap must not "+
				"be averaged into the quality of the runs that did report",
				got.Scored, got.MeanPercent)
		}
		if got.AwaitingEvidence != 3 {
			t.Errorf("AwaitingEvidence = %d, want 3", got.AwaitingEvidence)
		}
		if view.Coverage.AwaitingEvidence != 3 {
			t.Errorf("coverage AwaitingEvidence = %d, want 3", view.Coverage.AwaitingEvidence)
		}
		// The diagnostic breakdown is the actionable part: two runs never
		// reached the verifier, one reported ids the analyst never pinned.
		want := map[string]int{"missing_verifier_step": 2, "unknown_case_id": 1}
		gotDiag := map[string]int{}
		for _, d := range view.EvidenceGaps {
			gotDiag[d.Diagnostic] = d.Count
		}
		if len(gotDiag) != len(want) {
			t.Fatalf("EvidenceGaps = %+v, want %v", view.EvidenceGaps, want)
		}
		for k, v := range want {
			if gotDiag[k] != v {
				t.Errorf("EvidenceGaps[%s] = %d, want %d", k, gotDiag[k], v)
			}
		}
		if view.EvidenceGaps[0].Diagnostic != "missing_verifier_step" {
			t.Errorf("gaps must lead with the commonest, got %+v", view.EvidenceGaps)
		}
	})

	t.Run("no rows at all", func(t *testing.T) {
		view := summarizeExecutionQuality(nil)
		if view.NoPolicyInScope {
			t.Error("an empty window is not the same as a window where nothing declares " +
				"a policy; the page must not claim a configuration fact it has no evidence for")
		}
	})
}
