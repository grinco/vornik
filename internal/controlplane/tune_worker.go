// Package controlplane holds the vornik control-plane's server-side workers
// (LLD 2026-07-07-control-plane-design). Phase 1 ships the Tune detector: a
// leader-gated periodic scan that watches per-project health signals and, on
// a sustained breach, writes a DRAFT proposal to the ledger for a human to
// review. It NEVER mutates config — proposing is the only action.
package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// LeaderGate gates the tick to the elected leader so two daemons don't
// double-propose. Nil-safe at the worker.
type LeaderGate interface {
	IsLeader() bool
}

// RateSample is one project's failed-task rate over the scan window.
type RateSample struct {
	Failed int
	Total  int
	Rate   float64 // Failed / Total; 0 when Total == 0
}

// LatencySample is one project's execution-latency p95 (seconds) over the
// window, with the sample count it was computed from.
type LatencySample struct {
	P95Seconds float64
	Count      int
}

// MetricsSource supplies the per-project signals the Tune worker watches.
// Phase 1 tracks two concrete signals computed from the executions table
// (failed-task rate + latency p95); an empty-completion signal comes with the
// metric gap-analysis in a later phase.
type MetricsSource interface {
	// FailedTaskRates returns, per project id, the failed-task rate over the
	// worker's window.
	FailedTaskRates(ctx context.Context) (map[string]RateSample, error)
	// LatencyP95s returns, per project id, the execution-latency p95 over the
	// worker's window.
	LatencyP95s(ctx context.Context) (map[string]LatencySample, error)
}

// TuneWorker is the leader-gated failed-rate detector.
type TuneWorker struct {
	Proposals persistence.ProposalRepository
	Metrics   MetricsSource
	Interval  time.Duration

	// Threshold is the failed-rate at/above which a project is "breaching"
	// (default 0.5). LatencyThresholdSeconds is the execution p95 at/above
	// which the latency signal breaches (default 300s). MinSamples avoids
	// proposing off tiny samples (default 5). BreachesToPropose is how many
	// CONSECUTIVE breaching ticks before a proposal is raised (default 3), so
	// a single bad window doesn't fire.
	Threshold               float64
	LatencyThresholdSeconds float64
	MinSamples              int
	BreachesToPropose       int

	LeaderGate LeaderGate
	Logger     zerolog.Logger

	// per-signal consecutive-breach counters, keyed by project.
	failedBreaches  map[string]int
	latencyBreaches map[string]int
	stopped         chan struct{}
}

func (w *TuneWorker) latencyThreshold() float64 {
	if w.LatencyThresholdSeconds > 0 {
		return w.LatencyThresholdSeconds
	}
	return 300
}

func (w *TuneWorker) threshold() float64 {
	if w.Threshold > 0 {
		return w.Threshold
	}
	return 0.5
}

func (w *TuneWorker) minSamples() int {
	if w.MinSamples > 0 {
		return w.MinSamples
	}
	return 5
}

func (w *TuneWorker) breachesToPropose() int {
	if w.BreachesToPropose > 0 {
		return w.BreachesToPropose
	}
	return 3
}

// Run drives the periodic loop until ctx is cancelled or the worker is
// structurally disabled (nil deps or non-positive Interval).
func (w *TuneWorker) Run(ctx context.Context) {
	if w == nil || w.Proposals == nil || w.Metrics == nil {
		return
	}
	if w.Interval <= 0 {
		w.Logger.Debug().Msg("tune worker disabled by config")
		return
	}
	if w.failedBreaches == nil {
		w.failedBreaches = map[string]int{}
	}
	if w.latencyBreaches == nil {
		w.latencyBreaches = map[string]int{}
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)
	w.Logger.Info().Dur("interval", w.Interval).Msg("control-plane tune worker started")
	defer w.Logger.Info().Msg("control-plane tune worker stopped")

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	if w.LeaderGate == nil || w.LeaderGate.IsLeader() {
		w.tick(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
				continue
			}
			w.tick(ctx)
		}
	}
}

// Stopped returns a channel closed when Run exits (test sync).
func (w *TuneWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

// tick reads each signal and proposes for any project that has been breaching
// for BreachesToPropose consecutive ticks.
func (w *TuneWorker) tick(ctx context.Context) {
	w.scanFailedRate(ctx)
	w.scanLatency(ctx)
}

func (w *TuneWorker) scanFailedRate(ctx context.Context) {
	rates, err := w.Metrics.FailedTaskRates(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read failed-rate metrics")
		return
	}
	seen := map[string]bool{}
	for project, s := range rates {
		seen[project] = true
		breaching := s.Total >= w.minSamples() && s.Rate >= w.threshold()
		if !w.advanceStreak(w.failedBreaches, project, breaching) {
			continue
		}
		w.propose(ctx, project, tuneFailedRateTitle(project),
			fmt.Sprintf("Failed-task rate %.0f%% (%d/%d) over the scan window, sustained for %d consecutive scans — above the %.0f%% threshold. Investigate the failing step (logs/traces) or consider a model/timeout change.",
				s.Rate*100, s.Failed, s.Total, w.breachesToPropose(), w.threshold()*100),
			fmt.Sprintf(`{"signal":"failed_task_rate","rate":%.3f,"failed":%d,"total":%d}`, s.Rate, s.Failed, s.Total),
		)
	}
	w.resetAbsent(w.failedBreaches, seen)
}

func (w *TuneWorker) scanLatency(ctx context.Context) {
	lats, err := w.Metrics.LatencyP95s(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read latency metrics")
		return
	}
	seen := map[string]bool{}
	for project, s := range lats {
		seen[project] = true
		breaching := s.Count >= w.minSamples() && s.P95Seconds >= w.latencyThreshold()
		if !w.advanceStreak(w.latencyBreaches, project, breaching) {
			continue
		}
		w.propose(ctx, project, tuneLatencyTitle(project),
			fmt.Sprintf("Execution p95 latency %.0fs (n=%d) over the scan window, sustained for %d consecutive scans — above the %.0fs threshold. Investigate slow steps/tools or consider a faster model or a step-timeout change.",
				s.P95Seconds, s.Count, w.breachesToPropose(), w.latencyThreshold()),
			fmt.Sprintf(`{"signal":"latency_p95_seconds","p95":%.1f,"count":%d}`, s.P95Seconds, s.Count),
		)
	}
	w.resetAbsent(w.latencyBreaches, seen)
}

// advanceStreak increments project's consecutive-breach counter and reports
// whether it just reached the propose threshold (resetting it so we don't
// re-propose every subsequent tick). A non-breaching tick resets the streak.
func (w *TuneWorker) advanceStreak(counters map[string]int, project string, breaching bool) bool {
	if !breaching {
		delete(counters, project)
		return false
	}
	counters[project]++
	if counters[project] >= w.breachesToPropose() {
		delete(counters, project)
		return true
	}
	return false
}

// resetAbsent clears streaks for projects that dropped out of this tick's
// sample entirely (no data == not breaching).
func (w *TuneWorker) resetAbsent(counters map[string]int, seen map[string]bool) {
	for p := range counters {
		if !seen[p] {
			delete(counters, p)
		}
	}
}

// propose writes a DRAFT proposal unless an open (DRAFT) one with the same
// title already exists for this project (dedup so the ledger doesn't fill
// with duplicates while the operator hasn't decided yet).
func (w *TuneWorker) propose(ctx context.Context, project, title, rationale, evidence string) {
	existing, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		ProjectID: project, Statuses: []string{persistence.ProposalStatusDraft},
	})
	if err == nil {
		for _, p := range existing {
			if p.Title == title {
				return
			}
		}
	}
	p := &persistence.ControlPlaneProposal{
		ID:          persistence.GenerateID("cpp"),
		ProjectID:   project,
		Kind:        persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject,
		Title:       title,
		Rationale:   rationale,
		Evidence:    evidence,
		Status:      persistence.ProposalStatusDraft,
		// The proposer is the detector, never a human — so a human approving
		// it always satisfies the no-self-approval gate.
		ProposedBy: "tune-detector",
	}
	if err := w.Proposals.Create(ctx, p); err != nil {
		w.Logger.Warn().Err(err).Str("project", project).Msg("tune: failed to create proposal")
		return
	}
	w.Logger.Info().Str("project", project).Str("proposal_id", p.ID).Str("title", title).
		Msg("tune: raised DRAFT proposal")
}

func tuneFailedRateTitle(project string) string {
	return "Tune: high failed-task rate on " + project
}

func tuneLatencyTitle(project string) string {
	return "Tune: high p95 latency on " + project
}
