package budget

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ---------------------------------------------------------------------------
// Decision.Period — classifies which envelope/level a decision tripped. It's
// driven purely off the Reason string prefix Check writes, so these tests pin
// the literal-prefix contract: a refactor of Check's Reason wording that drops
// the "monthly" token would silently misroute alerts to the daily channel.
// ---------------------------------------------------------------------------

func TestPeriod_DailyHard(t *testing.T) {
	d := Decision{Blocked: true, Reason: "daily budget exceeded: $6.00 spent of $5.00 hard cap"}
	period, level := d.Period()
	assert.Equal(t, "daily", period)
	assert.Equal(t, "hard", level)
}

func TestPeriod_MonthlyHard(t *testing.T) {
	d := Decision{Blocked: true, Reason: "monthly budget exceeded: $21.00 spent of $20.00 hard cap"}
	period, level := d.Period()
	assert.Equal(t, "monthly", period)
	assert.Equal(t, "hard", level)
}

func TestPeriod_DailySoft(t *testing.T) {
	d := Decision{SoftBreached: true, Reason: "daily soft budget breached: $7.00 spent of $5.00 soft cap"}
	period, level := d.Period()
	assert.Equal(t, "daily", period)
	assert.Equal(t, "soft", level)
}

func TestPeriod_MonthlySoft(t *testing.T) {
	d := Decision{SoftBreached: true, Reason: "monthly soft budget breached: $51.00 spent of $50.00 soft cap"}
	period, level := d.Period()
	assert.Equal(t, "monthly", period)
	assert.Equal(t, "soft", level)
}

func TestPeriod_UntrippedReturnsEmpty(t *testing.T) {
	// A clean decision (under all caps) classifies as nothing — callers
	// gate on Blocked/SoftBreached before calling Period, but the empty
	// return is the documented contract.
	period, level := Decision{}.Period()
	assert.Empty(t, period)
	assert.Empty(t, level)
}

// ---------------------------------------------------------------------------
// Check — boundary at exactly the cap. The implementation uses `>=`, so spend
// landing *exactly* on a hard cap must block, and exactly on a soft cap must
// warn. This is the load-bearing edge: a `>` regression would let a project
// spend right up to and at its cap before refusing.
// ---------------------------------------------------------------------------

func TestCheck_ExactlyAtDailyHardCapBlocks(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 10}}
	d, err := Check(context.Background(), &stubRepo{daily: 10.0, monthly: 0}, p, now)
	require.NoError(t, err)
	assert.True(t, d.Blocked, "spend == hard cap must block (>= semantics)")
	assert.Contains(t, d.Reason, "daily budget exceeded")
}

func TestCheck_JustUnderDailyHardCapAllows(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 10}}
	// One cent under — must not block and must not soft-breach (no soft cap).
	d, err := Check(context.Background(), &stubRepo{daily: 9.99, monthly: 0}, p, now)
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.False(t, d.SoftBreached)
}

func TestCheck_ExactlyAtDailySoftCapWarns(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailySoftUSD: 5, DailyHardUSD: 100}}
	d, err := Check(context.Background(), &stubRepo{daily: 5.0, monthly: 0}, p, now)
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.True(t, d.SoftBreached, "spend == soft cap must warn (>= semantics)")
}

func TestCheck_ExactlyAtMonthlySoftCapWarns(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	// Daily clean; monthly lands exactly on its soft cap. Daily checks run
	// first and pass, so the monthly-soft branch is the one that fires.
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{
		DailyHardUSD: 100, MonthlySoftUSD: 50, MonthlyHardUSD: 200,
	}}
	d, err := Check(context.Background(), &stubRepo{daily: 1, monthly: 50.0}, p, now)
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.True(t, d.SoftBreached)
	assert.Contains(t, d.Reason, "monthly soft budget breached")
}

func TestCheck_RepoErrorPropagates(t *testing.T) {
	// A daily-sum query failure must surface as an error so the gate caller
	// can decide its own fail-open / fail-closed policy.
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 10}}
	_, err := Check(context.Background(), &stubRepo{err: assert.AnError}, p, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sum daily spend")
}

// ---------------------------------------------------------------------------
// Reserve — monthly-axis blocking + custom-estimate boundary. The existing
// suite covers the daily block; this pins the monthly-period branch (which
// writes a distinct Reason) and the fail-open contract on a ledger error.
// ---------------------------------------------------------------------------

func TestReserve_BlocksOnMonthlyCap(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	// Daily generous (1000), monthly tight: committed 90 + reserved 8 +
	// estimate 5 = 103 > 100 monthly hard cap. The fake repo reports
	// Period="monthly", driving Reserve's monthly Reason branch.
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{
		DailyHardUSD: 1000, MonthlyHardUSD: 100, ReservationEstimateUSD: 5,
	}}
	f := &fakeReservationRepo{reservedSum: 8}
	d, err := Reserve(context.Background(), f, &stubRepo{daily: 90, monthly: 90}, p, "task-1", now)
	require.NoError(t, err)
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "monthly budget would be exceeded")
}

func TestReserve_LedgerErrorFailsOpenWithError(t *testing.T) {
	// IMPORTANT contract: a reservation-ledger failure returns an error so
	// the caller fails OPEN — a ledger glitch must never block legit work.
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 100}}
	f := &fakeReservationRepo{forceErr: assert.AnError}
	d, err := Reserve(context.Background(), f, &stubRepo{daily: 1, monthly: 1}, p, "task-1", now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserve budget")
	assert.False(t, d.Blocked, "error path must not return a Blocked decision — caller fails open on err")
}

func TestReserve_PassesCommittedSpendIntoLedger(t *testing.T) {
	// Reserve reads committed daily+monthly and threads them into the
	// ReserveRequest so the atomic check has the real committed baseline.
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{
		DailyHardUSD: 100, MonthlyHardUSD: 500,
	}}
	f := &fakeReservationRepo{}
	_, err := Reserve(context.Background(), f, &stubRepo{daily: 33, monthly: 222}, p, "task-9", now)
	require.NoError(t, err)
	assert.InDelta(t, 33.0, f.lastReq.DailyCommittedUSD, 1e-9)
	assert.InDelta(t, 222.0, f.lastReq.MonthlyCommittedUSD, 1e-9)
	assert.InDelta(t, 100.0, f.lastReq.DailyHardUSD, 1e-9)
	assert.InDelta(t, 500.0, f.lastReq.MonthlyHardUSD, 1e-9)
}

// ---------------------------------------------------------------------------
// resolveStepModel — per-step model resolution precedence (63.6% → exercise
// the role.Model and envVars branches and the fallbacks). Mirrors
// executor.effectiveRoleModel: role.Model wins, then envVars[VORNIK_LLM_MODEL],
// then the daemon default.
// ---------------------------------------------------------------------------

func TestResolveStepModel_RoleModelWins(t *testing.T) {
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "coder", Model: "pinned-model", Runtime: registry.SwarmRoleRuntime{
			EnvVars: map[string]string{"VORNIK_LLM_MODEL": "env-model"},
		}},
	}}
	step := registry.WorkflowStep{Type: "agent", Role: "coder"}
	got := resolveStepModel(step, swarm, "daemon-default")
	assert.Equal(t, "pinned-model", got, "role.Model takes precedence over envVars and default")
}

func TestResolveStepModel_EnvVarsWhenNoRoleModel(t *testing.T) {
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "coder", Runtime: registry.SwarmRoleRuntime{
			EnvVars: map[string]string{"VORNIK_LLM_MODEL": "env-model"},
		}},
	}}
	step := registry.WorkflowStep{Type: "agent", Role: "coder"}
	got := resolveStepModel(step, swarm, "daemon-default")
	assert.Equal(t, "env-model", got, "envVars override the daemon default when role.Model is empty")
}

func TestResolveStepModel_FallsBackToDefault(t *testing.T) {
	// Role found but neither Model nor a VORNIK_LLM_MODEL envVar set →
	// daemon default. Also covers the empty-role and nil-swarm short circuits.
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "coder", Runtime: registry.SwarmRoleRuntime{EnvVars: map[string]string{"LOG_LEVEL": "debug"}}},
	}}
	assert.Equal(t, "daemon-default",
		resolveStepModel(registry.WorkflowStep{Type: "agent", Role: "coder"}, swarm, "daemon-default"),
		"role present but no model/env → daemon default")
	assert.Equal(t, "daemon-default",
		resolveStepModel(registry.WorkflowStep{Type: "agent", Role: ""}, swarm, "daemon-default"),
		"empty role → daemon default")
	assert.Equal(t, "daemon-default",
		resolveStepModel(registry.WorkflowStep{Type: "agent", Role: "coder"}, nil, "daemon-default"),
		"nil swarm → daemon default")
}

func TestResolveStepModel_UnknownRoleFallsBackToDefault(t *testing.T) {
	// Step references a role the swarm doesn't define — the loop completes
	// without a match and falls through to the default.
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "coder", Model: "pinned"},
	}}
	got := resolveStepModel(registry.WorkflowStep{Type: "agent", Role: "ghost"}, swarm, "daemon-default")
	assert.Equal(t, "daemon-default", got)
}

// ---------------------------------------------------------------------------
// CheckForecast — boundary at exactly the cap + the nil-forecast no-cap path.
// Uses `>=`, so a projected total landing exactly on the hard cap must refuse.
// ---------------------------------------------------------------------------

func TestCheckForecast_ExactlyAtDailyCapRefuses(t *testing.T) {
	proj := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 5.0}}
	// 4.00 spent + 1.00 forecast = 5.00 == cap → refuse (>= semantics).
	d := CheckForecast(proj, Forecast{USD: 1.0}, Decision{DailyUSD: 4.0})
	assert.True(t, d.Refused, "projected == daily hard cap must refuse")
	assert.Contains(t, d.Reason, "daily hard cap")
}

func TestCheckForecast_NilProjectAllows(t *testing.T) {
	// Defensive: a nil project can't have caps, so the forecast can't refuse.
	d := CheckForecast(nil, Forecast{USD: 999}, Decision{DailyUSD: 999})
	assert.False(t, d.Refused)
	assert.Equal(t, 999.0, d.Forecast.USD, "forecast is echoed back even when project is nil")
}

// ---------------------------------------------------------------------------
// ForecastTask — non-agent/plan step types are marked "skip" and contribute
// zero, regardless of whether history would have matched. Pins the step-type
// switch independent of the gate-only case the existing tests cover.
// ---------------------------------------------------------------------------

func TestForecastTask_ApprovalAndTerminalStepsSkip(t *testing.T) {
	wf := &registry.Workflow{
		ID: "wf",
		Steps: map[string]registry.WorkflowStep{
			"approve":  {Type: "approval", Role: "human"},
			"terminal": {Type: "terminal"},
		},
	}
	// History rows exist that *would* match if the step were chargeable —
	// proves the skip happens on type, not on missing history.
	repo := &stubHistoryRepo{rows: []persistence.RoleModelSpend{
		{Role: "human", Model: "m", CostUSD: 100, StepCount: 1},
	}}
	f, err := ForecastTask(context.Background(), repo, nil, ForecastInput{Workflow: wf}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, f.USD, "approval + terminal steps must not bill")
	require.Len(t, f.Steps, 2)
	for _, s := range f.Steps {
		assert.Equal(t, "skip", s.Source)
		assert.Zero(t, s.USD)
	}
}

func TestForecastTask_NoHistoryNoPricingMarksUnknown(t *testing.T) {
	// Chargeable step, but nil pricing table and no history → "unknown"
	// source at $0 rather than inventing a number.
	wf := &registry.Workflow{
		ID:    "wf",
		Steps: map[string]registry.WorkflowStep{"impl": {Type: "agent", Role: "coder"}},
	}
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{{Name: "coder", Model: "some-model"}}}
	f, err := ForecastTask(context.Background(), &stubHistoryRepo{}, nil, ForecastInput{
		Workflow: wf, Swarm: swarm,
	}, time.Now())
	require.NoError(t, err)
	assert.Zero(t, f.USD)
	require.Len(t, f.Steps, 1)
	assert.Equal(t, "unknown", f.Steps[0].Source)
}

func TestForecastTask_DefaultLookbackWindow(t *testing.T) {
	// LookbackDays unset (0) must default to 30 and be reported back so the
	// caller's log line shows the real data window.
	wf := &registry.Workflow{ID: "wf", Steps: map[string]registry.WorkflowStep{
		"impl": {Type: "agent", Role: "coder"},
	}}
	f, err := ForecastTask(context.Background(), &stubHistoryRepo{}, nil, ForecastInput{Workflow: wf}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 30, f.LookbackDays, "zero LookbackDays must default to 30")
}

// ---------------------------------------------------------------------------
// DefaultDriftConfig (0% → pin the shipped defaults) + ComputeDrift behaviour:
// HasBaseline=false when the baseline window has too few oks, and the direction
// of the Ratio (improvement <1 vs regression >1).
// ---------------------------------------------------------------------------

func TestDefaultDriftConfig_Values(t *testing.T) {
	cfg := DefaultDriftConfig()
	assert.Equal(t, 24*time.Hour, cfg.CurrentWindow)
	assert.Equal(t, 7*24*time.Hour, cfg.BaselineWindow)
	assert.InDelta(t, 0.10, cfg.MinCurrentSpendUSD, 1e-9)
	assert.Equal(t, int64(5), cfg.MinBaselineOks)
}

func TestComputeDrift_RowWithoutBaselineWhenOksTooFew(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 10},
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 1.0, StepCount: 4},
		},
	}
	outcomes := &stubOutcomeCounter{
		now:        now,
		current24:  []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 5}},
		baseline7d: []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 3}}, // < min 5
	}
	rows, err := ComputeDrift(context.Background(), spend, outcomes, "", DefaultDriftConfig(), now)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the row is still returned with current numbers")
	r := rows[0]
	assert.False(t, r.HasBaseline, "fewer than MinBaselineOks → no baseline yet")
	assert.InDelta(t, 1.0, r.CurrentUSDPerOk, 1e-9, "5.00/5 = $1.00 per ok still computed")
	assert.Zero(t, r.Ratio, "ratio left at zero when there's no baseline")
}

func TestComputeDrift_ImprovementRatioBelowOne(t *testing.T) {
	// Cost-per-success fell vs baseline → ratio < 1 (an improvement, not a
	// regression). Confirms direction is encoded as current/baseline.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		// current: $2 / 10 oks = $0.20 per ok
		current: []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: 2.0, StepCount: 10}},
		// baseline: $40 / 100 oks = $0.40 per ok
		baseline: []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: 40.0, StepCount: 100}},
	}
	outcomes := &stubOutcomeCounter{
		now:        now,
		current24:  []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 10}},
		baseline7d: []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 100}},
	}
	rows, err := ComputeDrift(context.Background(), spend, outcomes, "", DefaultDriftConfig(), now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	r := rows[0]
	assert.True(t, r.HasBaseline)
	assert.InDelta(t, 0.5, r.Ratio, 0.001, "0.20 / 0.40 = 0.5 → improvement, ratio < 1")
}

func TestComputeDrift_ZeroDefaultsApplied(t *testing.T) {
	// A zero-valued DriftConfig must fall to the documented defaults
	// (24h/7d/min-5) rather than computing a degenerate window.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now:      now,
		current:  []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 10}},
		baseline: []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90}},
	}
	outcomes := &stubOutcomeCounter{
		now:        now,
		current24:  []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 5}},
		baseline7d: []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 70}},
	}
	// MinCurrentSpendUSD intentionally left 0 — ComputeDrift only defaults
	// CurrentWindow/BaselineWindow/MinBaselineOks, so a 0 floor lets the
	// $5 row through and the row computes against the 7d baseline.
	rows, err := ComputeDrift(context.Background(), spend, outcomes, "", DriftConfig{}, now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].HasBaseline, "70 baseline oks >= defaulted min 5")
}

// ---------------------------------------------------------------------------
// SetNotifier (0%) — attaching a notifier post-construction must route alerts
// through it on the next scan. Drives the deferred-wiring pattern the daemon
// uses (monitor built before the Telegram bot exists).
// ---------------------------------------------------------------------------

func TestSetNotifier_RoutesAlertsAfterAttach(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now:      now,
		current:  []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 8}},
		baseline: []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90}},
	}
	outcomes := &stubOutcomeCounter{
		now:        now,
		current24:  []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 5}},
		baseline7d: []persistence.RoleModelOutcomeCount{{Role: "coder", Model: "m", Count: 70}},
	}
	// Construct WITHOUT a notifier, then attach one.
	m := newTestMonitor(t, spend, outcomes, nil)
	m.now = func() time.Time { return now }
	notifier := &recordingNotifier{}
	m.SetNotifier(notifier)

	m.ScanOnce()
	assert.Len(t, notifier.snapshot(), 1, "alert must route through the notifier attached via SetNotifier")
}
