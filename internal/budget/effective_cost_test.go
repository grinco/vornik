package budget

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubSpendRepo lets tests dictate per-window spend without a DB.
// since/until determine which canned slice gets returned: tests
// pre-populate currentRows for the 24h window and baselineRows for
// the 7d window. The discriminator is the window length —
// AggregateByRoleModel doesn't carry a "window kind" arg so we infer
// from since.
type stubSpendRepo struct {
	current  []persistence.RoleModelSpend
	baseline []persistence.RoleModelSpend
	now      time.Time
	err      error
}

func (s *stubSpendRepo) AggregateByRoleModel(_ context.Context, since, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	if s.err != nil {
		return nil, s.err
	}
	gap := s.now.Sub(since)
	if gap <= 36*time.Hour {
		return s.current, nil
	}
	return s.baseline, nil
}

// stubOutcomeCounter mirrors stubSpendRepo on the outcome side.
type stubOutcomeCounter struct {
	current24  []persistence.RoleModelOutcomeCount
	baseline7d []persistence.RoleModelOutcomeCount
	now        time.Time
	err        error
}

func (s *stubOutcomeCounter) CountByRoleModelOutcome(_ context.Context, _ string, since, _ time.Time, _ string) ([]persistence.RoleModelOutcomeCount, error) {
	if s.err != nil {
		return nil, s.err
	}
	gap := s.now.Sub(since)
	if gap <= 36*time.Hour {
		return s.current24, nil
	}
	return s.baseline7d, nil
}

// recordingNotifier captures every fired alert so the test can
// assert on count + payload.
type recordingNotifier struct {
	mu     sync.Mutex
	alerts []EffectiveCostAlert
}

func (r *recordingNotifier) NotifyEffectiveCostDrift(_ context.Context, a EffectiveCostAlert) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
}

func (r *recordingNotifier) snapshot() []EffectiveCostAlert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EffectiveCostAlert, len(r.alerts))
	copy(out, r.alerts)
	return out
}

// build a monitor wired to the supplied stubs. Used by every test to
// avoid repeating the New + ctx setup boilerplate.
func newTestMonitor(t *testing.T, spend SpendRepo, outcomes OutcomeRepo, notifier EffectiveCostNotifier) *EffectiveCostMonitor {
	t.Helper()
	cfg := DefaultEffectiveCostConfig()
	m := NewEffectiveCostMonitor(cfg, spend, outcomes, notifier, zerolog.Nop())
	require.NotNil(t, m)
	m.ctx = context.Background()
	return m
}

// TestScanOnce_FiresWhenRatioExceedsThreshold — the headline case.
// Coder/qwen-coder is doing 5x worse over the last 24h than its
// 7d baseline; alert must fire with the right numbers.
func TestScanOnce_FiresWhenRatioExceedsThreshold(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			// 24h: $5 spent, 5 successes (will join below) → $1.00/ok
			{Role: "coder", Model: "qwen-coder", CostUSD: 5.0, StepCount: 8},
		},
		baseline: []persistence.RoleModelSpend{
			// 7d: $14 spent, 70 successes → $0.20/ok
			{Role: "coder", Model: "qwen-coder", CostUSD: 14.0, StepCount: 90},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: now,
		current24: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "qwen-coder", Count: 5},
		},
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "qwen-coder", Count: 70},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)
	m.now = func() time.Time { return now }

	m.ScanOnce()

	alerts := notifier.snapshot()
	require.Len(t, alerts, 1, "5x ratio must fire one alert")
	assert.Equal(t, "coder", alerts[0].Role)
	assert.Equal(t, "qwen-coder", alerts[0].Model)
	assert.InDelta(t, 1.0, alerts[0].Current24hUSDPerSuccess, 0.001, "5/5 = $1.00 per ok")
	assert.InDelta(t, 0.20, alerts[0].Baseline7dUSDPerSuccess, 0.001, "14/70 = $0.20 per ok")
	assert.Equal(t, 5.0, alerts[0].Ratio, "ratio = 1.00 / 0.20 = 5.00")
	assert.Equal(t, int64(5), alerts[0].Successes24h)
}

// TestScanOnce_BelowThresholdDoesNotFire — 1.5x is uncomfortable but
// not alert-worthy at the default 2x threshold.
func TestScanOnce_BelowThresholdDoesNotFire(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	// 24h: $3/10 = $0.30; baseline: $14/70 = $0.20 → ratio 1.5
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 3.0, StepCount: 12},
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: now,
		current24: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 10},
		},
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 70},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)
	m.now = func() time.Time { return now }

	m.ScanOnce()
	assert.Empty(t, notifier.snapshot(), "1.5x ratio is below the 2x threshold — must not fire")
}

// TestScanOnce_DedupesViaCooldown — same combo over threshold across
// two consecutive ticks must fire only once. The second tick is
// inside the cooldown window.
func TestScanOnce_DedupesViaCooldown(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 8},
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: now,
		current24: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 5},
		},
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 70},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)
	m.now = func() time.Time { return now }

	m.ScanOnce()
	m.ScanOnce()
	m.ScanOnce()

	assert.Len(t, notifier.snapshot(), 1,
		"three back-to-back ticks of the same regression must fire once — cooldown holds")
}

// TestScanOnce_FiresAgainAfterCooldown — after the 12h window, the
// same combo can re-alert. The clock advance is the trigger.
func TestScanOnce_FiresAgainAfterCooldown(t *testing.T) {
	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: t0,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 8},
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: t0,
		current24: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 5},
		},
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 70},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)

	m.now = func() time.Time { return t0 }
	m.ScanOnce()

	// Advance 13 hours — past the 12h cooldown.
	t1 := t0.Add(13 * time.Hour)
	spend.now = t1
	outcomes.now = t1
	m.now = func() time.Time { return t1 }
	m.ScanOnce()

	assert.Len(t, notifier.snapshot(), 2,
		"after the cooldown elapses, a sustained regression must re-fire")
}

// TestScanOnce_MinBaselineOksGuard — too few baseline successes
// makes the ratio statistically meaningless; alert must suppress.
func TestScanOnce_MinBaselineOksGuard(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 5},
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 0.50, StepCount: 5},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: now,
		current24: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 5},
		},
		// Only 3 baseline successes — below the default min-5.
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 3},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)
	m.now = func() time.Time { return now }

	m.ScanOnce()
	assert.Empty(t, notifier.snapshot(),
		"baseline with <5 successes must not fire — too little signal to trust the ratio")
}

// TestScanOnce_MinCurrentSpendGuard — a tiny spend over 24h is
// statistical noise even if the ratio is high. Default $0.10 floor.
func TestScanOnce_MinCurrentSpendGuard(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 0.05, StepCount: 1}, // below floor
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: now,
		current24: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 1},
		},
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 70},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)
	m.now = func() time.Time { return now }

	m.ScanOnce()
	assert.Empty(t, notifier.snapshot(), "spend below the $0.10 floor must not fire — signal-to-noise too low")
}

// TestScanOnce_NoSuccessesIn24hSkips — division by zero guard.
// Zero "ok" outcomes in the 24h window means the combo is either
// dead or all-failing; either way, the $/success metric is
// undefined and we skip silently.
func TestScanOnce_NoSuccessesIn24hSkips(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	spend := &stubSpendRepo{
		now: now,
		current: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 5.0, StepCount: 8},
		},
		baseline: []persistence.RoleModelSpend{
			{Role: "coder", Model: "m", CostUSD: 14.0, StepCount: 90},
		},
	}
	outcomes := &stubOutcomeCounter{
		now: now,
		// no current24 entry → 0 successes
		baseline7d: []persistence.RoleModelOutcomeCount{
			{Role: "coder", Model: "m", Count: 70},
		},
	}
	notifier := &recordingNotifier{}
	m := newTestMonitor(t, spend, outcomes, notifier)
	m.now = func() time.Time { return now }

	m.ScanOnce()
	assert.Empty(t, notifier.snapshot(),
		"zero successes in the current window must skip — ratio is undefined, not a 'cost is infinite' signal")
}

// TestNewEffectiveCostMonitor_NilReposReturnsNil — defensive shape:
// the daemon may be configured without one of the repos. The
// monitor refuses to construct rather than running and panicking.
func TestNewEffectiveCostMonitor_NilReposReturnsNil(t *testing.T) {
	assert.Nil(t, NewEffectiveCostMonitor(DefaultEffectiveCostConfig(), nil, &stubOutcomeCounter{}, nil, zerolog.Nop()))
	assert.Nil(t, NewEffectiveCostMonitor(DefaultEffectiveCostConfig(), &stubSpendRepo{}, nil, nil, zerolog.Nop()))
}

// TestStart_DisabledIsNoop mirrors the watchdog test: callers can
// invoke Start unconditionally and Enabled flag does the gating.
func TestStart_DisabledIsNoop(t *testing.T) {
	m := NewEffectiveCostMonitor(EffectiveCostConfig{Enabled: false}, &stubSpendRepo{}, &stubOutcomeCounter{}, nil, zerolog.Nop())
	require.NotNil(t, m)
	require.NoError(t, m.Start())
	assert.False(t, m.started, "disabled Start must not flip started")
	require.NoError(t, m.Stop())
}
