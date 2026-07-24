package taskcreate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// fcUsageRepo drives ForecastTask via AggregateByRoleModel (per-step cost) and
// budget.Check via SumCostByProject, satisfying the full repo interface.
type fcUsageRepo struct{ perStepCost float64 }

func (f *fcUsageRepo) Record(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (f *fcUsageRepo) Upsert(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (f *fcUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (f *fcUsageRepo) SumCostByTask(context.Context, string) (float64, error) { return 0, nil }
func (f *fcUsageRepo) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (f *fcUsageRepo) SumCost(context.Context, time.Time, time.Time) (float64, error) { return 0, nil }
func (f *fcUsageRepo) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	return []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: f.perStepCost, StepCount: 1}}, nil
}
func (f *fcUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (f *fcUsageRepo) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (f *fcUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (f *fcUsageRepo) TopTasks(context.Context, time.Time, time.Time, int, string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (f *fcUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}
func (f *fcUsageRepo) SumCostByAPIKey(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (f *fcUsageRepo) MeanCostByWorkflow(context.Context, string, string, time.Time, time.Time) (float64, int, error) {
	return 0, 0, nil
}

func budgetProjectRegistry(t *testing.T, budgetYAML string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("swarms/coder.md", "---\nswarmId: \"cs\"\nroles:\n  - name: \"coder\"\n    model: \"m\"\n    runtime:\n      image: \"alpine:latest\"\n---\n")
	write("workflows/build.md", "---\nworkflowId: \"build\"\nentrypoint: \"s1\"\nsteps:\n  s1:\n    type: \"agent\"\n    role: \"coder\"\n    prompt: \"go\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n")
	write("projects/demo.yaml", "projectId: \"demo\"\ndisplayName: \"D\"\nswarmId: \"cs\"\ndefaultWorkflowId: \"build\"\n"+budgetYAML)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	return reg
}

// TestCreate_ForecastRefusesOverPerTaskBudget — I4 regression: the shared
// Creator (which backs POST /tasks AND the companion MCP delegate) refuses a
// create whose run forecast exceeds the per-task budget.
func TestCreate_ForecastRefusesOverPerTaskBudget(t *testing.T) {
	reg := budgetProjectRegistry(t, "budget:\n  default_task_budget_usd: 0.5\n")
	c := New(
		WithTaskRepository(&mocks.MockTaskRepository{}),
		WithProjectRegistry(reg),
		WithLLMUsageRepository(&fcUsageRepo{perStepCost: 10.0}),
	)
	_, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "x"})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonBudgetExceeded {
		t.Fatalf("forecast $10 > per-task budget $0.50 → want ReasonBudgetExceeded, got %v", err)
	}
}

// TestCreate_ForecastShortCircuitsWhenNothingConfigured — M6/backward-compat:
// no per-task budget AND no project cap ⇒ the forecast gate short-circuits; a
// large forecast does NOT refuse the create.
func TestCreate_ForecastShortCircuitsWhenNothingConfigured(t *testing.T) {
	reg := budgetProjectRegistry(t, "") // no budget block
	c := New(
		WithTaskRepository(&mocks.MockTaskRepository{}),
		WithProjectRegistry(reg),
		WithLLMUsageRepository(&fcUsageRepo{perStepCost: 10.0}),
	)
	task, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "x"})
	if err != nil {
		t.Fatalf("nothing configured → gate must short-circuit, got %v", err)
	}
	if task == nil {
		t.Fatal("expected a task on the short-circuit path")
	}
}
