package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/registry"
)

// stubBudgetRepo satisfies budget.Repo with pre-canned totals so the
// injection helper can be exercised without a database. Mirrors the
// shape budget_test.go uses, kept local so the executor test package
// doesn't depend on internal test types.
type stubBudgetRepo struct {
	daily   float64
	monthly float64
	err     error
}

func (s *stubBudgetRepo) SumCostByProject(_ context.Context, _ string, since, _ time.Time) (float64, error) {
	if s.err != nil {
		return 0, s.err
	}
	if since.Day() == 1 {
		return s.monthly, nil
	}
	return s.daily, nil
}

// Pin to a date that's NOT the 1st so stubBudgetRepo's heuristic
// returns daily for the daily query and monthly for the monthly query.
var midMonth = time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

// TestInjectBudgetEnv_BothCapsProduceRemainingValues — happy path: a
// project with both daily and monthly hard caps gets both
// VORNIK_BUDGET_*_REMAINING_USD env vars populated with the right
// arithmetic (cap minus spent).
func TestInjectBudgetEnv_BothCapsProduceRemainingValues(t *testing.T) {
	proj := &registry.Project{
		ID: "p1",
		Budget: registry.ProjectBudget{
			DailyHardUSD:   10,
			MonthlyHardUSD: 100,
		},
	}
	repo := &stubBudgetRepo{daily: 6.5, monthly: 42}
	env := map[string]string{}

	d, err := injectBudgetEnv(context.Background(), env, repo, proj, midMonth)
	require.NoError(t, err)
	assert.False(t, d.Blocked, "spend below caps must not block")

	// 10.00 - 6.50 = 3.5000
	assert.Equal(t, "3.5000", env["VORNIK_BUDGET_DAILY_REMAINING_USD"])
	// 100 - 42 = 58.0000
	assert.Equal(t, "58.0000", env["VORNIK_BUDGET_MONTHLY_REMAINING_USD"])
}

// TestInjectBudgetEnv_NoCapsConfigured — the agent must see no env
// vars when the project has no hard caps; the in-loop check then
// short-circuits and never trips the tripwire. Soft caps on their own
// don't enforce anything (they're a notification trigger, not a
// gate), so they shouldn't make the env vars appear either.
func TestInjectBudgetEnv_NoCapsConfigured(t *testing.T) {
	proj := &registry.Project{ID: "p1"}
	repo := &stubBudgetRepo{daily: 999, monthly: 999}
	env := map[string]string{"VORNIK_LLM_MODEL": "stays"}

	_, err := injectBudgetEnv(context.Background(), env, repo, proj, midMonth)
	require.NoError(t, err)

	_, hasD := env["VORNIK_BUDGET_DAILY_REMAINING_USD"]
	_, hasM := env["VORNIK_BUDGET_MONTHLY_REMAINING_USD"]
	assert.False(t, hasD, "no caps → no daily env var")
	assert.False(t, hasM, "no caps → no monthly env var")
	assert.Equal(t, "stays", env["VORNIK_LLM_MODEL"], "unrelated env keys must be untouched")
}

// TestInjectBudgetEnv_OnlyDailyCap — a project with a daily cap but no
// monthly cap should get only the daily env var. The agent's awk
// expression then defers to a sentinel for the missing monthly figure
// so the comparison only enforces the daily envelope.
func TestInjectBudgetEnv_OnlyDailyCap(t *testing.T) {
	proj := &registry.Project{
		ID:     "p1",
		Budget: registry.ProjectBudget{DailyHardUSD: 5},
	}
	repo := &stubBudgetRepo{daily: 3, monthly: 0}
	env := map[string]string{}

	_, err := injectBudgetEnv(context.Background(), env, repo, proj, midMonth)
	require.NoError(t, err)

	assert.Equal(t, "2.0000", env["VORNIK_BUDGET_DAILY_REMAINING_USD"])
	_, hasM := env["VORNIK_BUDGET_MONTHLY_REMAINING_USD"]
	assert.False(t, hasM, "no monthly cap → no monthly env var")
}

// TestInjectBudgetEnv_OverspentClampsToZero — when the project has
// already exceeded its cap (e.g. retry of a task whose first attempt
// pushed over), remaining is clamped to 0 instead of going negative.
// The agent reads 0 as "exhausted" and bails before its very next
// call.
func TestInjectBudgetEnv_OverspentClampsToZero(t *testing.T) {
	proj := &registry.Project{
		ID:     "p1",
		Budget: registry.ProjectBudget{DailyHardUSD: 5},
	}
	repo := &stubBudgetRepo{daily: 8} // 60% over cap
	env := map[string]string{}

	_, err := injectBudgetEnv(context.Background(), env, repo, proj, midMonth)
	require.NoError(t, err)

	assert.Equal(t, "0.0000", env["VORNIK_BUDGET_DAILY_REMAINING_USD"],
		"remaining must clamp to 0 — agent reading -3 would compute a negative envelope and skip the tripwire")
}

// TestInjectBudgetEnv_NilGuards — defensive coverage: nil env, nil
// project, and nil repo must all no-op without panicking.
func TestInjectBudgetEnv_NilGuards(t *testing.T) {
	proj := &registry.Project{
		ID:     "p1",
		Budget: registry.ProjectBudget{DailyHardUSD: 5},
	}
	repo := &stubBudgetRepo{daily: 1}

	// Nil env — caller misuse, but must not panic.
	_, err := injectBudgetEnv(context.Background(), nil, repo, proj, midMonth)
	assert.NoError(t, err)

	// Nil project — happens when the workflow resolver doesn't have
	// the project loaded yet.
	env := map[string]string{}
	_, err = injectBudgetEnv(context.Background(), env, repo, nil, midMonth)
	assert.NoError(t, err)
	assert.Empty(t, env)

	// Nil repo — usage repo not wired (some test paths). Same
	// behaviour as no caps configured.
	_, err = injectBudgetEnv(context.Background(), env, nil, proj, midMonth)
	assert.NoError(t, err)
	assert.Empty(t, env)
}

// TestInjectBudgetEnv_RepoErrorSurfaces — when budget.Check fails
// (transient DB error), the helper returns the error so the caller
// can log it. Env stays unwritten so the agent treats this as "no
// envelope" rather than reading stale or partial values.
func TestInjectBudgetEnv_RepoErrorSurfaces(t *testing.T) {
	proj := &registry.Project{
		ID:     "p1",
		Budget: registry.ProjectBudget{DailyHardUSD: 5},
	}
	repo := &stubBudgetRepo{err: errors.New("db is down")}
	env := map[string]string{}

	_, err := injectBudgetEnv(context.Background(), env, repo, proj, midMonth)
	assert.Error(t, err, "repo error must propagate so the caller can log it")
	_, hasD := env["VORNIK_BUDGET_DAILY_REMAINING_USD"]
	assert.False(t, hasD, "on error the env vars stay unset — agent treats absence as 'no enforcement', not stale data")
}

// Sanity: the helper's signature matches budget.Repo so production
// wiring can pass persistence.TaskLLMUsageRepository directly. This
// is a compile-time guard.
var _ budget.Repo = (*stubBudgetRepo)(nil)

func (s *stubBudgetRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
