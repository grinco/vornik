package controlplane

// costquality_worker.go — the Phase-2 prompt_token_budget detector (LLD
// 2026-07-21-cost-quality-tuning-loop §B). Periodically reads the two-tier
// quality read-model + per-(swarm,role) prompt-token percentiles, and for each
// runaway-tail-on-quality-sufficient locus files an APPLYABLE `config` DRAFT
// proposal (via the shared fileRenderedProposal path) that clamps
// VORNIK_STEP_PROMPT_TOKEN_BUDGET. Propose-only + human-gated; default OFF.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/quality"
)

const promptTokenBudgetEnvKey = "VORNIK_STEP_PROMPT_TOKEN_BUDGET"

// qualityReporter / percentileReader are the read-model seams (satisfied by
// *quality.Service and *postgres.QualityRepository; faked in tests).
type qualityReporter interface {
	Refresh(ctx context.Context, since time.Time) (quality.Report, error)
}
type percentileReader interface {
	RolePercentiles(ctx context.Context, since time.Time, projectIDs, swarmIDs []string) ([]quality.SwarmRolePercentile, error)
}

// CostQualityWorker emits prompt-token-budget proposals. Deps are interfaces/
// funcs so the container wires real components and tests drive tick() in-memory.
type CostQualityWorker struct {
	Quality     qualityReporter
	Percentiles percentileReader
	Actionize   *Actionizer
	Proposals   persistence.ProposalRepository
	// SwarmMap returns parallel (projectIDs, swarmIDs) from the registry,
	// EXCLUDING trading/broker swarms (trading-path exclusion, design §F).
	SwarmMap func() (projectIDs, swarmIDs []string)

	// Canaries lets the detector skip a (swarm,role) with an OPEN canary or a
	// (swarm,role,knob) still in cooldown after a regressed rollback (design §7).
	// Nil ⇒ no skip (back-compat: the canary guard isn't wired). CooldownDuration
	// is the guard's cooldown window (matches control_plane.cost_tuning_canary.
	// cooldown) so the skip's notBefore cutoff agrees with the guard.
	Canaries         persistence.CostTuningCanaryRepository
	CooldownDuration time.Duration

	Enabled       bool
	Interval      time.Duration
	Window        time.Duration
	MinP95Tokens  int64
	TailFactor    float64
	Margin        float64
	MinChangeFrac float64
	Logger        zerolog.Logger
}

// canaryBlocks reports whether an open canary on (swarm,role) or an active
// cooldown on (swarm,role,knob) suppresses a fresh proposal (design §7). Nil
// Canaries ⇒ never blocks. A query error is logged and treated as NOT blocking —
// the detector's proposals are human-gated + re-canaried, so failing open here
// is safe and avoids freezing the detector on a transient DB hiccup.
func (w *CostQualityWorker) canaryBlocks(ctx context.Context, swarm, role, knob string) bool {
	if w.Canaries == nil {
		return false
	}
	if open, err := w.Canaries.HasOpenForSwarmRole(ctx, swarm, role); err != nil {
		w.Logger.Warn().Err(err).Str("swarm", swarm).Str("role", role).Msg("cost-quality: open-canary skip check failed")
	} else if open {
		return true
	}
	cooldown := w.CooldownDuration
	if cooldown <= 0 {
		cooldown = 336 * time.Hour
	}
	if cooling, err := w.Canaries.HasActiveCooldown(ctx, swarm, role, knob, time.Now().Add(-cooldown)); err != nil {
		w.Logger.Warn().Err(err).Str("swarm", swarm).Str("role", role).Msg("cost-quality: cooldown skip check failed")
	} else if cooling {
		return true
	}
	return false
}

func (w *CostQualityWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return time.Hour
}
func (w *CostQualityWorker) window() time.Duration {
	if w.Window > 0 {
		return w.Window
	}
	return 7 * 24 * time.Hour
}
func (w *CostQualityWorker) tailFactor() float64 {
	if w.TailFactor > 0 {
		return w.TailFactor
	}
	return 1.5
}
func (w *CostQualityWorker) margin() float64 {
	if w.Margin > 0 {
		return w.Margin
	}
	return 0.2
}
func (w *CostQualityWorker) minChangeFrac() float64 {
	if w.MinChangeFrac > 0 {
		return w.MinChangeFrac
	}
	return 0.1
}
func (w *CostQualityWorker) minP95Tokens() int64 {
	if w.MinP95Tokens > 0 {
		return w.MinP95Tokens
	}
	return 100_000 // below this p95, a role isn't a token hog worth clamping
}

func costQualityBudgetTitle(swarm, role string) string {
	return fmt.Sprintf("cost/quality: prompt-token budget — %s/%s", swarm, role)
}

// initialScanDelay lets the DB/registry warm before the first scan so a
// boot-time tick doesn't race daemon startup.
const initialScanDelay = 30 * time.Second

// Run does one initial scan (after a short settle) then ticks on Interval until
// ctx is cancelled. The initial scan surfaces proposals without waiting a full
// interval. tick() itself is a no-op while disabled, so the gate is honored on
// every pass.
func (w *CostQualityWorker) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialScanDelay):
		w.tick(ctx)
	}
	t := time.NewTicker(w.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick runs one detection pass: quality + percentiles → decision → applyable
// proposal per qualifying (swarm, role).
func (w *CostQualityWorker) tick(ctx context.Context) {
	if !w.Enabled {
		return
	}
	projIDs, swarmIDs := w.SwarmMap()
	if len(projIDs) == 0 {
		return
	}
	since := time.Now().Add(-w.window())

	report, err := w.Quality.Refresh(ctx, since)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("cost-quality: quality refresh failed")
		return
	}
	sufficient := make(map[[2]string]bool, len(report.Steps))
	for _, s := range report.Steps {
		sufficient[[2]string{s.Swarm, s.Role}] = s.Sufficient
	}

	pcts, err := w.Percentiles.RolePercentiles(ctx, since, projIDs, swarmIDs)
	if err != nil {
		w.Logger.Warn().Err(err).Msg("cost-quality: percentile read failed")
		return
	}
	for _, p := range pcts {
		d := quality.DecidePromptTokenBudget(quality.BudgetDetectorInput{
			P95PromptTokens:   p.P95,
			P99PromptTokens:   p.P99,
			QualitySufficient: sufficient[[2]string{p.Swarm, p.Role}],
			MinP95Tokens:      w.minP95Tokens(),
			TailFactor:        w.tailFactor(),
			Margin:            w.margin(),
			MinChangeFrac:     w.minChangeFrac(),
		})
		if !d.ShouldPropose {
			continue
		}
		// Skip while a canary is open on this (swarm,role) or the knob is cooling
		// after a regressed rollback (design §7). A manual operator apply
		// mid-cooldown bypasses this (it's discovery-based), but the detector
		// itself holds off.
		if w.canaryBlocks(ctx, p.Swarm, p.Role, promptTokenBudgetEnvKey) {
			continue
		}
		rc, rerr := w.Actionize.RenderRoleEnv(p.Swarm, p.Role, promptTokenBudgetEnvKey, strconv.FormatInt(d.ProposedBudget, 10))
		if rerr != nil {
			if rerr != ErrChangeNotUseful {
				w.Logger.Warn().Err(rerr).Str("swarm", p.Swarm).Str("role", p.Role).Msg("cost-quality: render failed")
			}
			continue
		}
		title := costQualityBudgetTitle(p.Swarm, p.Role)
		rationale := fmt.Sprintf(
			"Runaway prompt-token tail on %s/%s: p95=%d, p99=%d tokens/step (n=%d, p99≥%.1f×p95). "+
				"Propose per-step budget %d (p95×%.2f) to clamp the tail; A1 quality is sufficient. "+
				"Applies to ALL projects sharing this swarm.",
			p.Swarm, p.Role, p.P95, p.P99, p.N, w.tailFactor(), d.ProposedBudget, 1+w.margin())
		evidence := fmt.Sprintf(
			`{"signal":"prompt_token_runaway","swarm":%q,"role":%q,"p95":%d,"p99":%d,"n":%d,"proposed_budget":%d}`,
			p.Swarm, p.Role, p.P95, p.P99, p.N, d.ProposedBudget)
		// ProjectID "" — the change is swarm-scoped (BlastRadius=swarm from rc),
		// not owned by a single project; dedup is by the (swarm,role) title.
		fileRenderedProposal(ctx, w.Proposals, w.Logger, "", title, rationale, evidence, costQualityDetectorProposedBy, rc)
	}
}
