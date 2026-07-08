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

// MetricsSource supplies the per-project signals the Tune worker watches.
// Phase 1 tracks one concrete signal (failed-task rate) computed from the
// executions table; more signals (latency p95, empty-completion) come with
// the metric gap-analysis in a later phase.
type MetricsSource interface {
	// FailedTaskRates returns, per project id, the failed-task rate over the
	// worker's window.
	FailedTaskRates(ctx context.Context) (map[string]RateSample, error)
}

// TuneWorker is the leader-gated failed-rate detector.
type TuneWorker struct {
	Proposals persistence.ProposalRepository
	Metrics   MetricsSource
	Interval  time.Duration

	// Threshold is the failed-rate at/above which a project is "breaching"
	// (default 0.5). MinSamples avoids proposing off tiny samples (default 5).
	// BreachesToPropose is how many CONSECUTIVE breaching ticks before a
	// proposal is raised (default 3), so a single bad window doesn't fire.
	Threshold         float64
	MinSamples        int
	BreachesToPropose int

	LeaderGate LeaderGate
	Logger     zerolog.Logger

	// breaches counts consecutive breaching ticks per project.
	breaches map[string]int
	stopped  chan struct{}
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
	if w.breaches == nil {
		w.breaches = map[string]int{}
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

// tick reads the signals and proposes for any project that has been breaching
// for BreachesToPropose consecutive ticks.
func (w *TuneWorker) tick(ctx context.Context) {
	rates, err := w.Metrics.FailedTaskRates(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read metrics")
		return
	}
	seen := map[string]bool{}
	for project, s := range rates {
		seen[project] = true
		breaching := s.Total >= w.minSamples() && s.Rate >= w.threshold()
		if !breaching {
			delete(w.breaches, project)
			continue
		}
		w.breaches[project]++
		if w.breaches[project] < w.breachesToPropose() {
			continue
		}
		w.maybePropose(ctx, project, s)
		// Reset so we don't re-propose every subsequent tick; a fresh
		// sustained breach after a decision starts the count over.
		delete(w.breaches, project)
	}
	// Projects that dropped out of the sample this tick reset.
	for p := range w.breaches {
		if !seen[p] {
			delete(w.breaches, p)
		}
	}
}

// maybePropose writes a DRAFT proposal unless an open (DRAFT) one already
// exists for this project's failed-rate signal (dedup so the ledger doesn't
// fill with duplicates while the operator hasn't decided yet).
func (w *TuneWorker) maybePropose(ctx context.Context, project string, s RateSample) {
	title := tuneFailedRateTitle(project)
	existing, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		ProjectID: project, Statuses: []string{persistence.ProposalStatusDraft},
	})
	if err == nil {
		for _, p := range existing {
			if p.Title == title {
				return // already an open proposal for this signal
			}
		}
	}
	p := &persistence.ControlPlaneProposal{
		ID:          persistence.GenerateID("cpp"),
		ProjectID:   project,
		Kind:        persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject,
		Title:       title,
		Rationale: fmt.Sprintf(
			"Failed-task rate %.0f%% (%d/%d) over the scan window, sustained for %d consecutive scans — above the %.0f%% threshold. Investigate the failing step (logs/traces) or consider a model/timeout change.",
			s.Rate*100, s.Failed, s.Total, w.breachesToPropose(), w.threshold()*100),
		Evidence: fmt.Sprintf(`{"signal":"failed_task_rate","rate":%.3f,"failed":%d,"total":%d}`, s.Rate, s.Failed, s.Total),
		Status:   persistence.ProposalStatusDraft,
		// The proposer is the detector, never a human — so a human approving
		// it always satisfies the no-self-approval gate.
		ProposedBy: "tune-detector",
	}
	if err := w.Proposals.Create(ctx, p); err != nil {
		w.Logger.Warn().Err(err).Str("project", project).Msg("tune: failed to create proposal")
		return
	}
	w.Logger.Info().Str("project", project).Str("proposal_id", p.ID).
		Float64("failed_rate", s.Rate).Msg("tune: raised DRAFT proposal for high failed-task rate")
}

func tuneFailedRateTitle(project string) string {
	return "Tune: high failed-task rate on " + project
}
