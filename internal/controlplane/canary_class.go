package controlplane

// canary_class.go — the canary CLASS REGISTRY (LLD
// 2026-08-10-canary-class-registry-step-outcome-design §4).
//
// Before this, the guard decided what to watch with two package constants
// (`costQualityDetectorProposedBy`, `swarmRoleEnvChangeKind`), which meant the
// tune-detector's `workflow_step_timeout` proposals were excluded on BOTH axes.
// That is why the 2026-08-10 easeit-companion ingest regression ran unwatched
// even though the rollback machinery it needed was already shipped and running.
//
// SCOPE OF THIS SEAM. The interface abstracts DISCOVERY — which proposals a
// class owns, the identity it stores on the canary row, and whether a pass may
// close the canary early. It deliberately does NOT yet abstract baseline
// capture or trip detection: `persistence.CostTuningCanary` hard-codes a
// (SwarmID, Role, Knob) locus and `CanaryBaseline` is quality-specific (A1 rate
// / EffCost / A2 map), so a second class with a (project, workflow, step) locus
// and step-outcome rates needs a schema change first. Abstracting evaluation
// before that data shape exists would be speculation, and the shipped cost path
// is live — see the design's §11.5 for the constraints that bind Class B when
// its schema lands.

import "vornik.io/vornik/internal/persistence"

// CanaryClass is one kind of applied change the guard knows how to watch.
type CanaryClass interface {
	// Name identifies the class in logs, metrics and the canary row.
	Name() string
	// Matches reports whether this class owns the proposal. Implementations
	// must gate on BOTH the proposing identity and the Evidence change kind:
	// either alone would let a foreign proposal into a watcher that cannot
	// evaluate it.
	Matches(p *persistence.ControlPlaneProposal) bool
	// Locus extracts the identity triple persisted on the canary row. ok=false
	// means the Evidence did not carry a usable change, which discovery treats
	// as a coverage gap (a stringly-typed contract drift made visible) rather
	// than opening a canary on empty identity.
	Locus(p *persistence.ControlPlaneProposal) (swarm, role, knob string, ok bool)
}

// NOTE ON DESIGN D1 ("Class B never closes early on a pass"). Verified against
// canary_guard.go:445 — the shipped guard finalizes `passed` ONLY when
// `now >= window_until`; there is no early-close-on-pass path. D1 is therefore
// already the behaviour for every class and needs no interface method. The
// design's D1 and review-20260810-53f0 finding 2 both assumed an early close
// that does not exist in code; corrected in the design's §11.6.

// defaultCanaryClasses is the registry, most-specific first. `canaryClassFor`
// returns the first match, so ordering is the tie-break if two predicates ever
// overlap.
func defaultCanaryClasses() []CanaryClass {
	return []CanaryClass{costQualityCanaryClass{}}
}

// canaryClassFor returns the class owning p, or nil when no class does. A nil
// result is a legitimate state, not an error: it means the guard has no
// evaluator for this change kind and must not open a canary it cannot judge.
func canaryClassFor(classes []CanaryClass, p *persistence.ControlPlaneProposal) CanaryClass {
	if p == nil {
		return nil
	}
	for _, c := range classes {
		if c.Matches(p) {
			return c
		}
	}
	return nil
}

// costQualityCanaryClass is the shipped cost/quality watcher (§D), lifted behind
// the interface with its predicate and locus parse unchanged. Its behaviour is
// pinned by the pre-existing canary_guard_test.go suite, which must pass
// unmodified — that suite is the regression gate on this refactor
// (review-20260810-53f0 finding 8).
type costQualityCanaryClass struct{}

func (costQualityCanaryClass) Name() string { return "cost_quality" }

func (costQualityCanaryClass) Matches(p *persistence.ControlPlaneProposal) bool {
	if p == nil || p.ProposedBy != costQualityDetectorProposedBy {
		return false
	}
	_, _, _, ok := parseSwarmRoleEnvChange(p.Evidence)
	return ok
}

func (costQualityCanaryClass) Locus(p *persistence.ControlPlaneProposal) (string, string, string, bool) {
	if p == nil {
		return "", "", "", false
	}
	return parseSwarmRoleEnvChange(p.Evidence)
}
