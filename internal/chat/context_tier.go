package chat

// ContextTier classifies a session's prompt-budget headroom into four
// bands. Surfaced to the dispatcher so it can degrade gracefully
// (force tool deferred loading, suggest /summarize) before context
// exhaustion produces a hard truncation or a runaway loop.
//
// Bands match the four-tier context-degradation pattern observed
// across recent agent-framework research. Boundaries are
// percentage-of-window:
//
//	PEAK      < 50%   — plenty of headroom; defaults apply.
//	GOOD      50-75%  — comfortable; defaults still apply.
//	DEGRADING 75-90%  — warning band; consumers should drop optional
//	                    payload (e.g. force deferred tool loading).
//	POOR      ≥ 90%   — emergency band; consumers should aggressively
//	                    trim history, refuse new tool expansions.
//
// The thresholds are part of the public API of this package — tests
// pin them so a future tuning is a deliberate change rather than a
// silent drift.
type ContextTier int

const (
	TierPeak ContextTier = iota
	TierGood
	TierDegrading
	TierPoor
)

// Threshold percentages for the four tiers. Exported so callers can
// surface the same numbers in the UI without re-deriving them.
const (
	TierGoodPct      = 50
	TierDegradingPct = 75
	TierPoorPct      = 90
)

// TierFromUsage returns the context tier for a session that has used
// `used` tokens against a budget of `limit` tokens. limit ≤ 0 (no
// configured budget) returns TierPeak — we have no signal to degrade
// on. used ≤ 0 also returns TierPeak.
func TierFromUsage(used, limit int) ContextTier {
	if limit <= 0 || used <= 0 {
		return TierPeak
	}
	pct := float64(used) / float64(limit) * 100.0
	switch {
	case pct < TierGoodPct:
		return TierPeak
	case pct < TierDegradingPct:
		return TierGood
	case pct < TierPoorPct:
		return TierDegrading
	default:
		return TierPoor
	}
}

// HeadroomPct returns the percentage of the budget still available
// (100 - usedPct), clamped to [0, 100]. Used by the histogram that
// observes per-turn headroom — operators tune the tier thresholds off
// the histogram's distribution. limit ≤ 0 yields 100 ("no signal" =
// full headroom by convention; matches TierFromUsage's PEAK fallback).
// used ≤ 0 also yields 100. used > limit yields 0 (overshoot clamps
// to "no headroom left").
func HeadroomPct(used, limit int) float64 {
	if limit <= 0 || used <= 0 {
		return 100.0
	}
	pct := float64(used) / float64(limit) * 100.0
	headroom := 100.0 - pct
	if headroom < 0 {
		// Used > limit (overshoot) — clamp to "no headroom left".
		// The upper bound (>100) can't be reached given the input
		// guards above, so we don't bother with that branch.
		return 0
	}
	return headroom
}

// IsDegraded reports whether the tier is DEGRADING or worse — the
// signal consumers use to switch to defensive behaviour (force
// deferred tool loading, refuse expansion, suggest /summarize).
func (t ContextTier) IsDegraded() bool {
	return t >= TierDegrading
}

// String returns the canonical lowercase name. Used in response
// metadata + structured logs.
func (t ContextTier) String() string {
	switch t {
	case TierPeak:
		return "peak"
	case TierGood:
		return "good"
	case TierDegrading:
		return "degrading"
	case TierPoor:
		return "poor"
	default:
		return "unknown"
	}
}
