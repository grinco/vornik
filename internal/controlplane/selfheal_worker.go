package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Self-healing incident detection (LLD 2026-07-08-control-plane-self-healing-
// design v2, review green). A leader-gated periodic scan: on a project's
// sustained failed-rate breach it auto-invokes the Diagnoser and files a
// review-only diagnosis proposal (the "incident"), then pushes one operator
// alert. Loop-safe: edge-triggered (advanceStreak), per-project open-DRAFT
// dedup, a global per-hour rate cap, and a never-diagnose SystemProjectID
// exclusion. Nothing auto-applies. When enabled it OWNS the failed-rate signal
// (the Tune worker's SkipFailedRate is set), and guarantees coverage: the
// specific diagnosis normally, the generic FileFailedRateProposal on a
// diagnoser error.

// IncidentDiagnoser is the single-shot diagnosis the self-healer triggers —
// satisfied by *Diagnoser; an interface for test fakes.
type IncidentDiagnoser interface {
	Diagnose(ctx context.Context, focus string, propose bool) (*DiagnoseVerdict, string, error)
}

// SelfHealWorker is the leader-gated failed-rate → auto-diagnosis escalator.
type SelfHealWorker struct {
	Proposals persistence.ProposalRepository
	Metrics   MetricsSource
	Diagnose  IncidentDiagnoser
	// Alert pushes one operator alert per opened incident (subject, body).
	// Nil-safe: no notifier configured → incident still filed, no alert.
	Alert    func(subject, body string)
	Interval time.Duration

	// Enabled is the per-tick opt-in gate (control_plane.self_heal_enabled,
	// read live so a config reload is an immediate emergency brake —
	// actionable-proposals §7). Nil = enabled. While it returns false the
	// worker skips its scan entirely (breach streaks reset) and the Tune
	// worker's mirrored closure files the generic failed-rate proposal.
	Enabled func() bool

	Threshold           float64 // failed-rate breach (default 0.5)
	MinSamples          int     // default 5
	BreachesToOpen      int     // consecutive breaching scans before opening (default 3)
	MaxIncidentsPerHour int     // GLOBAL rate cap (default 3)
	SystemProjectID     string  // never self-diagnosed ("" = no exclusion)

	LeaderGate LeaderGate
	Logger     zerolog.Logger

	breaches map[string]int // per-project consecutive-breach streak
	opened   []time.Time    // rolling open timestamps for the global rate cap
	stopped  chan struct{}
}

func (w *SelfHealWorker) threshold() float64 {
	if w.Threshold > 0 {
		return w.Threshold
	}
	return 0.5
}

func (w *SelfHealWorker) minSamples() int {
	if w.MinSamples > 0 {
		return w.MinSamples
	}
	return 5
}

func (w *SelfHealWorker) breachesToOpen() int {
	if w.BreachesToOpen > 0 {
		return w.BreachesToOpen
	}
	return 3
}

func (w *SelfHealWorker) maxIncidentsPerHour() int {
	if w.MaxIncidentsPerHour > 0 {
		return w.MaxIncidentsPerHour
	}
	return 3
}

// Run drives the periodic loop until ctx is cancelled or the worker is
// structurally disabled (nil deps or non-positive Interval).
func (w *SelfHealWorker) Run(ctx context.Context) {
	if w == nil || w.Proposals == nil || w.Metrics == nil || w.Diagnose == nil {
		return
	}
	if w.Interval <= 0 {
		w.Logger.Debug().Msg("self-heal worker disabled by config")
		return
	}
	if w.breaches == nil {
		w.breaches = map[string]int{}
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)
	w.Logger.Info().Dur("interval", w.Interval).Msg("control-plane self-heal worker started")
	defer w.Logger.Info().Msg("control-plane self-heal worker stopped")

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
func (w *SelfHealWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

func (w *SelfHealWorker) tick(ctx context.Context) {
	if w.Enabled != nil && !w.Enabled() {
		// Hot-disabled: drop accumulated streaks so a later re-enable
		// re-arms from scratch instead of firing off stale counts.
		for k := range w.breaches {
			delete(w.breaches, k)
		}
		return
	}
	rates, err := w.Metrics.FailedTaskRates(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("self-heal: failed to read failed-rate metrics")
		return
	}
	seen := map[string]bool{}
	for project, s := range rates {
		seen[project] = true
		breaching := s.Total >= w.minSamples() && s.Rate >= w.threshold()
		if !advanceStreak(w.breaches, project, breaching, w.breachesToOpen()) {
			continue
		}
		w.open(ctx, project, s)
	}
	resetAbsent(w.breaches, seen)
}

// open attempts to open an incident for a breaching project: skip the system
// project (loop-safety), skip if an open self-heal DRAFT already exists (dedup),
// enforce the global rate cap, then run the diagnoser. On a diagnoser error it
// files the generic failed-rate proposal instead (guaranteed coverage).
func (w *SelfHealWorker) open(ctx context.Context, project string, s RateSample) {
	if w.SystemProjectID != "" && project == w.SystemProjectID {
		return // never self-diagnose the control plane's own project
	}
	if w.hasOpenIncident(ctx, project) {
		return // dedup: one open incident per project until decided
	}
	if !w.underRateCap() {
		w.Logger.Warn().Str("project", project).Msg("self-heal: rate-capped; incident not opened")
		if w.Alert != nil {
			w.Alert("vornik self-heal rate-capped", fmt.Sprintf("Project %s is breaching but the self-heal per-hour cap (%d) is reached; open incidents by hand if needed.", project, w.maxIncidentsPerHour()))
		}
		return
	}
	verdict, proposalID, err := w.Diagnose.Diagnose(ctx, project, true)
	if err != nil {
		// Coverage guarantee: the operator still hears about the breach via the
		// generic failed-rate proposal (shared helper, no duplicated logic).
		w.Logger.Warn().Err(err).Str("project", project).Msg("self-heal: diagnosis failed; filing generic failed-rate proposal")
		FileFailedRateProposal(ctx, w.Proposals, w.Logger, project, s, w.breachesToOpen(), w.threshold())
		return
	}
	w.recordOpen()
	rootCause := "diagnosis filed"
	if verdict != nil && verdict.RootCause != "" {
		rootCause = verdict.RootCause
	}
	w.Logger.Info().Str("project", project).Str("proposal_id", proposalID).Msg("self-heal: opened incident (auto-diagnosis)")
	if w.Alert != nil {
		body := fmt.Sprintf("Project %s: %s.", project, rootCause)
		if proposalID != "" {
			body += " Review the proposed fix: control-plane proposal " + proposalID + "."
		}
		w.Alert("🩺 vornik self-heal opened an incident", body)
	}
}

// hasOpenIncident reports whether an open (DRAFT) self-heal proposal already
// exists for the project.
func (w *SelfHealWorker) hasOpenIncident(ctx context.Context, project string) bool {
	existing, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		ProjectID: project, Statuses: []string{persistence.ProposalStatusDraft},
	})
	if err != nil {
		// On a list error, err on the side of NOT opening (avoid duplicates).
		return true
	}
	for _, p := range existing {
		if p.ProposedBy == "self-heal" {
			return true
		}
	}
	return false
}

// underRateCap lazily evicts open-timestamps older than 1h and reports whether
// another incident may open this hour.
func (w *SelfHealWorker) underRateCap() bool {
	cutoff := time.Now().Add(-time.Hour)
	kept := w.opened[:0]
	for _, t := range w.opened {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.opened = kept
	return len(w.opened) < w.maxIncidentsPerHour()
}

func (w *SelfHealWorker) recordOpen() { w.opened = append(w.opened, time.Now()) }
