package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

func TestComputeWorkflowDiff_AddRemoveContext(t *testing.T) {
	current := "a\nb\nc\n"
	proposed := "a\nB2\nc\nd\n"
	diff := computeWorkflowDiff(current, proposed)

	// Expected: a(context), b→removed, B2→added, c(context), d→added.
	added, removed := diffStats(diff)
	if added != 2 || removed != 1 {
		t.Fatalf("stats = +%d/−%d, want +2/−1; diff=%+v", added, removed, diff)
	}
	// First line is unchanged context.
	if diff[0].Op != diffContext || diff[0].Text != "a" {
		t.Errorf("first line should be context 'a', got %+v", diff[0])
	}
}

func TestComputeWorkflowDiff_Identical(t *testing.T) {
	s := "x\ny\nz\n"
	diff := computeWorkflowDiff(s, s)
	added, removed := diffStats(diff)
	if added != 0 || removed != 0 {
		t.Fatalf("identical inputs should have no add/remove; got +%d/−%d", added, removed)
	}
	for _, l := range diff {
		if l.Op != diffContext {
			t.Fatalf("identical inputs should be all context; got %+v", l)
		}
	}
}

func TestComputeWorkflowDiff_EmptyCurrentIsAllAdd(t *testing.T) {
	diff := computeWorkflowDiff("", "p\nq\n")
	added, removed := diffStats(diff)
	if added != 2 || removed != 0 {
		t.Fatalf("empty current should be all-add; got +%d/−%d", added, removed)
	}
}

func TestPredictedImpactSummary(t *testing.T) {
	p := &persistence.WorkflowProposal{
		Kind:           persistence.WorkflowProposalKindChangeTimeout,
		Confidence:     0.82,
		EvidenceRunIDs: []string{"e1", "e2", "e3"},
	}
	got := predictedImpactSummary(p, 3, 1, true)
	for _, want := range []string{"change timeout", "+3/−1", "82%", "3 evidence"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	// Nil proposal is safe.
	if predictedImpactSummary(nil, 0, 0, false) != "" {
		t.Error("nil proposal should yield empty summary")
	}
	// Unspecified kind + no diff still produces a confidence/evidence line.
	bare := predictedImpactSummary(&persistence.WorkflowProposal{Confidence: 0.5}, 0, 0, false)
	if !strings.Contains(bare, "unspecified") || !strings.Contains(bare, "50%") {
		t.Errorf("bare summary unexpected: %q", bare)
	}
}

func TestBuildWorkflowBaseline_NilRollupFallsBack(t *testing.T) {
	if buildWorkflowBaseline(&persistence.WorkflowProposal{}, nil, 30) != nil {
		t.Error("nil rollup must yield nil baseline so the caller falls back to the heuristic")
	}
}

func TestBuildWorkflowBaseline_ComputesRate(t *testing.T) {
	p := &persistence.WorkflowProposal{
		Kind:       persistence.WorkflowProposalKindAddStep,
		Confidence: 0.9,
	}
	r := &workflowtelemetry.Rollup{
		RunCount: 40, FailureCount: 10, AvgCostUSD: 0.05,
		TopFailureClasses: []workflowtelemetry.FailureClassCount{{ErrorClass: "timeout", Count: 4}},
	}
	b := buildWorkflowBaseline(p, r, 30)
	if b == nil {
		t.Fatal("expected baseline")
	}
	if !b.HasRuns {
		t.Error("40 runs should set HasRuns")
	}
	if b.FailureRatePct != 25 {
		t.Errorf("failure rate = %v, want 25", b.FailureRatePct)
	}
	if b.WindowDays != 30 || b.AvgCostUSD != 0.05 || len(b.TopFailureClasses) != 1 {
		t.Errorf("baseline fields off: %+v", b)
	}
	if !strings.Contains(b.DirectionHint, "reduce failures") || !strings.Contains(b.DirectionHint, "90%") {
		t.Errorf("direction hint unexpected: %q", b.DirectionHint)
	}
}

func TestBuildWorkflowBaseline_ZeroRunsNoDivByZero(t *testing.T) {
	b := buildWorkflowBaseline(&persistence.WorkflowProposal{}, &workflowtelemetry.Rollup{RunCount: 0, FailureCount: 0}, 30)
	if b == nil {
		t.Fatal("expected baseline even with zero runs")
	}
	if b.HasRuns {
		t.Error("zero runs must leave HasRuns false")
	}
	if b.FailureRatePct != 0 {
		t.Errorf("zero runs must yield 0 rate, got %v", b.FailureRatePct)
	}
}
