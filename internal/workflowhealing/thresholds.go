package workflowhealing

// Promotion-gate thresholds sourced from the per-(project, workflow,
// class) overrides repo — Self-Healing Workflow Genome v1 (LLD
// § Promotion Gate). The detector arc shipped workflow_healing_overrides
// (migration 81) carrying a per-tuple ThresholdOverride + MutedUntil.
// The gate REUSES that knob (per the build mandate) as the
// success-uplift override: when an operator has set a custom relative
// threshold for a (project, workflow, class) tuple, that value becomes
// the required success uplift for the candidate spawned from that
// trigger. Cost/latency/hallucination/verifier tolerances are not yet
// carried on the override row, so they fall back to the documented v1
// defaults (DefaultGateThresholds).
//
// This file is the ONLY place that reads the overrides repo for gate
// purposes; the gate itself (gate.go) stays pure I/O-free so it remains
// trivially unit-testable. ResolveGateThresholds does the lookup and
// hands a fully-built GateThresholds to NewTrialRunner.

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GateThresholdResolver builds promotion-gate thresholds for a
// (project, workflow, class) tuple from the overrides repo, falling
// back to documented defaults when the repo is absent or the tuple has
// no row. It is nil-safe at every level so a deployment without the
// overrides repo wired still gets the conservative defaults.
type GateThresholdResolver struct {
	overrides persistence.HealingTriggerOverrideRepository
	log       zerolog.Logger
}

// NewGateThresholdResolver constructs a resolver. A nil overrides repo
// is allowed — Resolve then always returns DefaultGateThresholds.
func NewGateThresholdResolver(overrides persistence.HealingTriggerOverrideRepository, log zerolog.Logger) *GateThresholdResolver {
	return &GateThresholdResolver{overrides: overrides, log: log}
}

// Resolve returns the gate thresholds for the given tuple. The lookup
// order is:
//
//  1. Start from DefaultGateThresholds (conservative v1 baseline).
//  2. If the overrides repo carries a row for (project, workflow,
//     class) with a non-nil ThresholdOverride, use it as the required
//     SuccessUplift — an operator who raised the detector's regression
//     threshold is implicitly asking the gate to demand at least that
//     much uplift before promotion.
//
// Cost/latency/hallucination/verifier tolerances are not represented on
// the override row in v1, so they keep their defaults. A repo error is
// logged and swallowed — the gate must never fail-open on a transient
// lookup error, so it falls back to the (stricter) defaults.
//
// A missing repo or a not-found row both yield DefaultGateThresholds.
func (r *GateThresholdResolver) Resolve(ctx context.Context, projectID, workflowID string, class persistence.HealingTriggerClass) GateThresholds {
	g := DefaultGateThresholds()
	if r == nil || r.overrides == nil {
		return g
	}

	ov, err := r.overrides.Get(ctx, projectID, workflowID, class)
	if err != nil {
		// ErrNotFound is the common case (no override configured); any
		// other error is logged. Either way we keep the safe defaults.
		if !isNotFound(err) {
			r.log.Warn().Err(err).
				Str("project_id", projectID).
				Str("workflow_id", workflowID).
				Str("class", string(class)).
				Msg("workflowhealing: gate-threshold override lookup failed; using defaults")
		}
		return g
	}

	if ov != nil && ov.ThresholdOverride != nil {
		// The override is stored as a relative delta (e.g. 0.50 = "50%
		// lift"); reuse it directly as the required success uplift.
		g.SuccessUplift = *ov.ThresholdOverride
		g = g.WithConfigured()
		r.log.Debug().
			Str("project_id", projectID).
			Str("workflow_id", workflowID).
			Str("class", string(class)).
			Float64("success_uplift", g.SuccessUplift).
			Msg("workflowhealing: gate success-uplift sourced from overrides repo")
	}

	return g
}

// isNotFound reports whether err is the persistence not-found sentinel.
// Pulled out so Resolve reads cleanly and the import set stays minimal.
func isNotFound(err error) bool {
	return errors.Is(err, persistence.ErrNotFound)
}
