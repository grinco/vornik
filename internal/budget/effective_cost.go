package budget

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// Effective-cost alerts surface model degradation: when a (role,
// model) combo's $/success — total LLM spend divided by count of
// "ok" outcomes — drifts up sharply against its recent baseline,
// the operator gets a Telegram message. Distinct from the budget
// breach alerts (which fire when *total* spend approaches a cap):
// this signal fires when the *quality of spend* degrades, even if
// total spend is healthy.
//
// The math is deliberately simple:
//   current24h  = sum_cost_24h / count_ok_24h
//   baseline7d  = sum_cost_7d  / count_ok_7d
//   if current24h >= ratio_threshold * baseline7d → alert
//
// Defaults pick numbers that catch real regressions without firing
// on routine variance:
//   ratio_threshold = 2.0   — must double, not just creep up
//   min_24h_spend   = $0.10 — a single tiny task isn't a signal
//   min_7d_oks      = 5     — fewer than 5 baseline successes is noise
//   cooldown        = 12h   — don't re-alert the same combo within
//                              half a day; degraded models stay
//                              degraded for a while

// EffectiveCostAlert is the payload the notifier receives. Captures
// every number the operator needs to decide whether to investigate
// or to suppress (raise the threshold).
type EffectiveCostAlert struct {
	// Role + Model identify the affected combo. The pair is the
	// natural unit because operators tune models per role
	// (e.g. coder=qwen-coder, reviewer=claude-haiku).
	Role  string
	Model string
	// Current24hUSDPerSuccess is the present cost-per-ok over the
	// 24h window. Compared against Baseline7dUSDPerSuccess.
	Current24hUSDPerSuccess float64
	// Baseline7dUSDPerSuccess is the rolling 7-day average — the
	// "is the model behaving as it usually does" reference.
	Baseline7dUSDPerSuccess float64
	// Ratio is Current24h / Baseline7d, rounded to two decimals.
	// Always >= 2.0 when an alert fires (the threshold).
	Ratio float64
	// Spend24hUSD is the absolute spend over the 24h window. Useful
	// for the operator to know whether the alert is on a $0.50/day
	// role or a $50/day one — the latter deserves immediate
	// attention.
	Spend24hUSD float64
	// Successes24h is the count of ok outcomes in the 24h window.
	// Pairs with Spend to give the operator the raw inputs for the
	// ratio.
	Successes24h int64
	// FiredAt is the wall-clock time the alert was emitted, used
	// by the cooldown table.
	FiredAt time.Time
}

// EffectiveCostNotifier is the sink for alerts. The Telegram bot
// implements it; tests use a recording stub. Distinct from
// budget.Notifier because this fires on a different signal — total
// spend may be healthy when the alert fires.
type EffectiveCostNotifier interface {
	NotifyEffectiveCostDrift(ctx context.Context, alert EffectiveCostAlert)
}

// SpendRepo is the cost half of the alert input: per-(role, model)
// total spend in a window. The production
// persistence.TaskLLMUsageRepository satisfies it.
type SpendRepo interface {
	AggregateByRoleModel(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.RoleModelSpend, error)
}

// OutcomeRepo is the success half: count of "ok" outcomes per
// (role, model) in a window. The production
// persistence.ExecutionStepOutcomeRepository satisfies it.
type OutcomeRepo interface {
	CountByRoleModelOutcome(ctx context.Context, outcome string, since, until time.Time, projectID string) ([]persistence.RoleModelOutcomeCount, error)
}

// DriftRow is one (role, model) snapshot of effective-cost drift.
// Returned by ComputeDrift; consumed by both the spend-dashboard
// drift column and the project-detail drift panel. Captures the
// raw inputs so the UI can render insights ("$/success doubled
// because successes halved") without re-running the underlying
// queries.
type DriftRow struct {
	Role  string
	Model string
	// CurrentSpendUSD is the total task_llm_usage cost over the
	// current window for this (role, model) pair.
	CurrentSpendUSD float64
	// CurrentOks is the count of execution_step_outcomes(outcome=ok)
	// for the same pair over the same window. Zero is suppressed
	// upstream: a pair with no successes can't have a $/success.
	CurrentOks int64
	// CurrentUSDPerOk is CurrentSpendUSD / CurrentOks. The headline
	// number on the drift panel.
	CurrentUSDPerOk float64
	// BaselineSpendUSD + BaselineOks are the matching numbers over
	// the longer baseline window (default 7d) — the "normal" the
	// current window is compared against.
	BaselineSpendUSD float64
	BaselineOks      int64
	BaselineUSDPerOk float64
	// Ratio is CurrentUSDPerOk / BaselineUSDPerOk. >1 means cost
	// per success is rising; >2 typically indicates a regression
	// worth investigating; <1 means improvement.
	Ratio float64
	// HasBaseline is false when there isn't enough baseline data
	// (BaselineOks < MinBaselineOks). The UI renders these rows as
	// "no baseline yet" rather than misleading $0.00 ratios.
	HasBaseline bool
}

// DriftConfig parameters for ComputeDrift. Same shape as the
// fields on EffectiveCostConfig that govern threshold/window
// math, exposed here so the UI can reuse the same semantics
// without instantiating a full monitor.
type DriftConfig struct {
	CurrentWindow      time.Duration
	BaselineWindow     time.Duration
	MinCurrentSpendUSD float64
	MinBaselineOks     int64
}

// DefaultDriftConfig returns the same defaults used by the
// monitor (24h current, 7d baseline, $0.10 spend floor, 5 baseline
// oks). UI handlers reuse these unless the operator overrides.
func DefaultDriftConfig() DriftConfig {
	return DriftConfig{
		CurrentWindow:      24 * time.Hour,
		BaselineWindow:     7 * 24 * time.Hour,
		MinCurrentSpendUSD: 0.10,
		MinBaselineOks:     5,
	}
}

// ComputeDrift assembles per-(role, model) drift rows for the
// supplied projectID (empty = global). Rows are returned for
// every pair with non-zero current spend AND current ok-count;
// pairs with insufficient baseline data are still returned with
// HasBaseline=false so the UI can render "no baseline yet"
// alongside the current numbers. The function is read-only and
// has no notification side effects — the cost dashboard and the
// per-project drift panel both call it directly.
//
// `now` is parameterised so tests can pin the time window
// boundaries; production passes time.Now().UTC().
func ComputeDrift(
	ctx context.Context,
	spendRepo SpendRepo,
	outcomeRepo OutcomeRepo,
	projectID string,
	cfg DriftConfig,
	now time.Time,
) ([]DriftRow, error) {
	if cfg.CurrentWindow <= 0 {
		cfg.CurrentWindow = 24 * time.Hour
	}
	if cfg.BaselineWindow <= 0 {
		cfg.BaselineWindow = 7 * 24 * time.Hour
	}
	if cfg.MinBaselineOks <= 0 {
		cfg.MinBaselineOks = 5
	}
	currentSince := now.Add(-cfg.CurrentWindow)
	baselineSince := now.Add(-cfg.BaselineWindow)

	spend24, err := spendRepo.AggregateByRoleModel(ctx, currentSince, now, 0, projectID)
	if err != nil {
		return nil, fmt.Errorf("drift: 24h spend: %w", err)
	}
	spend7d, err := spendRepo.AggregateByRoleModel(ctx, baselineSince, now, 0, projectID)
	if err != nil {
		return nil, fmt.Errorf("drift: 7d spend: %w", err)
	}
	oks24, err := outcomeRepo.CountByRoleModelOutcome(ctx, "ok", currentSince, now, projectID)
	if err != nil {
		return nil, fmt.Errorf("drift: 24h oks: %w", err)
	}
	oks7d, err := outcomeRepo.CountByRoleModelOutcome(ctx, "ok", baselineSince, now, projectID)
	if err != nil {
		return nil, fmt.Errorf("drift: 7d oks: %w", err)
	}

	type k struct{ role, model string }
	spend7dByKey := make(map[k]float64, len(spend7d))
	for _, s := range spend7d {
		spend7dByKey[k{s.Role, s.Model}] = s.CostUSD
	}
	oks24ByKey := make(map[k]int64, len(oks24))
	for _, o := range oks24 {
		oks24ByKey[k{o.Role, o.Model}] = o.Count
	}
	oks7dByKey := make(map[k]int64, len(oks7d))
	for _, o := range oks7d {
		oks7dByKey[k{o.Role, o.Model}] = o.Count
	}

	rows := make([]DriftRow, 0, len(spend24))
	for _, s := range spend24 {
		key := k{s.Role, s.Model}
		oks := oks24ByKey[key]
		if s.CostUSD < cfg.MinCurrentSpendUSD || oks == 0 {
			// Pair is either too small to compare (current-spend
			// floor) or has no successes — skipping keeps the
			// dashboard signal-rich. Operators investigating tiny
			// pairs query the data directly.
			continue
		}
		row := DriftRow{
			Role:            s.Role,
			Model:           s.Model,
			CurrentSpendUSD: s.CostUSD,
			CurrentOks:      oks,
			CurrentUSDPerOk: s.CostUSD / float64(oks),
		}
		baseSpend := spend7dByKey[key]
		baseOks := oks7dByKey[key]
		row.BaselineSpendUSD = baseSpend
		row.BaselineOks = baseOks
		if baseOks >= cfg.MinBaselineOks && baseSpend > 0 {
			row.BaselineUSDPerOk = baseSpend / float64(baseOks)
			if row.BaselineUSDPerOk > 0 {
				row.Ratio = row.CurrentUSDPerOk / row.BaselineUSDPerOk
				row.HasBaseline = true
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// EffectiveCostMonitor runs the periodic drift scan as a single
// background goroutine. Lifecycle mirrors Watchdog and Scheduler:
// Start spawns the loop, Stop cancels and waits.
type EffectiveCostMonitor struct {
	cfg         EffectiveCostConfig
	spendRepo   SpendRepo
	outcomeRepo OutcomeRepo
	notifier    EffectiveCostNotifier
	logger      zerolog.Logger
	now         func() time.Time

	mu      sync.Mutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// cooldown tracks (role, model) → last-alert time so the same
	// combo doesn't get re-flagged on every tick during a sustained
	// regression. Map-based — at most a few dozen entries even for
	// a heavy deployment, so no LRU needed.
	cooldownLock sync.Mutex
	cooldown     map[string]time.Time
}

// EffectiveCostConfig holds the alert tunables. Zero-valued fields
// fall to defaults at construction.
type EffectiveCostConfig struct {
	// Enabled gates the periodic scan. Default true once wired.
	Enabled bool
	// Interval is the gap between scans. Default 1h.
	Interval time.Duration
	// CurrentWindow is the "is something off right now" window.
	// Default 24h.
	CurrentWindow time.Duration
	// BaselineWindow is the "what's normal" window. Default 7d.
	// Should be a multiple of CurrentWindow so the ratio is
	// meaningful (a 24h current vs 24h baseline collapses to
	// "did anything change between yesterday and today" which
	// is too noisy).
	BaselineWindow time.Duration
	// RatioThreshold is the multiplier above which an alert fires.
	// Default 2.0. Operators raise it for noisy projects.
	RatioThreshold float64
	// MinCurrentSpendUSD suppresses alerts when the 24h spend on
	// this combo is below this floor — single small tasks aren't
	// a signal. Default $0.10.
	MinCurrentSpendUSD float64
	// MinBaselineOks suppresses alerts when the baseline window
	// has fewer than this many successes — too little signal to
	// trust the ratio. Default 5.
	MinBaselineOks int64
	// Cooldown is how long after firing an alert for a (role,
	// model) before that combo can fire again. Default 12h.
	Cooldown time.Duration
}

// DefaultEffectiveCostConfig returns a config that's safe to ship
// dark: enabled, 1h tick, 24h/7d windows, 2× threshold, 12h
// cooldown. All numbers picked to catch real regressions on a
// modest production deployment without paging the operator
// every other day.
func DefaultEffectiveCostConfig() EffectiveCostConfig {
	return EffectiveCostConfig{
		Enabled:            true,
		Interval:           time.Hour,
		CurrentWindow:      24 * time.Hour,
		BaselineWindow:     7 * 24 * time.Hour,
		RatioThreshold:     2.0,
		MinCurrentSpendUSD: 0.10,
		MinBaselineOks:     5,
		Cooldown:           12 * time.Hour,
	}
}

// NewEffectiveCostMonitor constructs the monitor. Returns nil when
// either repo is nil (without spend or outcome data, the alert is
// uncomputable and the daemon should run cleanly without it).
// notifier nil is allowed — the loop still runs and logs, just no
// outbound message.
func NewEffectiveCostMonitor(cfg EffectiveCostConfig, spend SpendRepo, outcomes OutcomeRepo, notifier EffectiveCostNotifier, logger zerolog.Logger) *EffectiveCostMonitor {
	if spend == nil || outcomes == nil {
		return nil
	}
	def := DefaultEffectiveCostConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = def.Interval
	}
	if cfg.CurrentWindow <= 0 {
		cfg.CurrentWindow = def.CurrentWindow
	}
	if cfg.BaselineWindow <= 0 {
		cfg.BaselineWindow = def.BaselineWindow
	}
	if cfg.RatioThreshold <= 0 {
		cfg.RatioThreshold = def.RatioThreshold
	}
	if cfg.MinCurrentSpendUSD <= 0 {
		cfg.MinCurrentSpendUSD = def.MinCurrentSpendUSD
	}
	if cfg.MinBaselineOks <= 0 {
		cfg.MinBaselineOks = def.MinBaselineOks
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = def.Cooldown
	}
	return &EffectiveCostMonitor{
		cfg:         cfg,
		spendRepo:   spend,
		outcomeRepo: outcomes,
		notifier:    notifier,
		logger:      logger,
		now:         func() time.Time { return time.Now().UTC() },
		cooldown:    make(map[string]time.Time),
	}
}

// SetNotifier attaches a notifier after construction. Useful when
// the daemon builds the monitor before the Telegram bot exists,
// then plugs the bot in once it's been initialized — same pattern
// as Executor.SetCompletionNotifier.
func (m *EffectiveCostMonitor) SetNotifier(n EffectiveCostNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifier = n
}

// Start begins the periodic scan. No-op when Enabled=false; calling
// Start twice returns an error. Mirrors watchdog.Watchdog so the
// daemon can wire both with the same lifecycle pattern.
func (m *EffectiveCostMonitor) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Enabled {
		m.logger.Info().Msg("effective-cost monitor disabled by config — drift scan will not run")
		return nil
	}
	if m.started {
		return fmt.Errorf("effective-cost monitor already started")
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.started = true
	m.wg.Add(1)
	go m.runLoop()
	m.logger.Info().
		Dur("interval", m.cfg.Interval).
		Dur("current_window", m.cfg.CurrentWindow).
		Dur("baseline_window", m.cfg.BaselineWindow).
		Float64("ratio_threshold", m.cfg.RatioThreshold).
		Msg("effective-cost monitor started")
	return nil
}

// Stop signals the loop and waits for drain. Safe to call multiple times.
func (m *EffectiveCostMonitor) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.cancel()
	m.started = false
	m.mu.Unlock()
	m.wg.Wait()
	return nil
}

func (m *EffectiveCostMonitor) runLoop() {
	defer m.wg.Done()
	// Skip the immediate first-scan that the watchdog does — alerts
	// fire on 24h windows; running them in the first second of
	// daemon life would just see whatever happened to be in the
	// window and produce a flurry of noise on every restart.
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.scanOnce()
		}
	}
}

// ScanOnce performs one drift evaluation across all (role, model)
// combos that have activity in the current window. Exported for
// tests; production drives it via the runLoop ticker.
func (m *EffectiveCostMonitor) ScanOnce() { m.scanOnce() }

func (m *EffectiveCostMonitor) scanOnce() {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	now := m.now()
	rows, err := ComputeDrift(ctx, m.spendRepo, m.outcomeRepo, "", DriftConfig{
		CurrentWindow:      m.cfg.CurrentWindow,
		BaselineWindow:     m.cfg.BaselineWindow,
		MinCurrentSpendUSD: m.cfg.MinCurrentSpendUSD,
		MinBaselineOks:     m.cfg.MinBaselineOks,
	}, now)
	if err != nil {
		m.logger.Warn().Err(err).Msg("effective-cost: drift query failed")
		return
	}

	for _, row := range rows {
		if !row.HasBaseline {
			continue
		}
		if row.Ratio < m.cfg.RatioThreshold {
			continue
		}
		if !m.shouldFire(row.Role, row.Model, now) {
			continue
		}
		alert := EffectiveCostAlert{
			Role:                    row.Role,
			Model:                   row.Model,
			Current24hUSDPerSuccess: row.CurrentUSDPerOk,
			Baseline7dUSDPerSuccess: row.BaselineUSDPerOk,
			Ratio:                   roundTo2(row.Ratio),
			Spend24hUSD:             row.CurrentSpendUSD,
			Successes24h:            row.CurrentOks,
			FiredAt:                 now,
		}
		// Log only — notifier wiring removed in 2026-05-02.
		// Operators read drift from /ui/spend's leaderboard
		// (drift column) and the per-project drift panel on
		// /ui/projects/{id}. The log line stays as a forensic
		// trail for post-hoc investigation.
		m.logger.Warn().
			Str("role", alert.Role).
			Str("model", alert.Model).
			Float64("current_usd_per_success", alert.Current24hUSDPerSuccess).
			Float64("baseline_usd_per_success", alert.Baseline7dUSDPerSuccess).
			Float64("ratio", alert.Ratio).
			Float64("spend_24h_usd", alert.Spend24hUSD).
			Int64("successes_24h", alert.Successes24h).
			Msg("effective-cost: drift detected (UI surfaces it on /ui/spend + /ui/projects)")
		if m.notifier != nil {
			m.notifier.NotifyEffectiveCostDrift(ctx, alert)
		}
		m.markFired(row.Role, row.Model, now)
	}
}

// shouldFire reports whether enough time has passed since the last
// alert on this (role, model) combo for a fresh alert to be
// allowed. Cooldown prevents log + notification floods during a
// sustained regression.
func (m *EffectiveCostMonitor) shouldFire(role, model string, now time.Time) bool {
	m.cooldownLock.Lock()
	defer m.cooldownLock.Unlock()
	last, ok := m.cooldown[role+"|"+model]
	if !ok {
		return true
	}
	return now.Sub(last) >= m.cfg.Cooldown
}

func (m *EffectiveCostMonitor) markFired(role, model string, now time.Time) {
	m.cooldownLock.Lock()
	defer m.cooldownLock.Unlock()
	m.cooldown[role+"|"+model] = now
}

// roundTo2 rounds a float to two decimal places. Used so the alert's
// Ratio is presentation-ready ("2.43" not "2.4321987...").
//
// Pre-2026-05-29 used `int(f*100+0.5)` which truncates toward zero
// — correct for positive ratios (the only realistic input here),
// wrong for negative values where it produced -249 from -2.495
// instead of -250. math.Round handles both signs correctly.
func roundTo2(f float64) float64 {
	return math.Round(f*100) / 100.0
}

// Compile-time assertion that the production usage repo satisfies
// SpendRepo. Mirrors the HistoryRepo assertion in forecast.go.
var _ SpendRepo = (persistence.TaskLLMUsageRepository)(nil)
var _ OutcomeRepo = (persistence.ExecutionStepOutcomeRepository)(nil)
