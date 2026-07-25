package controlplane

// cost_auto_apply_worker.go — Phase-4 cost/quality auto-apply (LLD
// 2026-07-24-cost-quality-auto-apply-design). A leader-gated worker that
// auto-approves + auto-acks + applies a DRAFT cost-quality-detector proposal
// ONLY when its (swarm,role,knob) is proven-safe:
//
//   - TRACK RECORD: >= K prior canary-PASSED applies (CountPassedForKnob), AND
//   - M=1 RE-SEED: the most-recent apply on the knob was HUMAN
//     (LastApplyActorForKnob != CostAutoApplyActor).
//
// M=1 is the load-bearing safety property (design §5.1): it forbids CHAINED
// auto-applies, so the §D canary I3 baseline clamp can never hide cumulative
// drift — every auto-apply is exactly one marginal step from a human-seeded
// baseline. This worker MUTATES, so it is leader-gated; it ships enabled:false,
// requires the canary rollback guard (D4), excludes trading twice, and treats an
// empty swarm allow-list as NONE (safety inversion — enforced by the container's
// SwarmAllowed closure and defended here by a nil-closure = deny).

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// CostAutoApplyWorker is the Phase-4 auto-apply worker. Deps are interfaces/funcs
// so the container wires real components and tests drive tick()/reconcile().
type CostAutoApplyWorker struct {
	Proposals persistence.ProposalRepository
	Canaries  persistence.CostTuningCanaryRepository
	// Apply is bound to ApplyEngine.Apply. The worker approves the proposal
	// (SetStatus) first, then calls this with ackDaemon=true (the auto-ack of the
	// swarm-scope gate, design D5). Never mutates the engine otherwise.
	Apply func(ctx context.Context, id, actor string, ackDaemon bool) error
	// ReadFile reads a deployed-config path relative to the config root — used to
	// stage the pre-apply snapshot before Apply writes (D8) and to detect the
	// crash signature (file == ApplyContent) at reconcile.
	ReadFile func(rel string) ([]byte, error)
	// Enabled is the live-reloadable kill-switch, re-checked at apply time (D5).
	// Nil ⇒ disabled.
	Enabled func() bool
	// CanaryEnabled is the hard prerequisite (D4): no auto-apply without the
	// rollback net. Nil ⇒ treated as disabled (fail-safe).
	CanaryEnabled func() bool
	// SwarmAllowed is the per-swarm allow-list. For auto-apply the container's
	// closure encodes EMPTY = NONE; a nil closure here denies (safety inversion vs
	// the canary guard's nil = allow).
	SwarmAllowed func(swarm string) bool
	// IsTradingSwarm excludes trading at scan AND apply time (defense in depth;
	// (b) also refuses at the engine). Nil ⇒ never trading.
	IsTradingSwarm func(swarm string) bool

	MinPassedCanaries int
	// CooldownDuration matches the canary guard's cooldown so the worker's
	// cooldown skip agrees with the detector's (design D7). 0 ⇒ no cooldown skip
	// (the detector's own skip is the primary guard).
	CooldownDuration time.Duration
	Interval         time.Duration

	LeaderGate LeaderGate
	Metrics    *CostAutoApplyMetrics
	Now        func() time.Time
	Logger     zerolog.Logger

	stopped    chan struct{}
	reconciled bool
}

func (w *CostAutoApplyWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
func (w *CostAutoApplyWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return 15 * time.Minute
}
func (w *CostAutoApplyWorker) minPassed() int {
	if w.MinPassedCanaries > 0 {
		return w.MinPassedCanaries
	}
	return 2
}
func (w *CostAutoApplyWorker) enabled() bool { return w.Enabled != nil && w.Enabled() }
func (w *CostAutoApplyWorker) canaryEnabled() bool {
	return w.CanaryEnabled != nil && w.CanaryEnabled()
}
func (w *CostAutoApplyWorker) isTrading(s string) bool {
	return w.IsTradingSwarm != nil && w.IsTradingSwarm(s)
}

// swarmAllowed enforces the safety inversion: a nil closure DENIES (unlike the
// canary guard, where nil = allow). The container's real closure encodes
// empty-allow-list = NONE.
func (w *CostAutoApplyWorker) swarmAllowed(s string) bool {
	return w.SwarmAllowed != nil && w.SwarmAllowed(s)
}

// Run drives the periodic loop until ctx is cancelled. Leader-gated (it MUTATES).
// On the first leader pass it runs the crash reconciler once (D8) before the scan.
func (w *CostAutoApplyWorker) Run(ctx context.Context) {
	if w == nil || w.Proposals == nil || w.Canaries == nil || w.Apply == nil || w.ReadFile == nil {
		return
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)
	w.Logger.Info().Dur("interval", w.interval()).Int("min_passed_canaries", w.minPassed()).
		Msg("control-plane cost-auto-apply worker started")
	defer w.Logger.Info().Msg("control-plane cost-auto-apply worker stopped")

	leaderTick := func() {
		if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
			return
		}
		if !w.reconciled {
			w.reconcile(ctx) // D8; idempotent (file==ApplyContent gate), safe on any re-election
			w.reconciled = true
		}
		w.tick(ctx)
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(initialScanDelay):
		leaderTick()
	}
	t := time.NewTicker(w.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			leaderTick()
		}
	}
}

// Stopped returns a channel closed when Run exits (test sync).
func (w *CostAutoApplyWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

// tick runs one scan pass. No-op unless BOTH the auto-apply kill-switch and the
// canary guard (D4 prerequisite) are live.
func (w *CostAutoApplyWorker) tick(ctx context.Context) {
	if !w.enabled() {
		return
	}
	if !w.canaryEnabled() {
		w.Logger.Warn().Msg("cost-auto-apply: canary guard disabled; auto-apply refused (design D4 — no rollback net)")
		return
	}
	drafts, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		Statuses: []string{persistence.ProposalStatusDraft},
	})
	if err != nil {
		w.Logger.Warn().Err(err).Msg("cost-auto-apply: list drafts failed")
		return
	}
	pending, err := w.pendingDiscoveryLoci(ctx) // F7: (swarm,role) with an APPLIED proposal awaiting canary discovery
	if err != nil {
		w.Logger.Warn().Err(err).Msg("cost-auto-apply: pending-discovery safety check failed; skipping scan")
		return
	}
	for _, p := range drafts {
		if p == nil || p.ProposedBy != costQualityDetectorProposedBy {
			continue
		}
		swarm, role, knob, ok := parseSwarmRoleEnvChange(p.Evidence)
		if !ok {
			continue
		}
		if w.isTrading(swarm) || !w.swarmAllowed(swarm) {
			w.Metrics.inc("skipped_disallowed")
			continue
		}
		// Locus busy: an open canary OR an APPLIED proposal on (swarm,role) whose
		// canary hasn't been discovered yet — don't chain baselines (F7).
		open, oerr := w.Canaries.HasOpenForSwarmRole(ctx, swarm, role)
		if oerr != nil {
			w.Logger.Warn().Err(oerr).Str("swarm", swarm).Str("role", role).
				Msg("cost-auto-apply: open-canary safety check failed; skipping proposal")
			continue
		}
		if open || pending[swarm+"\x00"+role] {
			w.Metrics.inc("skipped_locus")
			continue
		}
		if w.CooldownDuration > 0 {
			cooling, cerr := w.Canaries.HasActiveCooldown(ctx, swarm, role, knob, w.now().Add(-w.CooldownDuration))
			if cerr != nil {
				w.Logger.Warn().Err(cerr).Str("swarm", swarm).Str("role", role).Str("knob", knob).
					Msg("cost-auto-apply: cooldown safety check failed; skipping proposal")
				continue
			}
			if cooling {
				w.Metrics.inc("skipped_cooldown")
				continue
			}
		}
		// TRUST (D1): track record + M=1 re-seed.
		passed, perr := w.Canaries.CountPassedForKnob(ctx, swarm, role, knob)
		if perr != nil {
			w.Logger.Warn().Err(perr).Str("swarm", swarm).Str("role", role).Str("knob", knob).
				Msg("cost-auto-apply: count-passed query failed")
			continue
		}
		if passed < w.minPassed() {
			w.Metrics.inc("skipped_untrusted")
			continue
		}
		actor, hasHistory, aerr := w.Canaries.LastApplyActorForKnob(ctx, swarm, role, knob)
		if aerr != nil {
			w.Logger.Warn().Err(aerr).Msg("cost-auto-apply: last-apply-actor query failed")
			continue
		}
		if !hasHistory || actor == persistence.CostAutoApplyActor {
			// M=1: the knob was last auto-applied (or has no history) — wait for a
			// human to re-seed before another machine apply. No chain, no drift.
			w.Metrics.inc("skipped_m1")
			w.Logger.Debug().Str("proposal_id", p.ID).Str("swarm", swarm).Str("role", role).Str("knob", knob).
				Bool("has_history", hasHistory).Str("last_apply_actor", actor).
				Msg("cost-auto-apply: M=1 skip — most-recent apply not human (awaiting re-seed)")
			continue
		}
		w.autoApply(ctx, p, swarm, role, knob)
	}
}

// pendingDiscoveryLoci returns the set of (swarm\x00role) keys that have an
// APPLIED cost-tuning proposal with no canary row yet (F7 pending-discovery).
func (w *CostAutoApplyWorker) pendingDiscoveryLoci(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	applied, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		Statuses: []string{persistence.ProposalStatusApplied},
	})
	if err != nil {
		return nil, err
	}
	for _, ap := range applied {
		if ap == nil || ap.ProposedBy != costQualityDetectorProposedBy {
			continue
		}
		s, r, _, ok := parseSwarmRoleEnvChange(ap.Evidence)
		if !ok {
			continue
		}
		if _, gerr := w.Canaries.GetByProposalID(ctx, ap.ID); errors.Is(gerr, persistence.ErrNotFound) {
			out[s+"\x00"+r] = true
		} else if gerr != nil {
			return nil, gerr
		}
	}
	return out, nil
}

// autoApply performs the D5 sequence for a trusted DRAFT: re-check the live
// brakes, approve, stage the pre-image snapshot (D8), then Apply(ack=true). Any
// Apply failure REJECTs the proposal (terminal, D6) — the detector re-files a
// fresh DRAFT next scan (the applied value never landed), so there is no retry
// loop and no permanent loss.
func (w *CostAutoApplyWorker) autoApply(ctx context.Context, p *persistence.ControlPlaneProposal, swarm, role, knob string) {
	// Live re-check at apply time (D5): a mid-tick brake suppresses this apply.
	if !w.enabled() || !w.canaryEnabled() || w.isTrading(swarm) || !w.swarmAllowed(swarm) {
		w.Metrics.inc("braked")
		return
	}
	if err := w.Proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, persistence.CostAutoApplyActor); err != nil {
		// Racing decision (operator approved/rejected first) — leave it alone.
		w.Logger.Info().Err(err).Str("proposal_id", p.ID).Msg("cost-auto-apply: approve raced; skipping")
		return
	}
	// D8: stage the pre-image BEFORE Apply writes any file, so a crash in the
	// atomicWrite→MarkApplied window is recoverable by reconcile().
	content, rerr := w.ReadFile(p.ApplyTarget)
	if rerr != nil {
		w.Logger.Warn().Err(rerr).Str("proposal_id", p.ID).Str("target", p.ApplyTarget).
			Msg("cost-auto-apply: read target for snapshot failed; rejecting")
		w.reject(ctx, p.ID)
		return
	}
	if serr := w.Proposals.StagePreApplySnapshot(ctx, p.ID, string(content)); serr != nil {
		w.Logger.Warn().Err(serr).Str("proposal_id", p.ID).Msg("cost-auto-apply: stage snapshot failed; rejecting")
		w.reject(ctx, p.ID)
		return
	}
	if aerr := w.Apply(ctx, p.ID, persistence.CostAutoApplyActor, true); aerr != nil {
		// D6: any Apply error is terminal for the worker. REJECT so it leaves the
		// APPROVED set (no false "completed auto-approval", no locus race); the
		// detector re-files if the signal persists.
		w.reject(ctx, p.ID)
		w.Metrics.inc("rejected")
		w.Logger.Warn().Err(aerr).Str("proposal_id", p.ID).Str("swarm", swarm).Str("role", role).Str("knob", knob).
			Msg("cost-auto-apply: apply failed; proposal rejected (detector will re-file if the signal persists)")
		return
	}
	w.Metrics.inc("applied")
	w.Logger.Info().Str("proposal_id", p.ID).Str("swarm", swarm).Str("role", role).Str("knob", knob).
		Msg("cost-auto-apply: proposal auto-applied (>=K passed canaries, M=1 re-seed); canary guard will watch it")
}

func (w *CostAutoApplyWorker) reject(ctx context.Context, id string) {
	if err := w.Proposals.SetStatus(ctx, id, persistence.ProposalStatusRejected, persistence.CostAutoApplyActor); err != nil {
		w.Logger.Warn().Err(err).Str("proposal_id", id).Msg("cost-auto-apply: reject failed")
	}
}

// reconcile is the D8 startup recovery: for each APPROVED cost-tuning proposal we
// pre-staged a snapshot on whose on-disk target already equals ApplyContent (the
// atomicWrite-succeeded-but-MarkApplied-never-ran crash signature), complete the
// apply forward so the §D canary guard opens a canary (the safety net engages)
// and the staged snapshot is present for any later rollback. A file that does NOT
// match ApplyContent means the write never completed → left cleanly APPROVED.
// Idempotent: a completed proposal becomes APPLIED and is no longer in the
// APPROVED set, so re-running (on any leader re-election) is safe.
func (w *CostAutoApplyWorker) reconcile(ctx context.Context) {
	approved, err := w.Proposals.List(ctx, persistence.ProposalListFilter{
		Statuses: []string{persistence.ProposalStatusApproved},
	})
	if err != nil {
		w.Logger.Warn().Err(err).Msg("cost-auto-apply: reconcile list approved failed")
		return
	}
	for _, p := range approved {
		if p == nil || p.ProposedBy != costQualityDetectorProposedBy || p.PreApplySnapshot == "" {
			continue // only proposals WE staged a snapshot on can be crash-stranded by us
		}
		content, rerr := w.ReadFile(p.ApplyTarget)
		if rerr != nil {
			continue
		}
		if string(content) != p.ApplyContent {
			continue // write never completed → proposal is cleanly APPROVED, nothing to recover
		}
		if merr := w.Proposals.MarkApplied(ctx, p.ID, persistence.CostAutoApplyActor, p.PreApplySnapshot); merr != nil {
			w.Logger.Warn().Err(merr).Str("proposal_id", p.ID).Msg("cost-auto-apply: reconcile complete-forward failed")
			continue
		}
		w.Metrics.inc("reconciled")
		w.Logger.Warn().Str("proposal_id", p.ID).Str("target", p.ApplyTarget).
			Msg("cost-auto-apply: completed a crash-stranded apply (on-disk file matched ApplyContent); canary guard will watch it")
	}
}
