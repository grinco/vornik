package budget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// stubHistoryRepo returns canned (role, model) spend rows so the
// forecast tests can exercise both populated-history and cold-start
// paths without standing up a database.
type stubHistoryRepo struct {
	rows []persistence.RoleModelSpend
	err  error
}

func (s *stubHistoryRepo) AggregateByRoleModel(_ context.Context, _ time.Time, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// loadTestPricing writes a pricing.yaml with one known model and
// returns the loaded table. Helper used by the cold-start tests so
// the Forecast.USD value is something assertable.
func loadTestPricing(t *testing.T) *pricing.Table {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  cheap-model:
    input: 0.10
    output: 0.50
  premium-model:
    input: 3.00
    output: 15.00
`), 0o644))
	tbl, err := pricing.Load(path)
	require.NoError(t, err)
	return tbl
}

// makeWorkflow builds a small workflow with mixed step types so the
// forecast walks an agent step, a plan step, a gate (skipped), and
// a non-charging terminal lookalike. Each agent/plan step is wired
// to a role so the swarm-resolver branch is exercised.
func makeWorkflow() *registry.Workflow {
	return &registry.Workflow{
		ID:         "wf",
		Entrypoint: "plan",
		Steps: map[string]registry.WorkflowStep{
			"plan":      {Type: "plan", Role: "lead"},
			"implement": {Type: "agent", Role: "coder"},
			"review":    {Type: "agent", Role: "reviewer"},
			"gate":      {Type: "gate"}, // skipped — gates don't bill LLM calls
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
}

// makeSwarm assigns a model per role: lead and coder use the cheap
// model, reviewer uses the premium model. Lets tests assert the
// per-step model resolution lands the right pricing entry.
func makeSwarm() *registry.Swarm {
	return &registry.Swarm{
		ID: "s",
		Roles: []registry.SwarmRole{
			{Name: "lead", Model: "cheap-model", Runtime: registry.SwarmRoleRuntime{Image: "x"}},
			{Name: "coder", Model: "cheap-model", Runtime: registry.SwarmRoleRuntime{Image: "x"}},
			{Name: "reviewer", Model: "premium-model", Runtime: registry.SwarmRoleRuntime{Image: "x"}},
		},
	}
}

// TestForecastTask_HistoryWins — when historical data exists for a
// (role, model) pair, the forecast uses cost/step from history,
// NOT the cold-start pricing fallback. The two should give
// different numbers so we can tell which branch fired.
func TestForecastTask_HistoryWins(t *testing.T) {
	repo := &stubHistoryRepo{rows: []persistence.RoleModelSpend{
		// Lead: 100 steps for $5 → $0.05 per step
		{Role: "lead", Model: "cheap-model", CostUSD: 5.0, StepCount: 100},
		// Coder: 200 steps for $40 → $0.20 per step
		{Role: "coder", Model: "cheap-model", CostUSD: 40.0, StepCount: 200},
		// Reviewer: 50 steps for $30 → $0.60 per step
		{Role: "reviewer", Model: "premium-model", CostUSD: 30.0, StepCount: 50},
	}}
	tbl := loadTestPricing(t)

	f, err := ForecastTask(context.Background(), repo, tbl, ForecastInput{
		Workflow: makeWorkflow(), Swarm: makeSwarm(),
	}, time.Now())
	require.NoError(t, err)

	// Total: 0.05 + 0.20 + 0.60 = 0.85
	assert.InDelta(t, 0.85, f.USD, 0.001, "forecast must sum the per-step historical averages, got %.4f", f.USD)

	// Verify each chargeable step was sourced from history.
	historicalSteps := 0
	for _, s := range f.Steps {
		if s.StepID == "gate" {
			assert.Equal(t, "skip", s.Source, "gates must be marked skip and contribute zero")
			assert.Zero(t, s.USD)
			continue
		}
		assert.Equal(t, "history", s.Source, "step %s must use history when data exists", s.StepID)
		assert.Greater(t, s.SampleSize, 0)
		historicalSteps++
	}
	assert.Equal(t, 3, historicalSteps, "all three chargeable steps must contribute")
}

// TestForecastTask_ColdStartFallsBackToPricing — when history is
// empty, the forecast multiplies pricing × the conservative token
// estimate. Asserts the math rather than just "non-zero" so a
// later refactor of the constants doesn't silently change the
// forecast envelope.
func TestForecastTask_ColdStartFallsBackToPricing(t *testing.T) {
	repo := &stubHistoryRepo{} // empty
	tbl := loadTestPricing(t)

	f, err := ForecastTask(context.Background(), repo, tbl, ForecastInput{
		Workflow: makeWorkflow(), Swarm: makeSwarm(),
	}, time.Now())
	require.NoError(t, err)

	// Per-step cost for cheap-model:
	//   (30000 * 0.10 + 4000 * 0.50) / 1_000_000 = 0.005
	// Per-step cost for premium-model:
	//   (30000 * 3.00 + 4000 * 15.00) / 1_000_000 = 0.15
	// Workflow has 2× cheap (lead+coder) + 1× premium (reviewer)
	// + 1 gate (skipped) → 0.01 + 0.15 = 0.16
	assert.InDelta(t, 0.16, f.USD, 0.0001, "cold-start forecast must multiply pricing × token estimate, got %.4f", f.USD)
	for _, s := range f.Steps {
		if s.StepID == "gate" {
			continue
		}
		assert.Equal(t, "pricing", s.Source, "step %s must fall back to pricing when no history", s.StepID)
		assert.Zero(t, s.SampleSize, "cold-start sample size must be 0")
	}
}

// TestForecastTask_PartialHistoryFallsBackPerStep — history exists
// for the coder but not the lead or reviewer. The lead+reviewer
// must fall back to pricing while the coder uses its history. The
// total is the sum of the two sources — proves the per-step
// branching works in isolation.
func TestForecastTask_PartialHistoryFallsBackPerStep(t *testing.T) {
	repo := &stubHistoryRepo{rows: []persistence.RoleModelSpend{
		{Role: "coder", Model: "cheap-model", CostUSD: 40.0, StepCount: 200}, // $0.20 per step
	}}
	tbl := loadTestPricing(t)

	f, err := ForecastTask(context.Background(), repo, tbl, ForecastInput{
		Workflow: makeWorkflow(), Swarm: makeSwarm(),
	}, time.Now())
	require.NoError(t, err)

	srcByStep := make(map[string]string)
	for _, s := range f.Steps {
		srcByStep[s.StepID] = s.Source
	}
	assert.Equal(t, "pricing", srcByStep["plan"], "lead has no history → pricing")
	assert.Equal(t, "history", srcByStep["implement"], "coder has history → history")
	assert.Equal(t, "pricing", srcByStep["review"], "reviewer has no history → pricing")
}

// TestForecastTask_RepoErrorPropagates — a transient repo failure
// must surface so the caller can decide whether to fail-open
// (allow the task) or fail-closed (refuse). The forecast helper
// stays neutral.
func TestForecastTask_RepoErrorPropagates(t *testing.T) {
	repo := &stubHistoryRepo{err: errors.New("db is having a moment")}
	_, err := ForecastTask(context.Background(), repo, nil, ForecastInput{
		Workflow: makeWorkflow(),
	}, time.Now())
	assert.Error(t, err)
}

// TestForecastTask_NilWorkflow — defensive shape: a nil workflow or
// empty Steps map must return a zero forecast, not panic.
func TestForecastTask_NilWorkflow(t *testing.T) {
	repo := &stubHistoryRepo{}
	f, err := ForecastTask(context.Background(), repo, nil, ForecastInput{}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, f.USD)
	assert.Empty(t, f.Steps)

	f, err = ForecastTask(context.Background(), repo, nil, ForecastInput{
		Workflow: &registry.Workflow{ID: "empty"},
	}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, f.USD)
}

// TestCheckForecast_RefusesWhenForecastBreachesDailyCap — the
// integration math: project has $5 daily cap, $4.50 already spent,
// task forecast is $1 → refuse.
func TestCheckForecast_RefusesWhenForecastBreachesDailyCap(t *testing.T) {
	proj := &registry.Project{
		ID:     "p",
		Budget: registry.ProjectBudget{DailyHardUSD: 5.0},
	}
	current := Decision{DailyUSD: 4.5}
	forecast := Forecast{USD: 1.0}

	d := CheckForecast(proj, forecast, current)
	assert.True(t, d.Refused, "4.50 spent + 1.00 forecast = 5.50 > 5.00 cap → must refuse")
	assert.Contains(t, d.Reason, "daily hard cap",
		"refusal reason must name the cap that tripped so operators know which dial to turn")
}

// TestCheckForecast_RefusesWhenForecastBreachesMonthlyCap — same
// shape, monthly axis. Catches the case where a project's daily
// cap is loose but the monthly is tight (large one-shot tasks
// allowed within a day, but cumulative monthly spend gates them).
func TestCheckForecast_RefusesWhenForecastBreachesMonthlyCap(t *testing.T) {
	proj := &registry.Project{
		ID: "p",
		Budget: registry.ProjectBudget{
			DailyHardUSD:   100.0, // generous
			MonthlyHardUSD: 50.0,
		},
	}
	current := Decision{DailyUSD: 5.0, MonthlyUSD: 49.0}
	forecast := Forecast{USD: 2.0}

	d := CheckForecast(proj, forecast, current)
	assert.True(t, d.Refused)
	assert.Contains(t, d.Reason, "monthly hard cap")
}

// TestCheckForecast_AllowsWhenWellBelowCaps — happy path. Forecast
// + spent stays under both caps; no refusal.
func TestCheckForecast_AllowsWhenWellBelowCaps(t *testing.T) {
	proj := &registry.Project{
		ID: "p",
		Budget: registry.ProjectBudget{
			DailyHardUSD:   10.0,
			MonthlyHardUSD: 100.0,
		},
	}
	current := Decision{DailyUSD: 1.0, MonthlyUSD: 5.0}
	forecast := Forecast{USD: 0.50}

	d := CheckForecast(proj, forecast, current)
	assert.False(t, d.Refused, "well under both caps must not refuse")
	assert.Empty(t, d.Reason)
}

// TestCheckForecast_NoCaps — projects without configured caps must
// never refuse based on forecast (matches Check semantics).
func TestCheckForecast_NoCaps(t *testing.T) {
	proj := &registry.Project{ID: "p"} // empty Budget
	d := CheckForecast(proj, Forecast{USD: 999.0}, Decision{})
	assert.False(t, d.Refused, "no caps configured → forecast can't refuse, regardless of size")
}

// Compile-time assertion that the production usage repo satisfies
// HistoryRepo. Catches a future signature change at build time
// rather than runtime.
var _ HistoryRepo = (persistence.TaskLLMUsageRepository)(nil)
