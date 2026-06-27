package executor

// budget_consumer_test.go — Slice 4 executor-level seam tests (LLD §11, §10).
//
// Covers:
//  - recordLearnedBudgetApplication: application row recorded + metric bumped
//  - recordLearnedBudgetApplication: nil repo / empty instinctID safe no-ops
//  - WithInstinctToolBudget: option sets field correctly
//  - regression guard naming the seam (dynamic-tool-budget-design §10)

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
)

// TestRecordLearnedBudgetApplication_RecordsRowAndMetric verifies that when the
// active budget consumer supplies a learned tier, an instinct_applications row
// is written with surface="tool_budget" / result="ignored", and the
// vornik_instinct_applications_total{surface="tool_budget",result="ignored"}
// counter is incremented (LLD §7, metric label guard).
func TestRecordLearnedBudgetApplication_RecordsRowAndMetric(t *testing.T) {
	repo := &stubInstinctRepo{}
	reg := prometheus.NewRegistry()
	m := observability.NewInstinctMetrics(reg)

	e := &Executor{
		instinctRepo:    repo,
		instinctMetrics: m,
		logger:          zerolog.Nop(),
	}

	task := &persistence.Task{ID: "t-budget", ProjectID: "proj-budget"}
	exec := &persistence.Execution{ID: "exec-budget"}

	e.recordLearnedBudgetApplication(context.Background(), task, exec, "step-budget", "instinct-42")

	if len(repo.applications) != 1 {
		t.Fatalf("expected 1 application row, got %d", len(repo.applications))
	}
	app := repo.applications[0]
	if app.Surface != persistence.InstinctSurfaceToolBudget {
		t.Errorf("surface = %q, want %q", app.Surface, persistence.InstinctSurfaceToolBudget)
	}
	if app.Result != persistence.InstinctResultIgnored {
		t.Errorf("result = %q, want %q", app.Result, persistence.InstinctResultIgnored)
	}
	if app.InstinctID != "instinct-42" {
		t.Errorf("instinct_id = %q, want %q", app.InstinctID, "instinct-42")
	}
	if app.TaskID != "t-budget" {
		t.Errorf("task_id = %q, want t-budget", app.TaskID)
	}
	if app.ExecutionID != "exec-budget" {
		t.Errorf("execution_id = %q, want exec-budget", app.ExecutionID)
	}
	if app.StepID != "step-budget" {
		t.Errorf("step_id = %q, want step-budget", app.StepID)
	}

	// Verify the metric counter was incremented.
	// vornik_instinct_applications_total{surface="tool_budget",result="ignored"}
	count, err := testutil.GatherAndCount(reg, "vornik_instinct_applications_total")
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	if count == 0 {
		t.Fatal("expected vornik_instinct_applications_total to be present")
	}
	val := testutil.ToFloat64(m.ApplicationsTotal.WithLabelValues(
		persistence.InstinctSurfaceToolBudget, persistence.InstinctResultIgnored))
	if val != 1.0 {
		t.Errorf("applications_total{tool_budget,ignored} = %v, want 1.0", val)
	}
}

// TestRecordLearnedBudgetApplication_NilRepo_NoOp ensures a nil repo is safe.
func TestRecordLearnedBudgetApplication_NilRepo_NoOp(_ *testing.T) {
	e := &Executor{
		instinctRepo: nil,
		logger:       zerolog.Nop(),
	}
	// Must not panic.
	e.recordLearnedBudgetApplication(context.Background(),
		&persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "s1", "i1")
}

// TestRecordLearnedBudgetApplication_EmptyInstinctID_NoOp ensures empty ID is safe.
func TestRecordLearnedBudgetApplication_EmptyInstinctID_NoOp(t *testing.T) {
	repo := &stubInstinctRepo{}
	e := &Executor{
		instinctRepo: repo,
		logger:       zerolog.Nop(),
	}
	e.recordLearnedBudgetApplication(context.Background(),
		&persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "s1", "")
	if len(repo.applications) != 0 {
		t.Fatalf("empty instinctID: expected no application row, got %d", len(repo.applications))
	}
}

// TestWithInstinctToolBudget_WiresField pins the option-setter contract.
func TestWithInstinctToolBudget_WiresField(t *testing.T) {
	e, _, _, _, _ := setup()
	if e.instinctToolBudget {
		t.Fatal("instinctToolBudget should be false by default")
	}
	WithInstinctToolBudget(true)(e)
	if !e.instinctToolBudget {
		t.Fatal("WithInstinctToolBudget(true) did not set field")
	}
	WithInstinctToolBudget(false)(e)
	if e.instinctToolBudget {
		t.Fatal("WithInstinctToolBudget(false) did not clear field")
	}
}

// TestBudgetConsumerSeam_RegressionGuard is the regression-guard test naming
// the instinct ↔ tool-budget seam (dynamic-tool-budget-design.md §10,
// instinct-tool-budget-seam-design.md §11):
//
//	"toolbudget.Resolve already takes the verdict as a parameter …
//	 the Continuous-Learning Instinct Layer can later (a) supply a learned tier
//	 when the planner omits one."
//
// This test asserts:
//  1. The executor exposes instinctToolBudget as a first-class field
//     (not a comment or dead code).
//  2. WithInstinctToolBudget is a real Option that persists across a NewWithOptions call.
//  3. The gate is off by default (no budget instincts ever fire unless opted in).
func TestBudgetConsumerSeam_RegressionGuard(t *testing.T) {
	// The seam gate must default to off.
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	e := NewWithOptions(rt, er, ar, tr, nil)
	if e.instinctToolBudget {
		t.Fatal("seam gate must default OFF (no learned budget tiers unless operator opts in)")
	}

	// Wiring via option must flip the gate.
	e2 := NewWithOptions(rt, er, ar, tr, nil, WithInstinctToolBudget(true))
	if !e2.instinctToolBudget {
		t.Fatal("WithInstinctToolBudget(true) must set the gate")
	}
}
