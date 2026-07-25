package controlplane

// canary_guard.go — the cost/quality canary + regression auto-rollback guard
// (LLD 2026-07-24-cost-quality-canary-rollback-design §D). A leader-gated
// background worker that, after a cost-tuning `config` proposal is APPLIED by the
// prompt_token_budget detector, opens a CANARY on the affected (swarm, role),
// compares a pre-apply baseline quality window to a post-apply window (both
// anchored to the proposal's AppliedAt), and on a statistically-guarded
// regression AUTO-ROLLS-BACK via the existing ApplyEngine.Rollback, marks the
// proposal REGRESSED, and opens a per-(swarm,role,knob) cooldown.
//
// The canary row is the SINGLE source of trip-prevention + cooldown (crash-safe,
// §4.5 C2): the durable row write (status + cooldown) ALWAYS precedes the
// best-effort MarkRegressed badge, so a lost badge never causes a re-propose
// flip-flop. Ships enabled:false; the kill-switch is re-checked at trip time.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/quality"
)

// costQualityDetectorProposedBy is the ProposedBy identity the prompt-token-budget
// detector stamps; the guard only canaries proposals it authored (design §4 step 1).
const costQualityDetectorProposedBy = "cost-quality-detector"

// swarmRoleEnvChangeKind is the Evidence.change.kind of a cost-tuning role-env
// proposal (Actionizer.RenderRoleEnv) — the only kind the guard canaries.
const swarmRoleEnvChangeKind = "swarm_role_env"

// canaryQuality is the read-model seam the guard needs: a bounded, gauge-free
// historical window read. Satisfied by *quality.Service (RefreshBetween); faked
// in tests.
type canaryQuality interface {
	RefreshBetween(ctx context.Context, from, to time.Time) (quality.Report, error)
}

// CanaryGuardWorker discovers newly-APPLIED cost-tuning proposals + evaluates
// open canaries. Deps are interfaces/funcs so the container wires real components
// and tests drive tick()/evaluateOne() in-memory.
type CanaryGuardWorker struct {
	Quality   canaryQuality
	Canaries  persistence.CostTuningCanaryRepository
	Proposals persistence.ProposalRepository
	// Rollback is the de-escalation primitive — bound to ApplyEngine.Rollback.
	// It takes the global apply mutex (ErrApplyInProgress on contention) and
	// fails closed on target drift (ErrRollbackTargetDrifted). The guard NEVER
	// mutates the engine; it only calls this.
	Rollback func(ctx context.Context, id string) error
	// BlastRadius returns the projects sharing `swarm` and the workflows those
	// projects run — the A2 watch set, re-derived from the registry at open
	// (design §5, not stale Evidence).
	BlastRadius func(swarm string) (projectIDs, workflowIDs []string)
	// Enabled is the live-reloadable kill-switch, re-checked at trip time
	// (design §9 #2). Nil ⇒ disabled.
	Enabled func() bool
	// SwarmAllowed is the per-swarm rollout allow-list, consulted at discover AND
	// trip time (design §8/I7). Nil ⇒ all swarms allowed.
	SwarmAllowed func(swarm string) bool
	// IsTradingSwarm re-checks the trading exclusion at trip time (design §9 #4).
	// Nil ⇒ never trading (the container always wires the real check).
	IsTradingSwarm func(swarm string) bool

	MinSamples   int
	A2MinSamples int
	A2Subwindows int
	MarginA1     float64
	MarginA2     float64
	MarginCost   float64
	Window       time.Duration
	// Cooldown is NOT consulted by the guard: the cooldown record IS the
	// regressed canary row (its closed_at), and the cooldown DURATION is applied
	// by the detector's skip (CostQualityWorker.CooldownDuration, design §7). The
	// guard only stamps closed_at at finalize.
	MaxCanaryAge time.Duration
	Interval     time.Duration

	LeaderGate LeaderGate
	Metrics    *CanaryMetrics
	// Now is injectable for deterministic tests; nil ⇒ time.Now.
	Now    func() time.Time
	Logger zerolog.Logger

	stopped chan struct{}
}

func (w *CanaryGuardWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
func (w *CanaryGuardWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return 15 * time.Minute
}
func (w *CanaryGuardWorker) window() time.Duration {
	if w.Window > 0 {
		return w.Window
	}
	return 168 * time.Hour
}
func (w *CanaryGuardWorker) maxCanaryAge() time.Duration {
	if w.MaxCanaryAge > 0 {
		return w.MaxCanaryAge
	}
	return 336 * time.Hour
}
func (w *CanaryGuardWorker) minSamples() int64 {
	if w.MinSamples > 0 {
		return int64(w.MinSamples)
	}
	return 20
}
func (w *CanaryGuardWorker) a2MinSamples() int64 {
	if w.A2MinSamples > 0 {
		return int64(w.A2MinSamples)
	}
	return 10
}
func (w *CanaryGuardWorker) a2Subwindows() int {
	if w.A2Subwindows > 0 {
		return w.A2Subwindows
	}
	return 4
}
func (w *CanaryGuardWorker) marginA1() float64 {
	if w.MarginA1 > 0 {
		return w.MarginA1
	}
	return 0.05
}
func (w *CanaryGuardWorker) marginA2() float64 {
	if w.MarginA2 > 0 {
		return w.MarginA2
	}
	return 0.10
}
func (w *CanaryGuardWorker) marginCost() float64 {
	if w.MarginCost > 0 {
		return w.MarginCost
	}
	return 0.15
}
func (w *CanaryGuardWorker) enabled() bool { return w.Enabled != nil && w.Enabled() }
func (w *CanaryGuardWorker) isTrading(s string) bool {
	return w.IsTradingSwarm != nil && w.IsTradingSwarm(s)
}
func (w *CanaryGuardWorker) swarmAllowed(s string) bool {
	return w.SwarmAllowed == nil || w.SwarmAllowed(s)
}

// Run drives the periodic loop until ctx is cancelled. Leader-gated (per the
// Tune/SelfHeal precedent) so one replica acts — no double-rollback. Does one
// initial scan after a short settle, then ticks on Interval. tick honours the
// kill-switch internally, so the gate is respected on every pass.
func (w *CanaryGuardWorker) Run(ctx context.Context) {
	if w == nil || w.Proposals == nil || w.Canaries == nil || w.Quality == nil {
		return
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)
	w.Logger.Info().Dur("interval", w.interval()).Msg("control-plane canary guard started")
	defer w.Logger.Info().Msg("control-plane canary guard stopped")

	select {
	case <-ctx.Done():
		return
	case <-time.After(initialScanDelay):
		if w.LeaderGate == nil || w.LeaderGate.IsLeader() {
			w.tick(ctx)
		}
	}
	t := time.NewTicker(w.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
				continue
			}
			w.tick(ctx)
		}
	}
}

// Stopped returns a channel closed when Run exits (test sync).
func (w *CanaryGuardWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

// tick runs one pass: DISCOVER (only when enabled — design §4 step 1) then
// EVALUATE (always, so the step-0 failsafe force-closes stale canaries and the
// detector is never frozen even while the guard is braked, §7).
func (w *CanaryGuardWorker) tick(ctx context.Context) {
	if w.enabled() {
		w.discover(ctx)
	}
	w.evaluate(ctx)
}

// discover opens a canary for each APPLIED cost-tuning proposal lacking a canary
// row (design §4 step 1).
func (w *CanaryGuardWorker) discover(ctx context.Context) {
	applied, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		Statuses: []string{persistence.ProposalStatusApplied},
	})
	if err != nil {
		w.Logger.Warn().Err(err).Msg("canary: list applied proposals failed")
		return
	}
	for _, p := range applied {
		if p == nil || p.ProposedBy != costQualityDetectorProposedBy {
			continue
		}
		// Already canaried (any status)? Discovery keys on NO canary row,
		// independent of proposal status (design §6) — makes rediscovery after a
		// trip impossible.
		if _, gerr := w.Canaries.GetByProposalID(ctx, p.ID); gerr == nil {
			continue
		} else if !errors.Is(gerr, persistence.ErrNotFound) {
			w.Logger.Warn().Err(gerr).Str("proposal_id", p.ID).Msg("canary: canary lookup failed")
			continue
		}
		swarm, role, knob, ok := parseSwarmRoleEnvChange(p.Evidence)
		if !ok {
			// Coverage gap (I8): an APPLIED cost-tuning proposal whose Evidence
			// didn't yield a swarm_role_env change — a silent stringly-typed
			// contract drift made visible rather than a silent no-op.
			w.Metrics.incCoverageGap()
			w.Logger.Warn().Str("proposal_id", p.ID).
				Msg("canary: APPLIED cost-tuning proposal opened no canary (unmatched Evidence.change.kind)")
			continue
		}
		if p.AppliedAt == nil {
			// Coverage gap (I8): an APPLIED cost-tuning proposal with no applied_at
			// is a data-integrity anomaly that opens no canary — surface it the same
			// way as an unparseable change, not just a log line.
			w.Metrics.incCoverageGap()
			w.Logger.Warn().Str("proposal_id", p.ID).Msg("canary: APPLIED proposal has no applied_at; skipping")
			continue
		}
		if w.isTrading(swarm) || !w.swarmAllowed(swarm) {
			continue // legitimate skip — not a coverage gap
		}
		w.openCanary(ctx, p, swarm, role, knob)
	}
}

// openCanary captures the pre-apply baseline over [baselineStart, AppliedAt) and
// inserts the open canary row (design §4 step 1, D1). baselineStart is clamped to
// the prior canary's window_until on this (swarm,role) so the baseline never
// spans a prior tuning change (I3).
func (w *CanaryGuardWorker) openCanary(ctx context.Context, p *persistence.ControlPlaneProposal, swarm, role, knob string) {
	appliedAt := p.AppliedAt.UTC()
	baselineStart := appliedAt.Add(-w.window())
	if prior, ok, perr := w.Canaries.LatestWindowUntil(ctx, swarm, role, appliedAt); perr != nil {
		w.Logger.Warn().Err(perr).Str("proposal_id", p.ID).Msg("canary: baseline-clamp lookup failed; using unclamped baseline")
	} else if ok && prior.After(baselineStart) {
		baselineStart = prior // I3 clamp
	}

	projectIDs, workflowIDs := w.blastRadius(swarm)

	rep, err := w.Quality.RefreshBetween(ctx, baselineStart, appliedAt)
	if err != nil {
		// Can't capture a baseline this tick — leave it undiscovered; next tick
		// retries (the proposal is still APPLIED with no canary row).
		w.Logger.Warn().Err(err).Str("proposal_id", p.ID).Msg("canary: baseline read failed; will retry")
		return
	}
	baseline := persistence.CanaryBaseline{}
	if a1, ok := quality.PickRole(rep, swarm, role); ok {
		baseline.A1Rate = a1.QualityRate
		baseline.A1Samples = a1.SampleCount
		baseline.A1Sufficient = a1.SampleCount >= w.minSamples()
		baseline.EffCost = a1.EffectiveCostTokens
	}
	if len(workflowIDs) > 0 {
		baseline.A2 = make(map[string]persistence.CanaryA2Series, len(workflowIDs))
		for _, wf := range workflowIDs {
			if s, ok := quality.PickWorkflow(rep, swarm, wf); ok {
				baseline.A2[wf] = persistence.CanaryA2Series{
					Rate:       s.QualityRate,
					Samples:    s.SampleCount,
					Sufficient: s.SampleCount >= w.a2MinSamples(),
				}
			}
		}
	}
	c := &persistence.CostTuningCanary{
		ProposalID:    p.ID,
		SwarmID:       swarm,
		Role:          role,
		Knob:          knob,
		ProjectIDs:    projectIDs,
		WorkflowIDs:   workflowIDs,
		AppliedAt:     appliedAt,
		BaselineStart: baselineStart,
		WindowUntil:   appliedAt.Add(w.window()),
		Baseline:      baseline,
		Status:        persistence.CanaryStatusOpen,
		OpenedAt:      w.now().UTC(),
	}
	if err := w.Canaries.Open(ctx, c); err != nil {
		w.Logger.Warn().Err(err).Str("proposal_id", p.ID).Msg("canary: open failed")
		return
	}
	w.Metrics.incOutcome("opened")
	w.Logger.Info().Str("proposal_id", p.ID).Str("swarm", swarm).Str("role", role).Str("knob", knob).
		Time("baseline_start", baselineStart).Time("window_until", c.WindowUntil).
		Bool("a1_baseline_sufficient", baseline.A1Sufficient).Msg("canary: opened")
}

// blastRadius nil-safely resolves the (swarm) → (projects, workflows) watch set.
func (w *CanaryGuardWorker) blastRadius(swarm string) (projectIDs, workflowIDs []string) {
	if w.BlastRadius == nil {
		return nil, nil
	}
	return w.BlastRadius(swarm)
}

// evaluate walks every open canary through the §4.5 trip sequence.
func (w *CanaryGuardWorker) evaluate(ctx context.Context) {
	open, err := w.Canaries.ListOpen(ctx)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("canary: list-open failed")
		return
	}
	for _, c := range open {
		w.evaluateOne(ctx, c)
	}
}

// evaluateOne runs the crash-safe §4.5 trip sequence for ONE open canary. Steps
// are numbered to the design. Every durable step (Rollback, canary finalize,
// cooldown) is idempotently completable; MarkRegressed is the one best-effort,
// possibly-lost step, and it always runs AFTER the durable canary write (C2).
func (w *CanaryGuardWorker) evaluateOne(ctx context.Context, c *persistence.CostTuningCanary) {
	now := w.now().UTC()

	// STEP 0 — FAILSAFE (F2). The ONLY exit for a canary the guard could never
	// evaluate (RefreshBetween erroring every tick, or stuck open past
	// window_until after prolonged downtime). Force-closes regardless of any
	// other condition so the detector is never frozen on it (§7). No cooldown.
	if now.Sub(c.OpenedAt) > w.maxCanaryAge() {
		_ = w.finalize(ctx, c, persistence.CanaryStatusInsufficientData,
			"max_canary_age failsafe: exceeded max age before evaluation completed", now)
		return
	}

	// STEP 1 — load proposal; recovery/conflation branch (F1).
	p, err := w.Proposals.GetByID(ctx, c.ProposalID)
	if err != nil {
		w.Logger.Warn().Err(err).Str("proposal_id", c.ProposalID).Msg("canary: proposal load failed; retry next tick")
		return
	}
	if p.Status != persistence.ProposalStatusApplied {
		// A rollback already happened. This fires for (a) a guard trip whose tick
		// crashed AFTER Rollback but BEFORE finalize, and (b) an OPERATOR manual
		// rollback while the canary was open. It does NOT fire for supersession —
		// a superseded original stays APPLIED (the ApplyEngine refuses to roll
		// back a target a later APPLIED proposal overwrote); that is handled live
		// at step 4 as `superseded`. Both (a) and (b) are DELIBERATELY finalized
		// regressed + cooldown + best-effort MarkRegressed: a knob rolled back
		// (by us or the operator) while under canary should not be immediately
		// re-proposed. The canary row carries no "who rolled back" field in v1,
		// so (a) and (b) are intentionally conflated; there is never a `regressed`
		// canary without a real rollback, so no false trip state — only a
		// labelling imprecision for the operator-initiated case. We do NOT
		// re-evaluate data. This is what makes step 2+ crash-safe.
		reason := "proposal no longer APPLIED while under canary — rolled back by the guard (crash-recovery) or the operator; cooling down the knob (design §4.5 step 1 / F1)"
		if ferr := w.finalize(ctx, c, persistence.CanaryStatusRegressed, reason, now); ferr == nil {
			w.bestEffortMarkRegressed(ctx, c.ProposalID, reason)
		}
		return
	}

	// STEP 2 — trip-time re-check of the kill-switch AND trading (I7 / §9). If
	// braked, leave the canary OPEN and do nothing this tick (no trip).
	if !w.enabled() || !w.swarmAllowed(c.SwarmID) || w.isTrading(c.SwarmID) {
		return
	}

	// STEP 3 — compute post window vs the row's captured baseline.
	postEnd := minTime(now, c.WindowUntil)
	postRep, err := w.Quality.RefreshBetween(ctx, c.AppliedAt, postEnd)
	if err != nil {
		// Transient read failure: leave open, retry next tick. Step-0 failsafe
		// bounds the total time a persistently-erroring canary stays open.
		w.Logger.Warn().Err(err).Str("proposal_id", c.ProposalID).Msg("canary: post-window read failed; retry next tick")
		return
	}
	postA1, _ := quality.PickRole(postRep, c.SwarmID, c.Role)
	if postA1.SampleCount < w.minSamples() {
		if now.Before(c.WindowUntil) {
			return // still filling the post window — wait
		}
		// Window over and still thin → insufficient_data (I1/I6). No cooldown;
		// the detector is NOT blocked (only `open` blocks, §7).
		_ = w.finalize(ctx, c, persistence.CanaryStatusInsufficientData,
			"post-window A1 samples below floor at window_until", now)
		return
	}

	// STEP 4 — trip predicates (§4.2). post A1 is min-sample-gated by step 3.
	trip, reason := w.detectTrip(ctx, c, postA1)
	if !trip {
		if !now.Before(c.WindowUntil) { // now >= window_until
			_ = w.finalize(ctx, c, persistence.CanaryStatusPassed, "no regression through the full window", now)
		}
		return // else keep watching
	}

	// TRIP — Rollback FIRST (the critical de-escalation), then the canary
	// finalize + cooldown in ONE durable row write, then the best-effort badge.
	rerr := w.Rollback(ctx, c.ProposalID)
	switch {
	case rerr == nil:
		// Canary write (status + cooldown) MUST precede the best-effort
		// MarkRegressed — that is the C2 fix. If the tick crashes between them,
		// the canary is already closed (not `open`), so EVALUATE never revisits
		// it: the cooldown is set (no flip-flop) and only the audit label is lost
		// (F3, a v1.1 badge-reconciliation sweep is the follow-up).
		if ferr := w.finalize(ctx, c, persistence.CanaryStatusRegressed, reason, now); ferr == nil {
			w.bestEffortMarkRegressed(ctx, c.ProposalID, reason)
		}
	case errors.Is(rerr, ErrApplyInProgress):
		// Another apply/rollback holds the global mutex — transient. Leave open,
		// retry next tick (no special backoff; ticks are ~15m).
		w.Logger.Info().Str("proposal_id", c.ProposalID).Msg("canary: rollback deferred (apply in progress); retry next tick")
	case errors.Is(rerr, ErrRollbackTargetDrifted), errors.Is(rerr, persistence.ErrProposalNotApplied):
		// A newer change now owns the locus — superseded. No cooldown.
		_ = w.finalize(ctx, c, persistence.CanaryStatusSuperseded,
			"rollback refused: target drifted or superseded by a later apply", now)
		w.Logger.Warn().Str("proposal_id", c.ProposalID).Err(rerr).Msg("canary: rollback superseded/drifted")
	default:
		// Unknown rollback failure: leave open, retry (failsafe bounds it).
		w.Logger.Error().Err(rerr).Str("proposal_id", c.ProposalID).Msg("canary: rollback failed unexpectedly; retry next tick")
	}
}

// detectTrip computes trip = A1_regress || effcost_regress || A2_regress (§4.2)
// and a human reason. post A1 is already min-sample-gated (step 3), so
// postSufficient is true here.
func (w *CanaryGuardWorker) detectTrip(ctx context.Context, c *persistence.CostTuningCanary, postA1 quality.ScoredSwarmRole) (bool, string) {
	a1 := a1Regress(c.Baseline, postA1.QualityRate, true, w.marginA1())
	cost := effcostRegress(c.Baseline, postA1.EffectiveCostTokens, true, w.marginCost())
	a2, a2Reason := w.a2Regress(ctx, c)
	if !a1 && !cost && !a2 {
		return false, ""
	}
	var parts []string
	if a1 {
		parts = append(parts, fmt.Sprintf("A1 quality regressed: post %.3f < baseline %.3f - %.3f", postA1.QualityRate, c.Baseline.A1Rate, w.marginA1()))
	}
	if cost {
		parts = append(parts, fmt.Sprintf("EffectiveCost regressed: post %.1f > baseline %.1f × (1+%.2f)", postA1.EffectiveCostTokens, c.Baseline.EffCost, w.marginCost()))
	}
	if a2 {
		parts = append(parts, a2Reason)
	}
	return true, strings.Join(parts, "; ")
}

// a2Regress is the conservative task-tier backstop (§4.2/§5): trip iff ANY
// blast-radius workflow with a sufficient baseline has its post window, split
// into A2Subwindows equal sub-windows, EVERY sub-window sufficient AND below
// baseline - MarginA2. Thin/insufficient workflows are ignored (never trip). A
// sub-window read error is treated conservatively as "no A2 trip".
func (w *CanaryGuardWorker) a2Regress(ctx context.Context, c *persistence.CostTuningCanary) (bool, string) {
	if len(c.Baseline.A2) == 0 || len(c.WorkflowIDs) == 0 {
		return false, ""
	}
	n := w.a2Subwindows()
	postEnd := minTime(w.now().UTC(), c.WindowUntil)
	total := postEnd.Sub(c.AppliedAt)
	if total <= 0 {
		return false, ""
	}
	subReports := make([]quality.Report, n)
	for i := 0; i < n; i++ {
		start := c.AppliedAt.Add(total * time.Duration(i) / time.Duration(n))
		end := c.AppliedAt.Add(total * time.Duration(i+1) / time.Duration(n))
		rep, err := w.Quality.RefreshBetween(ctx, start, end)
		if err != nil {
			w.Logger.Warn().Err(err).Str("proposal_id", c.ProposalID).Int("subwindow", i).Msg("canary: A2 sub-window read failed; no A2 trip")
			return false, ""
		}
		subReports[i] = rep
	}
	for _, wf := range c.WorkflowIDs {
		base, ok := c.Baseline.A2[wf]
		if !ok || !base.Sufficient {
			continue
		}
		subs := make([]a2Sub, n)
		for i := 0; i < n; i++ {
			s, found := quality.PickWorkflow(subReports[i], c.SwarmID, wf)
			subs[i] = a2Sub{Rate: s.QualityRate, Sufficient: found && s.SampleCount >= w.a2MinSamples()}
		}
		if a2WorkflowRegress(base, subs, w.marginA2()) {
			return true, fmt.Sprintf("A2 deliverable erosion on workflow %s (all %d sub-windows below baseline %.3f - %.3f)", wf, n, base.Rate, w.marginA2())
		}
	}
	return false, ""
}

// finalize writes the terminal canary status (+ reason + closed_at) in one row
// write and, on success, bumps the outcome metric. Returns the repo error so the
// caller only fires the best-effort MarkRegressed AFTER a durable canary write.
func (w *CanaryGuardWorker) finalize(ctx context.Context, c *persistence.CostTuningCanary, status, reason string, now time.Time) error {
	if err := w.Canaries.Finalize(ctx, c.ProposalID, status, reason, now); err != nil {
		w.Logger.Warn().Err(err).Str("proposal_id", c.ProposalID).Str("status", status).Msg("canary: finalize failed")
		return err
	}
	w.Metrics.incOutcome(status)
	w.Logger.Info().Str("proposal_id", c.ProposalID).Str("swarm", c.SwarmID).Str("role", c.Role).
		Str("status", status).Str("reason", reason).Msg("canary: finalized")
	return nil
}

// bestEffortMarkRegressed stamps the REGRESSED audit badge after the durable
// canary trip record. Its failure is logged and swallowed — trip-prevention +
// cooldown key off the canary row, so a lost badge never re-opens the trip (C2).
func (w *CanaryGuardWorker) bestEffortMarkRegressed(ctx context.Context, id, reason string) {
	if err := w.Proposals.MarkRegressed(ctx, id, reason); err != nil {
		w.Logger.Warn().Err(err).Str("proposal_id", id).
			Msg("canary: best-effort MarkRegressed badge failed; cooldown already set (C2 stays closed), audit label lost (F3)")
	}
}

// --- pure predicates (§4.2) — unit-testable without a DB ------------------

// a1Regress: baseline & post sufficient, post rate below baseline by > MarginA1.
func a1Regress(baseline persistence.CanaryBaseline, postRate float64, postSufficient bool, marginA1 float64) bool {
	return baseline.A1Sufficient && postSufficient && postRate < baseline.A1Rate-marginA1
}

// effcostRegress: baseline & post sufficient, non-zero baseline cost (zero-cost
// baseline is SKIPPED, not tripped — §4.1), post cost above baseline×(1+MarginCost).
func effcostRegress(baseline persistence.CanaryBaseline, postEffCost float64, postSufficient bool, marginCost float64) bool {
	return baseline.A1Sufficient && postSufficient && baseline.EffCost > 0 &&
		postEffCost > baseline.EffCost*(1+marginCost)
}

// a2Sub is one post sub-window's A2 score for a single workflow.
type a2Sub struct {
	Rate       float64
	Sufficient bool
}

// a2WorkflowRegress: baseline sufficient AND every sub-window sufficient AND
// below baseline - MarginA2 (the concrete "sustained" rule, I2). Any insufficient
// sub-window (thin series) means no trip.
func a2WorkflowRegress(base persistence.CanaryA2Series, subs []a2Sub, marginA2 float64) bool {
	if !base.Sufficient || len(subs) == 0 {
		return false
	}
	for _, s := range subs {
		if !s.Sufficient || s.Rate >= base.Rate-marginA2 {
			return false
		}
	}
	return true
}

// parseSwarmRoleEnvChange recovers (swarm, role, knob) from an APPLIED cost-tuning
// proposal's Evidence: {"change":{"kind":"swarm_role_env","swarm":..,"role":..,
// "key":..}}. ok=false when Evidence isn't that shape (the coverage-gap signal).
func parseSwarmRoleEnvChange(evidence string) (swarm, role, knob string, ok bool) {
	if strings.TrimSpace(evidence) == "" {
		return "", "", "", false
	}
	var env struct {
		Change *DiagnoseConfigChange `json:"change"`
	}
	if err := json.Unmarshal([]byte(evidence), &env); err != nil || env.Change == nil {
		return "", "", "", false
	}
	cc := env.Change
	if cc.Kind != swarmRoleEnvChangeKind || cc.Swarm == "" || cc.Role == "" || cc.Key == "" {
		return "", "", "", false
	}
	return cc.Swarm, cc.Role, cc.Key, true
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
