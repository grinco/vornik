// Package controlplane holds the vornik control-plane's server-side workers
// (LLD 2026-07-07-control-plane-design). Phase 1 ships the Tune detector: a
// leader-gated periodic scan that watches per-project health signals and, on
// a sustained breach, writes a DRAFT proposal to the ledger for a human to
// review. It NEVER mutates config — proposing is the only action.
package controlplane

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// LeaderGate gates the tick to the elected leader so two daemons don't
// double-propose. Nil-safe at the worker.
type LeaderGate interface {
	IsLeader() bool
}

// reservedProposers are ProposedBy identities that ONLY in-daemon code may
// stamp (the control-plane workers + the operator-UI hub). An external caller
// (an agent hitting the operator-propose API) must not be able to masquerade as
// one of these and thereby satisfy the human-approval gate — see IsReservedProposer.
var reservedProposers = map[string]bool{
	"operator-ui":   true,
	"tune-detector": true,
	"instinct":      true,
	"diagnose":      true,
	"self-heal":     true,
}

// IsReservedProposer reports whether a ProposedBy value is a reserved
// system principal that only in-daemon code may set. The operator-propose
// API/CLI rejects a client-supplied reserved value (hardening from the
// control-plane hub review: a compromised agent must not forge "operator-ui"
// to self-approve).
func IsReservedProposer(proposedBy string) bool {
	return reservedProposers[proposedBy]
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

// ProjectToolKey identifies a (project, tool) instance for the operational-
// instinct tool-timeout signal (Phase 3). Used directly as a Go map key — no
// delimiter-joined strings (review-hardened: no collision).
type ProjectToolKey struct {
	Project string
	Tool    string
}

// ToolLatencySample is one (project, tool)'s tool-call p95 (seconds) + sample
// count over the window (operational-instinct tool-timeout signal).
type ToolLatencySample struct {
	Key        ProjectToolKey
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
	// ToolLatencies returns per-(project, tool) tool-call p95 over the window
	// — the operational-instinct tool-timeout signal (Phase 3). An empty slice
	// (e.g. the query is unavailable) simply means the instinct never fires.
	ToolLatencies(ctx context.Context) ([]ToolLatencySample, error)
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

	// ToolLatencyThresholdSeconds is the tool-call p95 (seconds) at/above which
	// the operational-instinct tool-timeout signal breaches (default 60).
	// MaxSuggestedTimeoutSeconds caps the suggested new timeout so a transient
	// p95 spike can't propose an absurd value (default 300). Either signal is
	// disabled by setting its threshold < 0.
	ToolLatencyThresholdSeconds float64
	MaxSuggestedTimeoutSeconds  float64

	// SkipFailedRate makes scanFailedRate a no-op — set at construction when
	// the SelfHealWorker owns the failed-rate signal (self-healing design §5).
	// Unconditional, no per-tick handshake; false = today's behaviour.
	SkipFailedRate bool

	LeaderGate LeaderGate
	Logger     zerolog.Logger

	// per-signal consecutive-breach counters. failed/latency are keyed by
	// project; tool by the (project, tool) composite (typed, no string join).
	failedBreaches  map[string]int
	latencyBreaches map[string]int
	toolBreaches    map[ProjectToolKey]int
	stopped         chan struct{}
}

func (w *TuneWorker) toolLatencyThreshold() float64 {
	if w.ToolLatencyThresholdSeconds != 0 {
		return w.ToolLatencyThresholdSeconds
	}
	return 60
}

func (w *TuneWorker) maxSuggestedTimeout() float64 {
	if w.MaxSuggestedTimeoutSeconds > 0 {
		return w.MaxSuggestedTimeoutSeconds
	}
	return 300
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
	if w.toolBreaches == nil {
		w.toolBreaches = map[ProjectToolKey]int{}
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

// tick reads each signal and proposes for any instance that has been breaching
// for BreachesToPropose consecutive ticks.
func (w *TuneWorker) tick(ctx context.Context) {
	w.scanFailedRate(ctx)
	w.scanLatency(ctx)
	w.scanToolLatency(ctx)
}

func (w *TuneWorker) scanFailedRate(ctx context.Context) {
	if w.SkipFailedRate {
		return // the SelfHealWorker owns the failed-rate signal
	}
	rates, err := w.Metrics.FailedTaskRates(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read failed-rate metrics")
		return
	}
	seen := map[string]bool{}
	for project, s := range rates {
		seen[project] = true
		breaching := s.Total >= w.minSamples() && s.Rate >= w.threshold()
		if !advanceStreak(w.failedBreaches, project, breaching, w.breachesToPropose()) {
			continue
		}
		FileFailedRateProposal(ctx, w.Proposals, w.Logger, project, s, w.breachesToPropose(), w.threshold())
	}
	resetAbsent(w.failedBreaches, seen)
}

// FileFailedRateProposal writes the generic review-only failed-rate DRAFT
// proposal (ProposedBy="tune-detector"), deduped on the open-DRAFT title. It is
// the SINGLE source of the generic failed-rate proposal — called by the Tune
// worker's scan AND by the SelfHealWorker when its diagnosis is unavailable
// (self-healing design §4.5), so the logic is never duplicated.
func FileFailedRateProposal(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project string, s RateSample, wantStreak int, threshold float64) {
	title := tuneFailedRateTitle(project)
	rationale := fmt.Sprintf("Failed-task rate %.0f%% (%d/%d) over the scan window, sustained for %d consecutive scans — above the %.0f%% threshold. Investigate the failing step (logs/traces) or consider a model/timeout change.",
		s.Rate*100, s.Failed, s.Total, wantStreak, threshold*100)
	evidence := fmt.Sprintf(`{"signal":"failed_task_rate","rate":%.3f,"failed":%d,"total":%d}`, s.Rate, s.Failed, s.Total)
	fileProposal(ctx, proposals, logger, project, title, rationale, evidence, "tune-detector")
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
		if !advanceStreak(w.latencyBreaches, project, breaching, w.breachesToPropose()) {
			continue
		}
		w.propose(ctx, project, tuneLatencyTitle(project),
			fmt.Sprintf("Execution p95 latency %.0fs (n=%d) over the scan window, sustained for %d consecutive scans — above the %.0fs threshold. Investigate slow steps/tools or consider a faster model or a step-timeout change.",
				s.P95Seconds, s.Count, w.breachesToPropose(), w.latencyThreshold()),
			fmt.Sprintf(`{"signal":"latency_p95_seconds","p95":%.1f,"count":%d}`, s.P95Seconds, s.Count),
			"tune-detector",
		)
	}
	resetAbsent(w.latencyBreaches, seen)
}

// scanToolLatency is the operational-instinct tool-timeout signal (Phase 3):
// per (project, tool) tool-call p95 above the threshold, sustained, proposes a
// review-only timeout bump. Disabled when ToolLatencyThresholdSeconds < 0.
func (w *TuneWorker) scanToolLatency(ctx context.Context) {
	if w.ToolLatencyThresholdSeconds < 0 {
		return // signal disabled by config
	}
	samples, err := w.Metrics.ToolLatencies(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read tool-latency metrics")
		return
	}
	threshold := w.toolLatencyThreshold()
	seen := map[ProjectToolKey]bool{}
	for _, s := range samples {
		seen[s.Key] = true
		breaching := s.Count >= w.minSamples() && s.P95Seconds >= threshold
		if !advanceStreak(w.toolBreaches, s.Key, breaching, w.breachesToPropose()) {
			continue
		}
		suggested := math.Ceil(s.P95Seconds * 1.5)
		clamped := ""
		if maxT := w.maxSuggestedTimeout(); suggested > maxT {
			suggested = maxT
			clamped = fmt.Sprintf(" (clamped to the %0.fs cap; the raw p95 suggests a larger bump — raise it by hand if you truly need more)", maxT)
		}
		w.propose(ctx, s.Key.Project, instinctToolTimeoutTitle(s.Key),
			fmt.Sprintf("Tool %q p95 call latency is %.0fs (n=%d) in project %s, sustained for %d consecutive scans — above the %.0fs threshold. Consider raising this tool's timeout to ~%.0fs%s, or investigate why the tool is slow.",
				s.Key.Tool, s.P95Seconds, s.Count, s.Key.Project, w.breachesToPropose(), threshold, suggested, clamped),
			fmt.Sprintf(`{"signal":"tool_latency_p95_seconds","tool":%q,"p95":%.1f,"count":%d,"suggested_timeout_s":%.0f}`, s.Key.Tool, s.P95Seconds, s.Count, suggested),
			"instinct",
		)
	}
	resetAbsent(w.toolBreaches, seen)
}

// advanceStreak increments key's consecutive-breach counter and reports whether
// it just reached wantStreak (resetting it so we don't re-propose every tick).
// A non-breaching tick (below threshold OR below MinSamples — the caller folds
// both into `breaching`) resets the streak. Generic over the key type so the
// same hysteresis serves project-keyed and (project,tool)-keyed signals.
func advanceStreak[K comparable](counters map[K]int, key K, breaching bool, wantStreak int) bool {
	if !breaching {
		delete(counters, key)
		return false
	}
	counters[key]++
	if counters[key] >= wantStreak {
		delete(counters, key)
		return true
	}
	return false
}

// resetAbsent clears streaks for keys that dropped out of this tick's sample
// entirely (no data == not breaching).
func resetAbsent[K comparable](counters map[K]int, seen map[K]bool) {
	for k := range counters {
		if !seen[k] {
			delete(counters, k)
		}
	}
}

// propose writes a DRAFT proposal unless an open (DRAFT) one with the same
// title already exists for this project (dedup so the ledger doesn't fill with
// duplicates while the operator hasn't decided yet). proposedBy tags the source
// ("tune-detector" for the metric signals, "instinct" for operational
// instincts) — never a human, so a human approving always satisfies the
// no-self-approval gate.
func (w *TuneWorker) propose(ctx context.Context, project, title, rationale, evidence, proposedBy string) {
	fileProposal(ctx, w.Proposals, w.Logger, project, title, rationale, evidence, proposedBy)
}

// fileProposal writes a project-scoped review-only DRAFT proposal, deduped on
// the open-DRAFT title. Shared by the Tune worker, the instinct scans, and the
// self-heal generic fallback.
func fileProposal(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project, title, rationale, evidence, proposedBy string) {
	existing, err := proposals.List(ctx, persistence.ProposalListFilter{
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
		ProposedBy:  proposedBy,
	}
	if err := proposals.Create(ctx, p); err != nil {
		logger.Warn().Err(err).Str("project", project).Msg("control-plane: failed to create proposal")
		return
	}
	logger.Info().Str("project", project).Str("proposal_id", p.ID).Str("title", title).Str("by", proposedBy).
		Msg("control-plane: raised DRAFT proposal")
}

func tuneFailedRateTitle(project string) string {
	return "Tune: high failed-task rate on " + project
}

func tuneLatencyTitle(project string) string {
	return "Tune: high p95 latency on " + project
}

func instinctToolTimeoutTitle(k ProjectToolKey) string {
	return fmt.Sprintf("Instinct: %s timeouts in %s", k.Tool, k.Project)
}
