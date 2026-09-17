package memory

import (
	"math"
	"sort"
	"time"
)

// Retrieval recency — the freshness term in ranking.
// https://docs.vornik.io
//
// The incident this exists for: two `research` chunks of the same recurring
// digest series, 11 days apart, and the STALE one won an undated query because
// nothing in the ranking pipeline could tell them apart by age. Trust-verdict
// freshness (routing.go) looks like it should have caught it and cannot — it
// is a per-query CONFIDENCE signal computed over the top-K, not a per-chunk
// ranking factor, and its no-TTL age cap has a 0-180 day blind window (§4).
// So this is a second, separate freshness notion by design, not a duplicate.
//
// This file is the pure-Go foundation: the curve, its constants, and the
// config. Applying it to a result set (§5.5) and the series demotion (§5.3)
// land separately.

const (
	// rrfConstantK mirrors the k=60 hardcoded in the reciprocal-rank-fusion
	// expressions in repository.go (`1.0/(60 + rank)`, three search paths).
	// It is duplicated here rather than shared because the SQL cannot read a
	// Go constant; §5.3b's whole point is that the floor must be DERIVED
	// from these two numbers rather than copied from an old calculation, so
	// if the SQL's k ever moves, this is the one place that has to move
	// with it and the floor follows automatically.
	rrfConstantK = 60

	// perArmCandidateCap is the per-arm `LIMIT 20` the search CTEs apply
	// before RRF fusion — i.e. before any Go-side factor exists. Note that
	// rag-retrieval-and-graph-design.md §4.1-4.2 specifies a configurable
	// candidate_pool_size defaulting to 50; that is NOT what shipped, and
	// this slice deliberately follows the code rather than the design
	// (§5.5 records the divergence). Read from repository.go :1534, :1546,
	// :1597, :1612, :1692, :1710.
	perArmCandidateCap = 20

	// minFreshnessSafetyMargin puts the computed floor just ABOVE the
	// dominance threshold it guards (§5.3b). Just above, not comfortably
	// above: the contract is that age reorders comparable results and never
	// overrides a decisive relevance gap, and a large margin would buy
	// nothing except a weaker freshness signal.
	minFreshnessSafetyMargin = 1.05

	// minFreshnessFloorBound / minFreshnessCeilBound clamp the computed
	// floor. The lower bound is also what reserves 0.0 as the "unset"
	// sentinel for RecencyConfig.MinFreshness: no legitimate value, computed
	// or operator-supplied, can land on 0.
	minFreshnessFloorBound = 0.05
	minFreshnessCeilBound  = 0.95
)

// RecencyConfig tunes the freshness term and the recurring-series demotion
// (§5.6). Defaults live beside RetrievalRoutingConfig's and follow the same
// applyDefaults pattern — including its distinction between "explicitly set
// false" and "unset", which matters here for exactly one field and matters a
// lot: Enabled is a kill switch, so false is the value an operator most wants
// to express and a bare zero-check would make it unexpressible.
type RecencyConfig struct {
	// Enabled is the master switch. False restores byte-identical
	// pre-change ranking INCLUDING the over-fetch (the SQL LIMIT reverts),
	// so the switch is a true revert rather than a partial one. Default
	// true; wire operator config through SetEnabled, not the bare field.
	Enabled bool
	// enabledSet records whether Enabled was explicitly wired, so
	// applyDefaults can tell "left false" from "unset". Mirrors
	// RetrievalRoutingConfig.enabledSet.
	enabledSet bool

	// RecencyWeight exponentiates the freshness factor (freshness ^ weight)
	// so its influence is tunable without touching the curve: 0 disables it
	// exactly, 0.5 halves its effect in log space. Default 1.0.
	RecencyWeight float64

	// MinFreshness is the floor the exponential decays to. ZERO MEANS
	// "COMPUTE IT" from the live RRF constant and pool depth (§5.3b), not
	// "no floor" — see computedMinFreshness. Set it only to override for
	// calibration; 0.0 is safely reserved as the sentinel because the
	// computation clamps to [0.05, 0.95], so no legitimate value collides
	// with "unset".
	MinFreshness float64

	// SeriesSupersededWeight multiplies a superseded member of a declared
	// recurring series (§5.3). Default 0.25 — deliberately dominant over
	// the RRF band, because for series demotion (unlike the freshness term)
	// dominance IS the contract: a superseded digest must lose an undated
	// query.
	SeriesSupersededWeight float64

	// OverFetchFactor multiplies the caller's LIMIT so the Go-side re-rank
	// has something to reorder that SQL did not already discard. Default 3.
	// The EFFECTIVE factor is min(L × factor, poolCap) / L and is 1.0 at
	// L >= 40 — at that point the mechanism is a pure re-sort and can
	// rescue nothing, however large this is set (§5.5).
	OverFetchFactor int
}

// SetEnabled explicitly wires the master kill switch so applyDefaults does not
// overwrite an intentional false with the default true. Use this (not the bare
// field) when the value comes from operator config.
func (c *RecencyConfig) SetEnabled(v bool) {
	c.Enabled = v
	c.enabledSet = true
}

// DefaultRecencyConfig returns the §5.6 shipped defaults. MinFreshness is
// computed, not written down, so it tracks the constants it depends on.
func DefaultRecencyConfig() RecencyConfig {
	return RecencyConfig{
		Enabled:                true,
		enabledSet:             true,
		RecencyWeight:          1.0,
		MinFreshness:           computedMinFreshness(rrfConstantK, perArmCandidateCap),
		SeriesSupersededWeight: 0.25,
		OverFetchFactor:        3,
	}
}

// applyDefaults fills any zero-valued knob from the shipped defaults,
// returning the completed config. Non-zero operator values win. Same shape as
// RetrievalRoutingConfig.applyDefaults, including the enabledSet check.
//
// RecencyWeight is the one knob where zero is arguably meaningful (§5.5 says
// "0 disables it exactly"), and it still defaults: an operator who wants the
// term off has Enabled=false, which is the true revert, and letting a
// zero-valued struct silently mean "weight 0" would make an unconfigured
// caller get the feature's inert form rather than its shipped form.
func (c RecencyConfig) applyDefaults() RecencyConfig {
	d := DefaultRecencyConfig()
	out := c
	if out.RecencyWeight <= 0 {
		out.RecencyWeight = d.RecencyWeight
	}
	if out.MinFreshness <= 0 {
		out.MinFreshness = d.MinFreshness
	}
	if out.SeriesSupersededWeight <= 0 {
		out.SeriesSupersededWeight = d.SeriesSupersededWeight
	}
	if out.OverFetchFactor <= 0 {
		out.OverFetchFactor = d.OverFetchFactor
	}
	if !out.enabledSet {
		out.Enabled = d.Enabled
		out.enabledSet = true
	}
	return out
}

// chunkHalfLife is how fast one chunk loses ranking preference: its OWN
// retention window, `expires_at - created_at`.
//
// DERIVED PER CHUNK, not per class, and that is a correction made during
// implementation. The design's §5.1 carried a hand-tuned half-life per content
// class, which assumed the class TTL table describes what is actually stored.
// It does not. TTL is overridable per swarm-role (`memory.ttl_days`) and per
// deposit (LLD-22's `ttl_days`), and the live store shows the overrides are
// pervasive rather than theoretical: `decision` is declared never-expiring and
// holds five distinct finite TTLs from 180 to 3650 days; `diagnostic` is
// declared 7 days and ranges 7 to 3650 across twelve values; `research` is
// declared 90 and holds twenty-seven values. A per-class constant would have
// been wrong for nearly every class and would have drifted further with every
// override an operator added.
//
// Reading the row removes the table entirely. It needs no new plumbing —
// SearchResult already carries CreatedAt and ExpiresAt, both already selected
// by all three search paths — and it is exact at every override level for free.
//
// A nil ExpiresAt means no TTL, which means NO DECAY (flat 1.0 at any age), not
// instant staleness. That is what keeps the design record un-demoted: §4's rule
// that ranking must never bury a spec or a ruling is now a consequence of those
// chunks having no expiry, rather than a separate table entry that could drift
// out of agreement with it.
func chunkHalfLife(createdAt time.Time, expiresAt *time.Time) time.Duration {
	if expiresAt == nil {
		return 0
	}
	d := expiresAt.Sub(createdAt)
	if d <= 0 {
		// A backdated or already-expired row. Such a chunk is hard-excluded
		// from search anyway; returning 0 (no decay) rather than a negative
		// half-life keeps the curve total if one ever reaches the ranker.
		return 0
	}
	return d
}

// recencyFreshness is §5.2's curve: 1.0 for a chunk with no TTL, else
// max(floor, 0.5^(age/halfLife)).
//
// The floor is reached at 1.32 half-lives. Because the half-life IS the
// retention window, that point lies 32% PAST the chunk's own expiry — so no
// chunk saturates while it is still retrievable, whatever TTL it was given.
// The anti-dominance floor is a guard that, for every chunk currently in the
// store, never actually binds.
func recencyFreshness(createdAt time.Time, expiresAt *time.Time, now time.Time, cfg RecencyConfig) float64 {
	if createdAt.IsZero() {
		// UNKNOWN age, not infinite age. The search scanner has column tiers,
		// and a query shape below the 14-column tier leaves CreatedAt at its
		// zero value — which reads as roughly two thousand years old and would
		// floor every result. routing.go carries the same guard for the same
		// reason; it is the trap this shape sets for anyone who forgets that
		// "absent" and "very old" are the same bits.
		return 1.0
	}
	halfLife := chunkHalfLife(createdAt, expiresAt)
	if halfLife == 0 {
		return 1.0
	}
	age := now.Sub(createdAt)
	if age <= 0 {
		// Clock skew or a future-dated row must not score above 1.0.
		return 1.0
	}
	f := math.Pow(0.5, age.Seconds()/halfLife.Seconds())
	if floor := cfg.effectiveMinFreshness(); f < floor {
		return floor
	}
	return f
}

// effectiveMinFreshness resolves the 0 = "compute it" sentinel (§5.6).
func (c RecencyConfig) effectiveMinFreshness() float64 {
	if c.MinFreshness > 0 {
		return c.MinFreshness
	}
	return computedMinFreshness(rrfConstantK, perArmCandidateCap)
}

// dominanceThreshold is the closed form of §5.3b: below this multiplier, a
// factor applied after RRF fusion can drag a top-ranked result below one that
// placed last in a single arm — i.e. it stops reordering comparable results
// and starts acting as a sort key.
//
//	band(k, D)      = 2(k + D) / (k + 1)     // rank 1 in BOTH arms ÷ rank D in one
//	dominance(k, D) = 1 / band(k, D) = (k + 1) / (2(k + D))
//
// At the shipped k=60, D=20 this is 0.381, and the algebra matches the
// EXPLAIN-level measurement exactly rather than merely agreeing with it.
//
// The direction is what makes this worth computing: raising the pool depth
// WIDENS the band and LOWERS the threshold, so a deeper pool makes any given
// floor safer. Lowering D below ~18, or lowering k, raises the threshold past
// 0.40 — and a floor written down as a literal would silently become a
// dominating factor at that moment, with nothing to notice.
func dominanceThreshold(k, perArmCap int) float64 {
	return float64(k+1) / (2 * float64(k+perArmCap))
}

// computedMinFreshness derives the freshness floor from the constants it
// depends on (§5.3b), instead of carrying a literal that goes stale the moment
// its source moves — the failure mode this design hit three times (§5.4's
// created_at note, §5.5's candidate_pool_size divergence, and this floor).
//
// The clamp bounds also do double duty: they are what reserves 0.0 as
// RecencyConfig.MinFreshness's "unset" sentinel.
//
// Asymmetry worth knowing before trusting either bound: the LOWER one is
// reachable (a deep enough pool drives the threshold under 0.05), the UPPER
// one is not. dominanceThreshold approaches 0.5 from below as perArmCap falls
// toward 0, so the largest computable floor is 0.5 × 1.05 = 0.525. The 0.95
// ceiling is kept because the design specifies the clamp as a pair and a
// one-sided clamp invites the question of what happens on the other side —
// but it has never fired and must not be read as evidence that a floor near
// 0.95 is a state this code can produce.
func computedMinFreshness(k, perArmCap int) float64 {
	v := dominanceThreshold(k, perArmCap) * minFreshnessSafetyMargin
	if v < minFreshnessFloorBound {
		return minFreshnessFloorBound
	}
	if v > minFreshnessCeilBound {
		return minFreshnessCeilBound
	}
	return v
}

// --- the re-rank (§5.5) -------------------------------------------------

// overFetchLimit is how many rows the SQL must return so the re-rank has
// something to reorder.
//
// The final LIMIT cannot be the caller's limit, or the re-rank only reorders a
// set already chosen without it. But the candidate pool is capped inside the
// CTEs at perArmCandidateCap per arm across two arms, so over-fetching past
// that buys nothing — the rows do not exist.
//
// EFFECTIVE factor is min(L × factor, poolCap) / L, NOT the configured factor,
// and the difference is not cosmetic: at L = 40 it is 1.0 and the mechanism
// degenerates to a pure re-sort that can rescue nothing. §5.5 carries the
// table; §5.7 logs the effective pool so a trace never reports 3× on a query
// where the cap made it 1.
func overFetchLimit(limit int, cfg RecencyConfig) int {
	if !cfg.applyDefaults().Enabled || limit <= 0 {
		return limit
	}
	cfg = cfg.applyDefaults()
	want := limit * cfg.OverFetchFactor
	if poolCap := perArmCandidateCap * 2; want > poolCap {
		want = poolCap
	}
	// Never fewer than the caller asked for: a limit above the pool cap must
	// still return what the pool holds, not a truncated page.
	if want < limit {
		return limit
	}
	return want
}

// applyRecency rescores, re-sorts and cuts to the caller's limit.
//
// Order matters and is the point: the cut happens AFTER the re-sort, so a
// chunk that the base score would have dropped can still surface on freshness.
// Doing it the other way round is the bug §5.5 exists to prevent — the re-rank
// would only be able to shuffle a set already chosen without it.
//
// Disabled is a true revert: same slice, same order, same scores, and
// overFetchLimit returns the caller's limit so even the SQL is unchanged.
func applyRecency(in []SearchResult, cfg RecencyConfig, now time.Time, limit int) []SearchResult {
	if !cfg.applyDefaults().Enabled || len(in) == 0 {
		return in
	}
	cfg = cfg.applyDefaults()
	out := make([]SearchResult, len(in))
	copy(out, in)
	for i := range out {
		out[i].Score *= recencyMultiplier(out[i], cfg, now)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		// Determinism contract (rag-retrieval-and-graph-design.md §3.1).
		return out[a].ChunkID < out[b].ChunkID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// recencyMultiplier is the per-chunk factor: freshness^weight × seriesWeight.
//
// The two legs are deliberately asymmetric (§5.3a). Freshness is a GRADIENT and
// is exponentiated by RecencyWeight so its influence is tunable without
// touching the curve. Series demotion is a BINARY state — a member either has a
// newer sibling or it does not — so it is a flat multiplier; an exponent over a
// two-valued function would just be a second spelling of the same knob.
//
// seriesWeight is 0.25, which DOMINATES the 2.62× RRF band by design. For an
// undated query a superseded digest should lose outright: that is the contract,
// not a side effect. Findability for the older member rests on the temporal
// query path (§5.3a), not on this number.
func recencyMultiplier(r SearchResult, cfg RecencyConfig, now time.Time) float64 {
	m := 1.0
	if f := recencyFreshness(r.CreatedAt, r.ExpiresAt, now, cfg); f < 1.0 {
		m *= math.Pow(f, cfg.RecencyWeight)
	}
	if r.SeriesSuperseded {
		m *= cfg.SeriesSupersededWeight
	}
	return m
}

// --- observability (§5.7) -----------------------------------------------

// recencyTraceParams builds the `recency_rerank` stage row's parameters.
//
// The two pool fields exist because §5.5 forced them: the CONFIGURED over-fetch
// factor and the EFFECTIVE one diverge once the per-arm candidate cap bites, and
// at a caller limit of 40 the effective factor is 1.0 — a pure re-sort that can
// rescue nothing. Logging the configured 3 there would be "a number with no
// scope reads as a guarantee". So the trace carries what actually happened.
//
// min_freshness is recorded rather than assumed because it is COMPUTED from the
// live k and pool depth (§5.3b), so a tuning session reading a literal from the
// design would be reading the wrong number.
func recencyTraceParams(cfg RecencyConfig, limit, reordered, topShift int, clipped bool) map[string]any {
	cfg = cfg.applyDefaults()
	return map[string]any{
		"enabled":               cfg.Enabled,
		"recency_weight":        cfg.RecencyWeight,
		"min_freshness":         cfg.effectiveMinFreshness(),
		"series_weight":         cfg.SeriesSupersededWeight,
		"over_fetch_configured": cfg.OverFetchFactor,
		"pool_size_effective":   overFetchLimit(limit, cfg),
		"pool_cap_clipped":      clipped,
		"reordered_count":       reordered,
		"top_shift":             topShift,
	}
}

// countReordered reports how many positions the re-rank changed, and how far
// the new rank-1 travelled.
//
// Measured against the PRE-rerank order, so a re-rank that changed nothing
// reports zeros — which is what distinguishes "enabled and inert" from
// "disabled", the distinction §5.7 exists to make. Truncation is deliberately
// not reordering: an over-fetched tail that got cut has not moved, it has gone,
// and counting it would report churn on every single query.
func countReordered(before, after []SearchResult) (reordered, topShift int) {
	if len(after) == 0 {
		return 0, 0
	}
	pos := make(map[string]int, len(before))
	for i, r := range before {
		pos[r.ChunkID] = i
	}
	for i, r := range after {
		if was, ok := pos[r.ChunkID]; ok && was != i {
			reordered++
			if i == 0 {
				topShift = was
			}
		}
	}
	return reordered, topShift
}
