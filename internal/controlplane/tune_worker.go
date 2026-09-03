// Package controlplane holds the vornik control-plane's server-side workers
// (LLD 2026-07-07-control-plane-design). Phase 1 ships the Tune detector: a
// leader-gated periodic scan that watches per-project health signals and, on
// a sustained breach, writes a DRAFT proposal to the ledger for a human to
// review. It NEVER mutates config — proposing is the only action.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
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
	"instinct-lift": true,
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

// StepLatencySample is one (project, workflow, step, role, model)'s step
// p95 (seconds) + count over the window — the latency signal's slowest-step
// attribution (actionable-proposals design §4.4).
type StepLatencySample struct {
	Project    string
	Workflow   string
	Step       string
	Role       string
	Model      string
	P95Seconds float64
	Count      int
	// MaxSeconds is the slowest observed run in the window. The reclaim path
	// floors its suggestion here rather than at p95 (design §6.1 / D4): a
	// timeout's job is to not truncate the runs that actually happen, and the
	// max IS one of those runs. Zero when the metrics source predates this
	// field, in which case reclaimSuggestion degrades to the old p95 term.
	MaxSeconds float64
	// DegradedCount is how many runs in the window ended in a degraded outcome
	// (timeout / schema_violation / degenerate_loop). Any non-zero value
	// disqualifies a reclaim: a step that timed out is not over-provisioned,
	// and because a truncated run's recorded duration is CAPPED at the timeout
	// it also drags p95 down, making the step look more reclaimable the worse
	// it gets.
	DegradedCount int
	// TimeoutCount is the subset of DegradedCount that ended in TRUNCATION
	// (timeout / context_timeout). Only truncation may drive a timeout RAISE:
	// a schema_violation or degenerate_loop is not short of time, and reacting
	// to one with a raise widens the ceiling on every scan for a cause the
	// raise cannot address (review-20260810-53f0 finding 6). The reclaim guard
	// deliberately stays broader and refuses on ANY degradation.
	TimeoutCount int
}

// reclaimSuggestion returns the timeout a reclaim proposal should suggest for
// st, applying the standard 1.5 headroom multiplier to whichever of p95 and
// the observed max is larger (design §6.1).
func reclaimSuggestion(st StepLatencySample) time.Duration {
	basis := st.P95Seconds
	if st.MaxSeconds > basis {
		basis = st.MaxSeconds
	}
	return time.Duration(math.Ceil(basis*1.5)) * time.Second
}

// reclaimEligible reports whether st may be proposed for a timeout reduction
// against its current setting. Reductions are held to a higher evidence bar
// than raises because a reduction is the only direction that can cause
// truncation (design §6.1).
func reclaimEligible(st StepLatencySample, current time.Duration, ratio float64, minSamplesReduce int) bool {
	if st.DegradedCount > 0 {
		return false
	}
	if st.Count < minSamplesReduce {
		return false
	}
	return st.P95Seconds <= ratio*current.Seconds()
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
	// StepLatencies returns per-(project, workflow, step, role, model) step
	// p95 over the window — consulted only on a latency-breach proposing tick
	// to attribute the slowest step (actionable-proposals §4.4). An empty
	// slice means attribution is unavailable and the latency proposal stays
	// generic.
	StepLatencies(ctx context.Context) ([]StepLatencySample, error)
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

	// SkipFailedRate makes scanFailedRate a no-op when it returns true — set
	// when the SelfHealWorker owns the failed-rate signal (self-healing
	// design §5). A per-tick closure (not a construction-time boolean) so
	// flipping control_plane.self_heal_enabled + reload hands the signal
	// back without a daemon restart (actionable-proposals §7 emergency
	// brake). Nil = false = today's behaviour.
	SkipFailedRate func() bool

	// Diagnose escalates a sustained latency breach whose step TIMEOUT is not
	// the binding constraint — the "the model is slow" case, which no threshold
	// can render a fix for. Nil disables the escalation entirely and the
	// informational proposal remains the outcome, exactly as before.
	//
	// Called with propose=FALSE. The Diagnoser's own filing path generates a
	// title from the model's root-cause prose, and the applyable-upgrade
	// mechanism this design depends on matches on tuneLatencyTitle(project) —
	// so we take the verdict and file it ourselves under the stable title.
	// See https://docs.vornik.io D1/D4.
	Diagnose IncidentDiagnoser
	// MaxLatencyDiagnosesPerHour caps LLM spend on the escalation (default 3).
	// Counts ATTEMPTS, not successes: a failed call still cost money, so a
	// persistently broken LLM must not bill without bound.
	MaxLatencyDiagnosesPerHour int
	// SystemProjectID is never escalated — the daemon must not diagnose its own
	// project into a loop. "" disables the exclusion.
	SystemProjectID string

	// Actionize renders concrete applyable changes for the latency +
	// tool-timeout signals (actionable-proposals §4.4). Nil → every
	// proposal stays informational (prior behaviour).
	Actionize *Actionizer

	// TimeoutDegradedRateThreshold is the share of runs ending in a degraded
	// outcome at/above which a step counts as pinned against its timeout,
	// independent of p95 (design §6). 0 → 0.05. A RATE, not a count: one
	// timeout in a large clean window is noise.
	TimeoutDegradedRateThreshold float64

	// RejectCooldown suppresses re-filing a proposal whose TITLE an operator
	// has already REJECTED, measured from the most recent rejection (design
	// §7). 0 → 168h, mirroring instinct.lift.reeval_cooldown_hours.
	//
	// Without it a rejection suppresses nothing: fileRenderedProposal dedups
	// only against open DRAFTs, so the detector re-files on the next streak.
	// The 2026-08-10 ingest outage was preceded by three rejections of the
	// identical title.
	RejectCooldown time.Duration

	// MinSamplesReduce is the sample floor for timeout REDUCTIONS, held
	// deliberately above the shared MinSamples used by every other detector
	// (design §6.1). A reduction is the only tuning direction that can cause
	// truncation, so it must demand materially more evidence than a raise.
	// 0 → 30. The 2026-08-10 ingest outage was proposed off exactly 5 samples,
	// which cleared the shared floor's default of 5 at its bare minimum.
	MinSamplesReduce int

	// TimeoutBindingThreshold: a step's explicit timeout counts as the
	// binding constraint when observed step p95 ≥ threshold × timeout
	// (inclusive). 0 → 0.8 — surfaces impending truncation while normal
	// p95 variance still clears healthy runs (design §4.4).
	TimeoutBindingThreshold float64

	LeaderGate LeaderGate
	Logger     zerolog.Logger

	// TimeoutReclaimRatio: a step's explicit timeout is a reclaim
	// candidate when observed step p95 ≤ ratio × timeout, sustained.
	// 0 → 0.5. Deliberately far below TimeoutBindingThreshold (0.8) so
	// a raise detector and this reduction detector can't ping-pong on
	// the same step (design: asymmetric bands + the 3-tick streak).
	TimeoutReclaimRatio float64

	// per-signal consecutive-breach counters. failed/latency are keyed by
	// project; tool by the (project, tool) composite (typed, no string join);
	// reclaim by the (workflow, step) composite.
	latencyDiagnoses []time.Time // rolling attempt timestamps for the per-hour cap

	failedBreaches  map[string]int
	latencyBreaches map[string]int
	toolBreaches    map[ProjectToolKey]int
	reclaimStreaks  map[WorkflowStepKey]int
	bindingStreaks  map[WorkflowStepKey]int
	stopped         chan struct{}
}

// WorkflowStepKey identifies a step for the timeout-reclaim streak counter.
type WorkflowStepKey struct {
	Project  string
	Workflow string
	Step     string
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

// scanTimeoutBinding is the raise-side counterpart to scanTimeoutReclaim
// (design §6). It watches a step against its OWN ceiling rather than waiting
// for a project-level latency breach to surface it, which is the gap that let
// the 2026-08-10 easeit-companion ingest step time out on every attempt while
// on_fail:recover kept the executions completing.
//
// A step breaches when EITHER its degraded-outcome rate clears
// TimeoutDegradedRateThreshold, OR its p95 is at/above the binding threshold.
// The rate term is load-bearing, not belt-and-braces: a truncated run's
// recorded duration is capped at the timeout, so a step that is timing out
// constantly can still present a p95 well below the binding threshold and be
// invisible to a p95-only test.
func (w *TuneWorker) scanTimeoutBinding(ctx context.Context) {
	if w.Actionize == nil {
		return
	}
	if w.bindingStreaks == nil {
		w.bindingStreaks = map[WorkflowStepKey]int{}
	}
	steps, err := w.Metrics.StepLatencies(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read step-latency metrics for binding scan")
		return
	}
	seen := map[WorkflowStepKey]bool{}
	for _, st := range steps {
		key := WorkflowStepKey{Project: st.Project, Workflow: st.Workflow, Step: st.Step}
		current, explicit, cerr := w.Actionize.CurrentStepTimeout(st.Workflow, st.Step)
		if cerr != nil || !explicit {
			continue // no explicit timeout to raise
		}
		seen[key] = true
		candidate := st.Count >= w.minSamples() && stepTimeoutBinding(st, current, w.bindingThreshold(), w.degradedRateThreshold())
		if !advanceStreak(w.bindingStreaks, key, candidate, w.breachesToPropose()) {
			continue
		}
		w.proposeTimeoutBinding(ctx, st, current)
	}
	resetAbsent(w.bindingStreaks, seen)
}

// stepTimeoutBinding reports whether st is pinned against current: either its
// degraded-outcome RATE clears rateThreshold (a rate, so one timeout in a large
// clean window stays noise) or its p95 is at/above bindingThreshold × current.
// The p95 comparison keeps the existing inclusive-with-epsilon boundary.
func stepTimeoutBinding(st StepLatencySample, current time.Duration, bindingThreshold, rateThreshold float64) bool {
	if st.Count > 0 && float64(st.TimeoutCount)/float64(st.Count) >= rateThreshold {
		return true
	}
	return st.P95Seconds+1e-9 >= bindingThreshold*current.Seconds()
}

// bindingSuggestion sizes a raise for a step pinned against `current`.
//
// The subtlety is CENSORED DATA. Once a step starts timing out, its recorded
// durations are right-censored at the timeout: every truncated run reads as
// exactly `current`, and the real duration it needed is unknowable from the
// sample. So max(p95, observedMax) × 1.5 can land BELOW `current` — on the
// 2026-08-10 numbers it lands at 450s against a 600s ceiling — and the raise
// renders as ErrChangeNotUseful, leaving the step to keep timing out forever.
//
// When truncation has actually been observed we therefore escalate relative to
// the CURRENT timeout rather than to the censored observations. With no
// truncation the observations are trustworthy and the usual basis applies.
func bindingSuggestion(st StepLatencySample, current time.Duration) time.Duration {
	suggested := reclaimSuggestion(st) // max(p95, observedMax) × 1.5
	if st.TimeoutCount > 0 {
		if escalated := time.Duration(math.Ceil(current.Seconds()*1.5)) * time.Second; escalated > suggested {
			suggested = escalated
		}
	}
	return suggested
}

// proposeTimeoutBinding renders + files the raise, sized by bindingSuggestion.
func (w *TuneWorker) proposeTimeoutBinding(ctx context.Context, st StepLatencySample, current time.Duration) {
	suggested := bindingSuggestion(st, current)
	rc, rerr := w.Actionize.RenderStepTimeout(st.Workflow, st.Step, suggested)
	if rerr != nil {
		if !errors.Is(rerr, ErrChangeNotUseful) {
			w.Logger.Warn().Err(rerr).Str("project", st.Project).Str("step", st.Step).
				Msg("tune: step-timeout raise render failed")
		}
		return
	}
	timeoutRate := 0.0
	if st.Count > 0 {
		timeoutRate = float64(st.TimeoutCount) / float64(st.Count)
	}
	evidence := fmt.Sprintf(`{"signal":"step_timeout_binding","step_p95":%.1f,"step_max":%.1f,"step_count":%d,"timeout_count":%d,"timeout_rate":%.3f,"degraded_count":%d,"current_timeout_s":%.0f,"workflow":%q,"role":%q,"model":%q}`,
		st.P95Seconds, st.MaxSeconds, st.Count, st.TimeoutCount, timeoutRate, st.DegradedCount, current.Seconds(), st.Workflow, st.Role, st.Model)
	rationale := fmt.Sprintf("Step %q (workflow %s, role %s) is pinned against its %s timeout — p95 %.0fs, max %.0fs, and %d of %d runs (%.0f%%) were TRUNCATED, sustained for %d consecutive scans. Note a truncated run's duration is capped at the timeout, so p95 understates the pressure. Proposed: %s.",
		st.Step, st.Workflow, st.Role, formatDurationShort(current), st.P95Seconds, st.MaxSeconds,
		st.TimeoutCount, st.Count, timeoutRate*100, w.breachesToPropose(), rc.Summary)
	w.fileRendered(ctx, st.Project, tuneTimeoutBindingTitle(st.Project, st.Step), rationale, evidence, "tune-detector", rc)
}

func (w *TuneWorker) degradedRateThreshold() float64 {
	if w.TimeoutDegradedRateThreshold > 0 {
		return w.TimeoutDegradedRateThreshold
	}
	return 0.05
}

func (w *TuneWorker) rejectCooldown() time.Duration {
	if w.RejectCooldown > 0 {
		return w.RejectCooldown
	}
	return 168 * time.Hour
}

func (w *TuneWorker) minSamplesReduce() int {
	if w.MinSamplesReduce > 0 {
		return w.MinSamplesReduce
	}
	return 30
}

func (w *TuneWorker) reclaimRatio() float64 {
	if w.TimeoutReclaimRatio > 0 {
		return w.TimeoutReclaimRatio
	}
	return 0.5
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
	if w.reclaimStreaks == nil {
		w.reclaimStreaks = map[WorkflowStepKey]int{}
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
	w.scanTimeoutReclaim(ctx)
	// Runs after the reclaim scan deliberately: the two are mutually exclusive
	// by construction (reclaim refuses any step with a degraded outcome, and
	// binding requires either degradation or a p95 in the 0.8+ band, while
	// reclaim needs 0.5-), so ordering is cosmetic — but keeping the reduce
	// scan first means a step that has just started degrading stops being
	// proposed for reduction in the same tick it starts being proposed for a
	// raise.
	w.scanTimeoutBinding(ctx)
	w.scanStaleProposals(ctx)
}

// scanStaleProposals retires APPROVED single-op config proposals whose target
// file drifted since drafting (base_hash no longer matches on-disk), so a
// proposal that can never apply (ErrStaleBase) stops surfacing as actionable
// (design 2026-07-23 §B). Reuses REJECTED via SetStatus; the authoring
// detector re-files a fresh proposal (tune dedupes on open-DRAFT title only).
// Requires the Actionizer to read the deployed tree; a no-op otherwise.
func (w *TuneWorker) scanStaleProposals(ctx context.Context) {
	if w.Actionize == nil || w.Actionize.ReadFile == nil {
		return
	}
	approved, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		Statuses: []string{persistence.ProposalStatusApproved},
	})
	if err != nil {
		w.Logger.Warn().Err(err).Msg("control-plane: stale-proposal sweep: list failed")
		return
	}
	for _, p := range approved {
		if p == nil {
			continue
		}
		// Multi-op / review-only proposals are out of scope for the base-hash
		// staleness check (which covers a single config-target file).
		if strings.TrimSpace(p.ApplyOps) != "" || strings.TrimSpace(p.ApplyTarget) == "" {
			continue
		}
		base, ok := parseBaseHash(p.Evidence)
		if !ok {
			continue
		}
		fresh := false
		if cur, rerr := w.Actionize.ReadFile(p.ApplyTarget); rerr == nil && hashBytes(cur) == base {
			fresh = true
		}
		if fresh {
			continue
		}
		if serr := w.Proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusRejected, AutoRetireStaleActor); serr != nil {
			w.Logger.Warn().Err(serr).Str("proposal_id", p.ID).
				Msg("control-plane: stale-proposal sweep: retire failed")
			continue
		}
		w.Logger.Info().Str("proposal_id", p.ID).Str("target", p.ApplyTarget).
			Msg("control-plane: auto-retired stale proposal (config drifted since drafted)")
	}
}

// scanTimeoutReclaim is the reclaim-capacity counterpart to scanLatency:
// per (workflow, step) it proposes LOWERING an over-provisioned step
// timeout when observed p95 sits well below the configured value
// (p95 ≤ reclaimRatio × current), sustained for BreachesToPropose ticks.
// Requires the Actionizer (needs to read + rewrite the workflow); a no-op
// otherwise. Never fires on a step without an explicit timeout. The
// asymmetric band (0.5 vs the raise path's 0.8) plus the shared 3-tick
// streak keep the raise and reduce detectors from oscillating on one step.
func (w *TuneWorker) scanTimeoutReclaim(ctx context.Context) {
	if w.Actionize == nil {
		return
	}
	if w.reclaimStreaks == nil {
		w.reclaimStreaks = map[WorkflowStepKey]int{}
	}
	steps, err := w.Metrics.StepLatencies(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read step-latency metrics for reclaim")
		return
	}
	seen := map[WorkflowStepKey]bool{}
	for _, st := range steps {
		key := WorkflowStepKey{Project: st.Project, Workflow: st.Workflow, Step: st.Step}
		current, explicit, cerr := w.Actionize.CurrentStepTimeout(st.Workflow, st.Step)
		if cerr != nil || !explicit {
			continue // no timeout to reclaim, or workflow unreadable
		}
		seen[key] = true
		// Candidate when p95 has enough headroom below the configured timeout,
		// we have enough samples to trust it AGAINST THE REDUCE FLOOR (not the
		// shared one), and the step has not actually degraded in the window
		// (design §6.1).
		candidate := reclaimEligible(st, current, w.reclaimRatio(), w.minSamplesReduce())
		if !advanceStreak(w.reclaimStreaks, key, candidate, w.breachesToPropose()) {
			continue
		}
		w.proposeTimeoutReclaim(ctx, st, current)
	}
	resetAbsent(w.reclaimStreaks, seen)
}

// proposeTimeoutReclaim renders + files the applyable step-timeout
// reduction. Headroom multiplier 1.5 (same as the raise path) keeps the
// new timeout comfortably above observed p95. A render that declines
// (ErrChangeNotUseful — the suggestion wouldn't actually reduce) files
// nothing: reclaim is an optimisation, not a breach, so unlike the latency
// path there is no informational fallback.
func (w *TuneWorker) proposeTimeoutReclaim(ctx context.Context, st StepLatencySample, current time.Duration) {
	suggested := reclaimSuggestion(st)
	rc, rerr := w.Actionize.RenderStepTimeoutReduction(st.Workflow, st.Step, suggested)
	if rerr != nil {
		if !errors.Is(rerr, ErrChangeNotUseful) {
			w.Logger.Warn().Err(rerr).Str("project", st.Project).Str("step", st.Step).Msg("tune: step-timeout reduction render failed")
		}
		return
	}
	evidence := fmt.Sprintf(`{"signal":"step_timeout_reclaim","step_p95":%.1f,"step_count":%d,"current_timeout_s":%.0f,"workflow":%q,"role":%q,"model":%q}`,
		st.P95Seconds, st.Count, current.Seconds(), st.Workflow, st.Role, st.Model)
	rationale := fmt.Sprintf("Step %q (workflow %s, role %s) has p95 %.0fs against a %s timeout — at/under %.0f%% of the configured value for %d consecutive scans, so the timeout is over-provisioned. Proposed: %s. Reclaiming the headroom tightens scheduling without risking healthy-run truncation.",
		st.Step, st.Workflow, st.Role, st.P95Seconds, formatDurationShort(current), w.reclaimRatio()*100, w.breachesToPropose(), rc.Summary)
	w.fileRendered(ctx, st.Project, tuneTimeoutReclaimTitle(st.Project, st.Step), rationale, evidence, "tune-detector", rc)
}

func (w *TuneWorker) bindingThreshold() float64 {
	if w.TimeoutBindingThreshold > 0 {
		return w.TimeoutBindingThreshold
	}
	return 0.8
}

func (w *TuneWorker) scanFailedRate(ctx context.Context) {
	if w.SkipFailedRate != nil && w.SkipFailedRate() {
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
		FileFailedRateProposal(ctx, w.Proposals, w.Logger, project, s, w.breachesToPropose(), w.threshold(), "tune-detector")
	}
	resetAbsent(w.failedBreaches, seen)
}

// FileFailedRateProposal writes the generic review-only failed-rate DRAFT
// proposal, deduped on the open-DRAFT title. It is the SINGLE source of the
// generic failed-rate proposal — called by the Tune worker's scan (proposedBy
// "tune-detector") AND by the SelfHealWorker for coverage when its diagnosis is
// unavailable or non-fileable (proposedBy "self-heal", so the open-incidents
// counter reflects it and the worker's per-project dedup matches — self-healing
// design §4.5). The logic is never duplicated.
func FileFailedRateProposal(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project string, s RateSample, wantStreak int, threshold float64, proposedBy string) {
	title := tuneFailedRateTitle(project)
	rationale := fmt.Sprintf("Failed-task rate %.0f%% (%d/%d) over the scan window, sustained for %d consecutive scans — above the %.0f%% threshold. Investigate the failing step (logs/traces) or consider a model/timeout change.",
		s.Rate*100, s.Failed, s.Total, wantStreak, threshold*100)
	evidence := fmt.Sprintf(`{"signal":"failed_task_rate","rate":%.3f,"failed":%d,"total":%d}`, s.Rate, s.Failed, s.Total)
	fileProposal(ctx, proposals, logger, project, title, rationale, evidence, proposedBy)
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
		w.proposeLatency(ctx, project, s)
	}
	resetAbsent(w.latencyBreaches, seen)
}

// proposeLatency files the latency-breach proposal, attributing the slowest
// step and rendering a concrete step-timeout change when the step's explicit
// timeout is the binding constraint (actionable-proposals §4.4). Every
// fallback path files the informational proposal — a breach never goes
// silent.
func (w *TuneWorker) proposeLatency(ctx context.Context, project string, s LatencySample) {
	generic := fmt.Sprintf("Execution p95 latency %.0fs (n=%d) over the scan window, sustained for %d consecutive scans — above the %.0fs threshold. Investigate slow steps/tools or consider a faster model or a step-timeout change.",
		s.P95Seconds, s.Count, w.breachesToPropose(), w.latencyThreshold())
	evidence := fmt.Sprintf(`{"signal":"latency_p95_seconds","p95":%.1f,"count":%d}`, s.P95Seconds, s.Count)

	slow, ok := w.slowestStep(ctx, project)
	if !ok {
		w.propose(ctx, project, tuneLatencyTitle(project), generic, evidence, "tune-detector")
		return
	}
	stepEvidence := fmt.Sprintf(`{"signal":"latency_p95_seconds","p95":%.1f,"count":%d,"slowest_step":%q,"workflow":%q,"role":%q,"model":%q,"step_p95":%.1f,"step_count":%d}`,
		s.P95Seconds, s.Count, slow.Step, slow.Workflow, slow.Role, slow.Model, slow.P95Seconds, slow.Count)

	if w.tryActionableLatency(ctx, project, s, slow, stepEvidence) {
		return
	}
	// The timeout is not the binding constraint, so no threshold can render the
	// fix — the question is which MODEL, which is a judgement. Escalate to the
	// diagnoser; it files the applyable proposal or reports false and we fall
	// through to the informational floor below.
	if w.tryDiagnosedLatency(ctx, project, s, slow, stepEvidence) {
		return
	}
	// Timeout absent / not binding / render declined: informational, but
	// naming the slow step + model dimension explicitly (design §4.4).
	rationale := fmt.Sprintf("Execution p95 latency %.0fs (n=%d), sustained for %d consecutive scans — above the %.0fs threshold. Slowest step is %q (workflow %s, role %s, model %s) at p95 %.0fs; its timeout is not the binding constraint. Consider a faster model for role %s — run Diagnose for a specific recommendation.",
		s.P95Seconds, s.Count, w.breachesToPropose(), w.latencyThreshold(),
		slow.Step, slow.Workflow, slow.Role, slow.Model, slow.P95Seconds, slow.Role)
	w.propose(ctx, project, tuneLatencyTitle(project), rationale, stepEvidence, "tune-detector")
}

// tryActionableLatency renders + files the applyable step-timeout proposal
// when the slowest step's explicit timeout is the binding constraint
// (p95 ≥ TimeoutBindingThreshold × current, inclusive). Reports whether a
// proposal was filed; false → the caller files the informational one.
func (w *TuneWorker) tryActionableLatency(ctx context.Context, project string, s LatencySample, slow StepLatencySample, stepEvidence string) bool {
	if w.Actionize == nil {
		return false
	}
	current, explicit, err := w.Actionize.CurrentStepTimeout(slow.Workflow, slow.Step)
	// The ≥-inclusive boundary with a tiny epsilon: 0.8×current is not exact
	// in float64, and p95 exactly at the threshold must count as binding
	// (design §4.4 / round-2 review) rather than lose to representation error.
	if err != nil || !explicit || slow.P95Seconds+1e-9 < w.bindingThreshold()*current.Seconds() {
		return false
	}
	suggested := time.Duration(math.Ceil(slow.P95Seconds*1.5)) * time.Second
	rc, rerr := w.Actionize.RenderStepTimeout(slow.Workflow, slow.Step, suggested)
	if rerr != nil {
		if !errors.Is(rerr, ErrChangeNotUseful) {
			w.Logger.Warn().Err(rerr).Str("project", project).Str("step", slow.Step).Msg("tune: step-timeout render failed; filing informational")
		}
		return false
	}
	clampNote := ""
	if rc.Clamped {
		clampNote = fmt.Sprintf(" (clamped; the raw step p95 %.0fs suggests a larger bump — raise it by hand if you truly need more)", slow.P95Seconds)
		w.Logger.Info().Str("project", project).Str("step", slow.Step).Msg("tune: suggested step timeout clamped to bound")
	}
	rationale := fmt.Sprintf("Execution p95 latency %.0fs (n=%d), sustained for %d consecutive scans — above the %.0fs threshold. Slowest step is %q (role %s, model %s) at p95 %.0fs, at/over %.0f%% of its %s timeout — the timeout is the binding constraint. Proposed: %s%s.",
		s.P95Seconds, s.Count, w.breachesToPropose(), w.latencyThreshold(),
		slow.Step, slow.Role, slow.Model, slow.P95Seconds, w.bindingThreshold()*100, formatDurationShort(current), rc.Summary, clampNote)
	w.fileRendered(ctx, project, tuneLatencyTitle(project), rationale, stepEvidence, "tune-detector", rc)
	return true
}

// tryDiagnosedLatency escalates a latency breach the timeout path could not
// fix: it asks the diagnoser which model to run the slow role on, renders that
// swap, and files it as an APPLYABLE proposal. Reports whether it filed;
// false → the caller files the informational one.
//
// WHY AN LLM AND NOT A THRESHOLD. StepLatencies reports p95 per (project,
// workflow, step, role, model), so this could pick the fastest model on the
// fleet from numbers alone. It deliberately does not: the fastest model is
// usually the smallest, so swapping a reviewer onto it trades output quality
// nobody is measuring for a latency number everybody is — and the regression
// would be invisible in exactly the metric that triggered the change. A model
// choice is a quality/cost/latency judgement, and this is the path that makes
// judgements.
//
// The daemon keeps the veto throughout: the verdict names a model, and
// RenderRoleModel refuses one outside the configured universe, a role that does
// not exist, and a no-op. NOTHING the model wrote as prose reaches the ledger —
// the title is tuneLatencyTitle(project), the rationale is written from metrics
// here, and only the structured model id is taken from the verdict. That is why
// this path needs no equivalent of maybeFileProposal's URL/secret output gate:
// it has no model free-text to screen.
//
// See https://docs.vornik.io
func (w *TuneWorker) tryDiagnosedLatency(ctx context.Context, project string, s LatencySample, slow StepLatencySample, stepEvidence string) bool {
	if w.Diagnose == nil || w.Actionize == nil {
		return false
	}
	// Loop safety, checked BEFORE the call so the exclusion costs nothing.
	if w.SystemProjectID != "" && project == w.SystemProjectID {
		return false
	}
	if !w.underLatencyDiagnosisCap() {
		w.Logger.Warn().Str("project", project).
			Int("cap_per_hour", w.maxLatencyDiagnosesPerHour()).
			Msg("tune: latency diagnosis rate-capped; filing informational")
		return false
	}
	// Counted here, not on success: the spend happens whether or not the
	// verdict turns out to be renderable.
	w.recordLatencyDiagnosis()

	verdict, _, err := w.Diagnose.Diagnose(ctx, project, false)
	if err != nil {
		w.Logger.Warn().Err(err).Str("project", project).
			Msg("tune: latency diagnosis failed; filing informational")
		return false
	}
	if verdict == nil || verdict.ConfigChange == nil {
		return false
	}
	cc := verdict.ConfigChange
	if cc.Kind != "swarm_role_model" {
		// The diagnoser may legitimately conclude the fix is a timeout or an
		// MCP change. Those are the timeout path's business or nobody's; this
		// escalation exists for the model question only.
		return false
	}
	rc, rerr := w.Actionize.RenderRoleModel(cc.Swarm, cc.Role, cc.Model)
	if rerr != nil {
		if !errors.Is(rerr, ErrChangeNotUseful) {
			w.Logger.Warn().Err(rerr).Str("project", project).Str("swarm", cc.Swarm).
				Str("role", cc.Role).Str("model", cc.Model).
				Msg("tune: role-model render refused; filing informational")
		}
		return false
	}
	rationale := fmt.Sprintf("Execution p95 latency %.0fs (n=%d), sustained for %d consecutive scans — above the %.0fs threshold. Slowest step is %q (workflow %s, role %s, model %s) at p95 %.0fs, and its timeout is NOT the binding constraint — so the model is the cost, not the budget. Proposed: %s. Review the trade-off before applying: this is chosen for latency, and a faster model may produce weaker output for this role.",
		s.P95Seconds, s.Count, w.breachesToPropose(), w.latencyThreshold(),
		slow.Step, slow.Workflow, slow.Role, slow.Model, slow.P95Seconds, rc.Summary)
	// tuneLatencyTitle(project) — the SAME title the informational and
	// timeout paths use. That is what lets fileRendered supersede a stale
	// informational DRAFT instead of filing beside it.
	w.fileRendered(ctx, project, tuneLatencyTitle(project), rationale, stepEvidence, "tune-detector", rc)
	return true
}

func (w *TuneWorker) maxLatencyDiagnosesPerHour() int {
	if w.MaxLatencyDiagnosesPerHour > 0 {
		return w.MaxLatencyDiagnosesPerHour
	}
	return 3
}

// underLatencyDiagnosisCap reports whether another diagnosis may be attempted
// this hour, pruning the rolling window as it goes.
func (w *TuneWorker) underLatencyDiagnosisCap() bool {
	cutoff := time.Now().Add(-time.Hour)
	kept := w.latencyDiagnoses[:0]
	for _, t := range w.latencyDiagnoses {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.latencyDiagnoses = kept
	return len(w.latencyDiagnoses) < w.maxLatencyDiagnosesPerHour()
}

func (w *TuneWorker) recordLatencyDiagnosis() {
	w.latencyDiagnoses = append(w.latencyDiagnoses, time.Now())
}

// slowestStep picks the breaching project's slowest attributed step with
// count ≥ MinSamples. Deterministic order: p95 DESC, count DESC, step ASC
// (review #1).
func (w *TuneWorker) slowestStep(ctx context.Context, project string) (StepLatencySample, bool) {
	steps, err := w.Metrics.StepLatencies(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("tune: failed to read step-latency metrics")
		return StepLatencySample{}, false
	}
	var best StepLatencySample
	found := false
	for _, st := range steps {
		if st.Project != project || st.Count < w.minSamples() {
			continue
		}
		if !found || st.P95Seconds > best.P95Seconds ||
			(st.P95Seconds == best.P95Seconds && (st.Count > best.Count ||
				(st.Count == best.Count && st.Step < best.Step))) {
			best = st
			found = true
		}
	}
	return best, found
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
		rationale := fmt.Sprintf("Tool %q p95 call latency is %.0fs (n=%d) in project %s, sustained for %d consecutive scans — above the %.0fs threshold. Consider raising this tool's timeout to ~%.0fs%s, or investigate why the tool is slow.",
			s.Key.Tool, s.P95Seconds, s.Count, s.Key.Project, w.breachesToPropose(), threshold, suggested, clamped)
		evidence := fmt.Sprintf(`{"signal":"tool_latency_p95_seconds","tool":%q,"p95":%.1f,"count":%d,"suggested_timeout_s":%.0f}`, s.Key.Tool, s.P95Seconds, s.Count, suggested)
		rc, why := w.renderToolTimeout(s.Key, int(suggested))
		if rc != nil {
			w.fileRendered(ctx, s.Key.Project, instinctToolTimeoutTitle(s.Key),
				rationale+" Proposed: "+rc.Summary+".", evidence, "instinct", rc)
			continue
		}
		if why != "" {
			// Don't advise a raise the config already exceeds (implementation
			// review #9) — say what the informational proposal actually means.
			rationale = fmt.Sprintf("Tool %q p95 call latency is %.0fs (n=%d) in project %s, sustained for %d consecutive scans — above the %.0fs threshold. %s",
				s.Key.Tool, s.P95Seconds, s.Count, s.Key.Project, w.breachesToPropose(), threshold, why)
		}
		w.propose(ctx, s.Key.Project, instinctToolTimeoutTitle(s.Key), rationale, evidence, "instinct")
	}
	resetAbsent(w.toolBreaches, seen)
}

// renderToolTimeout maps an MCP tool's qualified name to its server's
// timeout_seconds key (daemon-first scope, design §4.4) and renders the
// raise. rc==nil → the caller files the informational proposal (builtin
// tool, unknown server, raise-only guard, or render failure); a non-empty
// `why` replaces the generic raise advice when it would mislead.
func (w *TuneWorker) renderToolTimeout(key ProjectToolKey, suggestedSeconds int) (rc *RenderedChange, why string) {
	if w.Actionize == nil {
		return nil, ""
	}
	server, _, isMCP := ParseMCPToolName(key.Tool)
	if !isMCP {
		return nil, ""
	}
	scope, ok := w.Actionize.FindMCPServerScope(key.Project, server)
	if !ok {
		return nil, ""
	}
	rendered, err := w.Actionize.RenderMCPServerTimeout(scope, server, suggestedSeconds)
	if err != nil {
		if errors.Is(err, ErrChangeNotUseful) {
			return nil, fmt.Sprintf("The %s server's configured timeout already exceeds the ~%ds this p95 suggests — the timeout is not the constraint; investigate why the tool is slow (or reduce the timeout by hand if reclaiming capacity is the goal).", server, suggestedSeconds)
		}
		w.Logger.Warn().Err(err).Str("tool", key.Tool).Msg("tune: tool-timeout render failed; filing informational")
		return nil, ""
	}
	return rendered, ""
}

// fileRendered files a DRAFT proposal carrying a rendered applyable change:
// ApplyTarget/ApplyContent/Diff from the render, blast radius + live-apply
// from the change kind, and Evidence extended with {"base_hash", "change"}
// so apply re-validates the typed change against current state. Deduped on
// the open-DRAFT title like every worker proposal.
func (w *TuneWorker) fileRendered(ctx context.Context, project, title, rationale, evidenceJSON, proposedBy string, rc *RenderedChange) {
	if rejectedWithinCooldown(ctx, w.Proposals, w.Logger, project, title, w.rejectCooldown()) {
		return
	}
	if applyableDraftExistsForTarget(ctx, w.Proposals, w.Logger, project, title, rc) {
		return
	}
	fileRenderedProposal(ctx, w.Proposals, w.Logger, project, title, rationale, evidenceJSON, proposedBy, rc)
}

// applyableDraftExistsForTarget reports whether an open applyable DRAFT already
// edits the same file, in which case filing another is worse than noise.
//
// Single-op applies carry a base_hash of the target file. Two open proposals
// against one file means that whichever applies first leaves the other
// permanently ErrStaleBase — the exact dead-proposal state the 2026-07-23
// live-value-edits design had to add a retire sweep for. Two detectors
// genuinely can want the same file in one tick: scanLatency's raise path and
// scanTimeoutBinding both size a step timeout.
//
// Scoped to applyable proposals on both sides: an informational proposal has no
// target and no base_hash, so it neither blocks nor is blocked — the
// supersede-informational-with-applyable upgrade path depends on that.
// A same-title proposal is left to the existing dedup so its supersede
// behaviour is unchanged.
func applyableDraftExistsForTarget(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project, title string, rc *RenderedChange) bool {
	if proposals == nil || rc == nil || strings.TrimSpace(rc.ApplyTarget) == "" {
		return false
	}
	open, err := proposals.List(ctx, persistence.ProposalListFilter{
		ProjectID: project, Statuses: []string{persistence.ProposalStatusDraft},
	})
	if err != nil {
		// Fail OPEN, as with the rejection cooldown: a ledger read error must
		// not stop a real breach being surfaced.
		logger.Warn().Err(err).Msg("control-plane: same-target lookup failed; filing anyway")
		return false
	}
	for _, p := range open {
		if p == nil || p.Title == title {
			continue // same title → existing dedup/supersede logic owns it
		}
		if strings.TrimSpace(p.ApplyTarget) == strings.TrimSpace(rc.ApplyTarget) {
			logger.Debug().Str("target", rc.ApplyTarget).Str("held_by", p.Title).Str("skipped", title).
				Msg("control-plane: an applyable draft already edits this file; not filing a second")
			return true
		}
	}
	return false
}

// upsertObservation files a non-decidable observation, or refreshes the
// existing one for this (project, title) if it has been seen before.
//
// The dedup deliberately ignores STATUS. Keying on open-DRAFT — what the
// applyable path does, correctly, because a decided proposal is spent — is what
// produced the 2026-08-10 inert pile: approving an informational row moved it
// out of DRAFT, the dedup stopped matching, and the detector filed a fresh copy
// on its next streak. An observation is a standing finding, not a one-shot
// request, so its identity is (project, title) for as long as the row exists.
func upsertObservation(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project, title, rationale, evidence, proposedBy string) {
	existing, err := proposals.List(ctx, persistence.ProposalListFilter{ProjectID: project})
	if err != nil {
		logger.Warn().Err(err).Str("project", project).Msg("control-plane: observation lookup failed; filing a new row")
	}
	for _, p := range existing {
		if p == nil || p.Title != title || !persistence.IsObservationKind(p.Kind) {
			continue
		}
		merged, occ := bumpObservationOccurrences(p.Evidence, evidence)
		if rerr := proposals.RefreshObservation(ctx, p.ID, rationale, merged); rerr != nil {
			logger.Warn().Err(rerr).Str("proposal_id", p.ID).Msg("control-plane: observation refresh failed")
			return
		}
		logger.Debug().Str("proposal_id", p.ID).Str("title", title).Int("occurrences", occ).
			Msg("control-plane: observation recurred; refreshed in place")
		return
	}
	merged, _ := bumpObservationOccurrences("", evidence)
	p := &persistence.ControlPlaneProposal{
		ID:          persistence.GenerateID("cpp"),
		ProjectID:   project,
		Kind:        persistence.ProposalKindObservation,
		BlastRadius: persistence.ProposalScopeProject,
		Title:       title,
		Rationale:   rationale,
		Evidence:    merged,
		Status:      persistence.ProposalStatusDraft,
		ProposedBy:  proposedBy,
	}
	if err := proposals.Create(ctx, p); err != nil {
		logger.Warn().Err(err).Str("project", project).Msg("control-plane: failed to create observation")
		return
	}
	logger.Info().Str("proposal_id", p.ID).Str("title", title).Msg("control-plane: observation filed")
}

// bumpObservationOccurrences merges the newest evidence with a running
// occurrence count carried on the prior row, and returns the merged JSON plus
// the new count. The count is what tells an operator "still happening" — without
// it, refreshing in place would silently hide that a finding is persistent
// rather than a one-off blip.
func bumpObservationOccurrences(priorEvidence, newEvidence string) (string, int) {
	occ := 1
	if strings.TrimSpace(priorEvidence) != "" {
		var prior map[string]any
		if err := json.Unmarshal([]byte(priorEvidence), &prior); err == nil {
			if n, ok := prior["occurrences"].(float64); ok {
				occ = int(n) + 1
			}
		}
	}
	fields := map[string]any{}
	if strings.TrimSpace(newEvidence) != "" {
		if err := json.Unmarshal([]byte(newEvidence), &fields); err != nil {
			fields = map[string]any{"raw": newEvidence}
		}
	}
	fields["occurrences"] = occ
	out, err := json.Marshal(fields)
	if err != nil {
		return newEvidence, occ
	}
	return string(out), occ
}

// rejectedWithinCooldown reports whether an operator has REJECTED a proposal
// with this title recently enough that re-filing it would be nagging rather
// than informing (design §7).
//
// Measured from the MOST RECENT rejection, so a detector that re-files the
// instant a cooldown lapses and is rejected again backs off again instead of
// oscillating. Per-title, not per-project: a global cooldown would let one
// rejection mute every unrelated detector.
func rejectedWithinCooldown(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project, title string, cooldown time.Duration) bool {
	if proposals == nil || cooldown <= 0 {
		return false
	}
	rejected, err := proposals.List(ctx, persistence.ProposalListFilter{
		ProjectID: project, Statuses: []string{persistence.ProposalStatusRejected},
	})
	if err != nil {
		// Fail OPEN: a ledger read error must not silently stop the detectors
		// from surfacing a real breach. Worst case we re-file something the
		// operator rejected, which is the pre-2026-08-10 behaviour.
		logger.Warn().Err(err).Msg("control-plane: rejection-cooldown lookup failed; filing anyway")
		return false
	}
	var latest time.Time
	for _, p := range rejected {
		if p == nil || p.Title != title || p.DecidedAt == nil {
			continue
		}
		if p.DecidedAt.After(latest) {
			latest = *p.DecidedAt
		}
	}
	if latest.IsZero() || time.Since(latest) >= cooldown {
		return false
	}
	logger.Debug().Str("title", title).Time("rejected_at", latest).Dur("cooldown", cooldown).
		Msg("control-plane: suppressed re-filing a recently rejected proposal")
	return true
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
	fileRenderedProposal(ctx, proposals, logger, project, title, rationale, evidence, proposedBy, nil)
}

// fileRenderedProposal is fileProposal plus an optional rendered applyable
// change (actionable-proposals §4.3). rc == nil files the plain review-only
// proposal; otherwise ApplyTarget/ApplyContent/Diff, blast radius, and
// live-apply come from the render and Evidence is extended with
// {"base_hash", "change"} (the apply engine's staleness gate + apply-time
// re-validation input).
func fileRenderedProposal(ctx context.Context, proposals persistence.ProposalRepository, logger zerolog.Logger, project, title, rationale, evidence, proposedBy string, rc *RenderedChange) {
	// No rendered change ⇒ an OBSERVATION, not a decidable proposal. Handled
	// FIRST, ahead of the open-DRAFT dedup below: that dedup returns early on a
	// title match, which would swallow the recurrence and leave the occurrence
	// count frozen at 1. Observations key on (project, title) across EVERY
	// status — keying on DRAFT is what produced the 2026-08-10 inert pile, since
	// approving a row moved it out of DRAFT and the detector filed a fresh copy.
	if rc == nil {
		upsertObservation(ctx, proposals, logger, project, title, rationale, evidence, proposedBy)
		return
	}
	existing, err := proposals.List(ctx, persistence.ProposalListFilter{
		ProjectID: project, Statuses: []string{persistence.ProposalStatusDraft},
	})
	if err == nil {
		for _, p := range existing {
			if p.Title != title {
				continue
			}
			// Applyable upgrade (implementation review #7): a proposal that
			// started informational and now renders an applyable change
			// carries the same title — supersede the stale informational
			// DRAFT (→ REJECTED, actor = the detector) so the operator gets
			// the row with the Apply button instead of being stranded on
			// prose. Anything else (same shape, or downgrade) dedups.
			stale := strings.TrimSpace(p.ApplyTarget) == "" && strings.TrimSpace(p.ApplyOps) == ""
			if rc == nil || !stale {
				return
			}
			if serr := proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusRejected, proposedBy); serr != nil {
				logger.Warn().Err(serr).Str("proposal_id", p.ID).
					Msg("control-plane: failed to supersede informational draft; keeping it")
				return
			}
			logger.Info().Str("proposal_id", p.ID).Str("title", title).
				Msg("control-plane: superseded informational DRAFT with an applyable render")
			break
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
	if rc != nil {
		p.ApplyTarget = rc.ApplyTarget
		p.ApplyContent = rc.ApplyContent
		p.Diff = rc.Diff
		p.LiveApply = rc.LiveApply
		if rc.BlastRadius != "" {
			p.BlastRadius = rc.BlastRadius
		}
		if merged, merr := mergeChangeEvidence(evidence, rc); merr == nil {
			p.Evidence = merged
		} else {
			logger.Warn().Err(merr).Str("project", project).Msg("control-plane: evidence merge failed; filing informational")
			p.ApplyTarget, p.ApplyContent, p.Diff, p.LiveApply = "", "", "", false
		}
	}
	if err := proposals.Create(ctx, p); err != nil {
		logger.Warn().Err(err).Str("project", project).Msg("control-plane: failed to create proposal")
		return
	}
	logger.Info().Str("project", project).Str("proposal_id", p.ID).Str("title", title).Str("by", proposedBy).
		Bool("applyable", rc != nil).Msg("control-plane: raised DRAFT proposal")
}

// mergeChangeEvidence folds {"base_hash": …, "change": {…}} into the
// signal's evidence JSON object.
func mergeChangeEvidence(evidence string, rc *RenderedChange) (string, error) {
	m := map[string]any{}
	if strings.TrimSpace(evidence) != "" {
		if err := json.Unmarshal([]byte(evidence), &m); err != nil {
			return "", err
		}
	}
	m["base_hash"] = rc.BaseHash
	m["change"] = rc.Change
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tuneFailedRateTitle(project string) string {
	return "Tune: high failed-task rate on " + project
}

func tuneLatencyTitle(project string) string {
	return "Tune: high p95 latency on " + project
}

// tuneTimeoutReclaimTitle keys the reduction proposal per (project, step) so
// it dedups independently of the raise-side latency proposal (which is keyed
// per project). A step-scoped title also lets several reclaimable steps in
// one project coexist as distinct proposals.
func tuneTimeoutReclaimTitle(project, step string) string {
	return fmt.Sprintf("Tune: reclaim over-provisioned timeout for %s on %s", step, project)
}

// tuneTimeoutBindingTitle keys the raise proposal per (project, step), mirroring
// the reclaim title so the two dedup independently of each other and of the
// project-keyed latency proposal.
func tuneTimeoutBindingTitle(project, step string) string {
	return fmt.Sprintf("Tune: raise binding timeout for %s on %s", step, project)
}

func instinctToolTimeoutTitle(k ProjectToolKey) string {
	return fmt.Sprintf("Instinct: %s timeouts in %s", k.Tool, k.Project)
}
