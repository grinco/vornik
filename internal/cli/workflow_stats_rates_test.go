package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderTo runs the renderer into a temp file and returns what it wrote.
func renderTo(t *testing.T, r *workflowStatsResponse) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := renderWorkflowStats(f, r); err != nil {
		t.Fatalf("renderWorkflowStats: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// TestWorkflowStats_ShowsBothRatesNamed — regression for "a workflow scoring
// surface presents step-level quality as if it were run success rate".
//
// The motivating case, 2026-08-26: plan-and-write had failed ZERO of fourteen
// runs and was shown as "67%" — passing-steps over total-steps across its three
// steps (27/40). The operator was about to prioritise work on a workflow with a
// 100% completion rate because a number implied it was failing a third of the
// time. A metric that misroutes attention is worse than no metric.
//
// Both numbers are real and both are useful; conflating them is the defect. So
// both are printed, each NAMED for what it measures — "success rate" as a bare
// phrase is exactly what let one stand in for the other.
func TestWorkflowStats_ShowsBothRatesNamed(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{
		WorkflowID:   "plan-and-write",
		RunCount:     14,
		SuccessCount: 14,
		Steps: []workflowStatsStep{
			{StepID: "plan", OutcomeDist: map[string]int{"ok": 9, "failed": 5}},
			{StepID: "write", OutcomeDist: map[string]int{"ok": 9, "failed": 4}},
			{StepID: "review", OutcomeDist: map[string]int{"ok": 9, "failed": 4}},
		},
	})

	if !strings.Contains(got, "run success: 100% (14/14 runs)") {
		t.Errorf("run success rate missing or wrong:\n%s", got)
	}
	// 27/40 = 67.5%, rendered rounded rather than truncated. The backlog item
	// quotes "67%" from the surface that truncated; the counts beside it are
	// what settle any doubt, which is why they are printed.
	if !strings.Contains(got, "step pass (first attempt): 68% (27/40 step attempts)") {
		t.Errorf("step pass rate missing or wrong:\n%s", got)
	}
	// The bare phrase is the one that misled; it must not appear alone.
	for _, banned := range []string{"success rate: 67", "success rate: 68"} {
		if strings.Contains(got, banned) {
			t.Errorf("the step rate is presented as a success rate:\n%s", got)
		}
	}
}

// The disagreement is called out explicitly in exactly the case that caused the
// confusion — every run completed while the step rate sits below 100%.
func TestWorkflowStats_ExplainsTheGapWhenEveryRunCompleted(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{
		WorkflowID: "plan-and-write", RunCount: 14, SuccessCount: 14,
		Steps: []workflowStatsStep{{StepID: "plan", OutcomeDist: map[string]int{"ok": 27, "failed": 13}}},
	})
	if !strings.Contains(got, "retry ladder doing its job") {
		t.Errorf("the reader is left to notice two numbers disagree:\n%s", got)
	}
}

// A workflow that genuinely fails runs must NOT get the reassuring note.
func TestWorkflowStats_NoReassuranceWhenRunsActuallyFail(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{
		WorkflowID: "companion-doc-review", RunCount: 12, SuccessCount: 9, FailureCount: 3,
		Steps: []workflowStatsStep{{StepID: "review", OutcomeDist: map[string]int{"ok": 5, "failed": 7}}},
	})
	if strings.Contains(got, "retry ladder doing its job") {
		t.Errorf("a workflow that failed 3 of 12 runs was described as healthy:\n%s", got)
	}
	if !strings.Contains(got, "run success: 75% (9/12 runs)") {
		t.Errorf("run success rate missing or wrong:\n%s", got)
	}
}

// A workflow with no step outcomes still reports run success — the step half is
// omitted rather than printed as a misleading zero.
func TestWorkflowStats_NoStepsOmitsTheStepRate(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{WorkflowID: "w", RunCount: 4, SuccessCount: 4})
	if !strings.Contains(got, "run success: 100% (4/4 runs)") {
		t.Errorf("run success missing:\n%s", got)
	}
	if strings.Contains(got, "step pass") {
		t.Errorf("a step rate was printed with no step outcomes to compute it from:\n%s", got)
	}
}

// TestWorkflowStats_FirstAttemptExcludesRetryRungs — regression for the review
// finding of 2026-09-03: the executor persists every retry rung as its own row
// under a suffixed step id (_shape_retry, _model_fallback, _infra_retryN, …),
// and the "first attempt" rate summed those rows as if each were a step's first
// try. A rung's own `ok` is the ladder rescuing a step, not the step passing
// first time; counting it inflated the rate most where the ladder works best.
func TestWorkflowStats_FirstAttemptExcludesRetryRungs(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{
		WorkflowID:   "adaptive",
		RunCount:     10,
		SuccessCount: 10,
		Steps: []workflowStatsStep{
			{StepID: "route", OutcomeDist: map[string]int{"ok": 4, "failed": 6}},
			{StepID: "route_shape_retry", OutcomeDist: map[string]int{"ok": 3, "failed": 1}},
			{StepID: "route_model_fallback", OutcomeDist: map[string]int{"ok": 2}},
			{StepID: "route_infra_retry1", OutcomeDist: map[string]int{"ok": 1}},
			{StepID: "write", OutcomeDist: map[string]int{"ok": 10}},
		},
	})
	// route 4/10 + write 10/10 = 14/20. With the rungs folded in it read 20/27.
	if !strings.Contains(got, "step pass (first attempt): 70% (14/20 step attempts)") {
		t.Errorf("first-attempt rate counts retry rungs as first attempts:\n%s", got)
	}
}

// TestWorkflowStats_OrphanedAttemptsLeaveTheDenominator — the regression for
// the third misreading in this family (design
// 2026-09-04-orphaned-step-outcomes §1).
//
// `adaptive`'s `route` step recorded 294 attempts over 30 days, of which 200
// were `orphaned`: the task started a new run and the execution was cancelled
// before anything learned the step's outcome. Counting them as non-passes
// printed "6% first-pass", and a backlog item read that as "a step whose first
// attempt is decorative". Over the attempts that actually concluded it is 18%.
//
// The count is PRINTED, not quietly dropped. A number that rose from 6% to 18%
// without saying what left the denominator would be the same misdirection
// pointed the other way.
func TestWorkflowStats_OrphanedAttemptsLeaveTheDenominator(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{
		WorkflowID:   "adaptive",
		RunCount:     515,
		SuccessCount: 220,
		Steps: []workflowStatsStep{
			{StepID: "route", OutcomeDist: map[string]int{
				"orphaned": 200, "failed": 61, "ok": 17, "timeout": 15, "iteration_exhausted": 1,
			}},
		},
	})

	if !strings.Contains(got, "step pass (first attempt): 18% (17/94 step attempts; 200 orphaned attempts excluded)") {
		t.Errorf("orphaned attempts are still in the denominator, or the exclusion is unnamed:\n%s", got)
	}
	if strings.Contains(got, "17/294") {
		t.Errorf("the orphaned rows are still counted as attempts:\n%s", got)
	}
}

// A workflow with no orphaned rows renders exactly as before — no phantom
// "0 orphaned attempts excluded" clause on every other workflow in the fleet.
func TestWorkflowStats_NoOrphanedClauseWhenThereAreNone(t *testing.T) {
	got := renderTo(t, &workflowStatsResponse{
		WorkflowID: "ingest", RunCount: 10, SuccessCount: 10,
		Steps: []workflowStatsStep{{StepID: "ingest", OutcomeDist: map[string]int{"ok": 8, "failed": 2}}},
	})
	if !strings.Contains(got, "step pass (first attempt): 80% (8/10 step attempts)") {
		t.Errorf("the unqualified rendering changed:\n%s", got)
	}
	if strings.Contains(got, "orphaned") {
		t.Errorf("a workflow with no orphaned rows must not mention them:\n%s", got)
	}
}
