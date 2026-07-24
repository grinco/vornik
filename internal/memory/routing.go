package memory

import (
	"fmt"
	"time"
)

// Confidence-based retrieval routing (P3). Computes a daemon-side
// retrieval_trust_verdict from the stored trust fields (confidence,
// validation_status, freshness) — deliberately independent of the
// uncalibrated RRF SearchResult.Score, so the verdict is invariant to
// flipping the reranker. See
// https://docs.vornik.io

// Verdict levels. Ordered low < medium < high (see verdictRank).
const (
	VerdictHigh   = "high"
	VerdictMedium = "medium"
	VerdictLow    = "low"
	// VerdictUnknown is emitted when the result set carries no trust
	// signals at all — i.e. it came back from a non-projected fallback
	// search path (HybridSearch / KeywordSearch / substring tiers) whose
	// SELECT does not include the trust columns, so every row arrives with
	// zero CreatedAt, empty ValidationStatus, and zero Confidence. Without
	// this, `freshnessFor` would read the zero CreatedAt as ~2000 years old
	// → freshness 0 → trust ~0.36 → a spurious `low` that both misleads the
	// agent AND fires the widen loop (which re-hits the same fallback and
	// burns MaxRounds). "unknown" means "we cannot rate these"; it never
	// widens (the widen fires only on `low`) and its guidance tells the
	// agent the results are relevance-only. Review I-1 (review-…-P3-impl).
	VerdictUnknown = "unknown"
)

// weakest_dim enumeration — the (single) dimension that explains why the
// verdict is not high, chosen deterministically so guidance branching is
// testable. Precedence when several coincide on a medium verdict:
// aged_decision > result_count > trust_mean (§3.2 F-E).
const (
	WeakestNone         = "none"
	WeakestResultCount  = "result_count"
	WeakestTrustMean    = "trust_mean"
	WeakestAgedDecision = "aged_decision"
)

// Validation-status tokens (refuted/superseded are already filtered out of
// the result set by the search SQL, so only these three — plus the empty /
// unknown fallback treated as unverified — reach the verdict).
const (
	statusVerified   = "verified"
	statusUnverified = "unverified"
	statusLegacy     = "legacy"
)

// VerdictBasis is the explainable basis carried alongside the verdict —
// never a bare float, so guidance and the trace are reconstructable.
type VerdictBasis struct {
	ResultCount int     `json:"result_count"`
	TrustMean   float64 `json:"trust_mean"`
	AgeCapped   bool    `json:"age_capped"`
	WeakestDim  string  `json:"weakest_dim"`
	// TopHitAgeDays is the age of the rank-1 hit in whole days when
	// AgeCapped is set (drives the "decision N days old" guidance).
	// Zero and meaningless when AgeCapped is false.
	TopHitAgeDays int `json:"top_hit_age_days,omitempty"`
}

// RoutingVerdict is the full routing signal returned to opted-in callers.
// Emitted only when SearchOptions.Routing is set.
type RoutingVerdict struct {
	Verdict     string       `json:"retrieval_trust_verdict"`
	Basis       VerdictBasis `json:"verdict_basis"`
	Guidance    string       `json:"guidance"`
	WidenRounds int          `json:"widen_rounds"`
}

// verdictRank maps a verdict to a comparable rank (low < medium < high).
func verdictRank(v string) int {
	switch v {
	case VerdictHigh:
		return 2
	case VerdictMedium:
		return 1
	default:
		return 0
	}
}

// clamp01 clamps x to [0,1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// classTTLFor resolves the TTL policy for a chunk's content class. A zero
// duration means the class never expires (spec/decision) — the no-TTL band
// in §3.4. Unknown / empty classes fall back to the unclassified policy
// (which carries a TTL), matching how the rest of the pipeline treats them.
func classTTLFor(class string) time.Duration {
	if pol, ok := DefaultClassPolicies[ContentClass(class)]; ok {
		return pol.TTL
	}
	return DefaultClassPolicies[ClassUnclassified].TTL
}

// statusWeight maps a validation status to its trust weight (§3.2).
// Empty / unknown statuses are treated as unverified (the DB default), never
// silently dropped.
func statusWeight(status string) float64 {
	switch status {
	case statusVerified:
		return 1.0
	case statusLegacy:
		return 0.4
	default: // unverified + empty/unknown
		return 0.6
	}
}

// confTerm computes the (gated) confidence contribution for one result:
// verified → confidence; unverified/unknown → discount·confidence; legacy → 0
// (legacy confidence is a placeholder; its low statusWeight already reflects
// the distrust — don't double-inject noise, §3.2 F-D).
func confTerm(status string, confidence, discount float64) float64 {
	switch status {
	case statusVerified:
		return confidence
	case statusLegacy:
		return 0
	default:
		return discount * confidence
	}
}

// freshnessFor computes the freshness leg for one result (§3.4).
//   - TTL'd class: clamp(1 − age/TTL, 0, 1), computed from created_at + the
//     class TTL (NOT the stored expires_at, which a backdated edit could skew).
//   - No-TTL class (spec/decision): 1.0 under the age cap; floored to
//     noTTLStaleFreshness once age exceeds noTTLAgeCap.
func freshnessFor(r SearchResult, cfg RetrievalRoutingConfig, now time.Time) float64 {
	ttl := classTTLFor(r.ContentClass)
	age := now.Sub(r.CreatedAt)
	if ttl > 0 {
		return clamp01(1 - age.Seconds()/ttl.Seconds())
	}
	// No-TTL class.
	if age > cfg.NoTTLAgeCap {
		return cfg.NoTTLStaleFreshness
	}
	return 1.0
}

// trustFor computes trust_i ∈ [0,1] for one result.
func trustFor(r SearchResult, cfg RetrievalRoutingConfig, now time.Time) float64 {
	sw := statusWeight(r.ValidationStatus)
	ct := confTerm(r.ValidationStatus, r.Confidence, cfg.UnverifiedConfDiscount)
	fr := freshnessFor(r, cfg, now)
	return cfg.WStatus*sw + cfg.WConf*ct + cfg.WFresh*fr
}

// topHitAgeCapped reports whether the rank-1 result is an aged no-TTL chunk
// (§3.4 — the age cap is tied to the TOP hit only, not any top-K member).
// Also returns the top hit's age in whole days for the guidance string.
func topHitAgeCapped(results []SearchResult, cfg RetrievalRoutingConfig, now time.Time) (bool, int) {
	if len(results) == 0 {
		return false, 0
	}
	top := results[0]
	if classTTLFor(top.ContentClass) != 0 {
		return false, 0 // TTL'd class — the TTL filter, not the cap, governs it
	}
	age := now.Sub(top.CreatedAt)
	if age <= cfg.NoTTLAgeCap {
		return false, 0
	}
	return true, int(age.Hours() / 24)
}

// trustDataAbsent reports whether a non-empty result set carries no trust
// signals whatsoever — the signature of a non-projected fallback search path
// (its SELECT omits the confidence/validation_status/created_at columns, so
// scanSearchResults leaves them zero-valued). A single row with any of a
// non-zero CreatedAt, a non-empty ValidationStatus, or a non-zero Confidence
// means the projection ran and the set is ratable. See review I-1.
func trustDataAbsent(results []SearchResult) bool {
	for _, r := range results {
		if !r.CreatedAt.IsZero() || r.ValidationStatus != "" || r.Confidence != 0 {
			return false
		}
	}
	return true
}

// trustMean computes the mean trust over the top min(count, K) results,
// dividing by the actual count (never by K when fewer exist, §3.2).
func trustMean(results []SearchResult, cfg RetrievalRoutingConfig, now time.Time) float64 {
	n := len(results)
	if n > cfg.K {
		n = cfg.K
	}
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += trustFor(results[i], cfg, now)
	}
	return sum / float64(n)
}

// classifyVerdict maps (count, mean, ageCapped) to a verdict level (§3.2).
// The low boundary is EXCLUSIVE (mean < lowThreshold), so mean == lowThreshold
// is medium. age-cap forces the verdict to at most medium (never high).
func classifyVerdict(count int, mean float64, ageCapped bool, cfg RetrievalRoutingConfig) string {
	if count == 0 || mean < cfg.LowThreshold {
		return VerdictLow
	}
	if count >= cfg.MinResults && mean >= cfg.HighThreshold && !ageCapped {
		return VerdictHigh
	}
	return VerdictMedium
}

// weakestDim picks the deterministic explanation for a non-high verdict.
//   - high  → none.
//   - low   → result_count when the set is empty, else trust_mean. The age cap
//     never surfaces on a low verdict: low is reached via the count/trust gate,
//     not the cap (§3.3 F-C), so an aged top hit whose set is ALSO weak reports
//     trust_mean and (correctly) widens.
//   - medium→ precedence aged_decision > result_count > trust_mean (§3.2 F-E).
func weakestDim(verdict string, count int, ageCapped bool, cfg RetrievalRoutingConfig) string {
	switch verdict {
	case VerdictHigh:
		return WeakestNone
	case VerdictLow:
		if count == 0 {
			return WeakestResultCount
		}
		return WeakestTrustMean
	default: // medium
		if ageCapped {
			return WeakestAgedDecision
		}
		if count < cfg.MinResults {
			return WeakestResultCount
		}
		return WeakestTrustMean
	}
}

// EvaluateVerdict computes the retrieval_trust_verdict, its basis, and the
// basis-parameterised guidance over a result set. Pure and deterministic
// given (results, cfg, now) — this is the unit-testable heart of the feature.
// The caller must pass a defaulted cfg (cfg.applyDefaults()).
func (cfg RetrievalRoutingConfig) EvaluateVerdict(results []SearchResult, now time.Time) RoutingVerdict {
	count := len(results)
	// Trust-data-absent guard (review I-1): a non-empty set in which no row
	// carries any trust signal came from a non-projected fallback search
	// path. Rating it would manufacture a spurious `low` (zero CreatedAt →
	// freshness 0) and trigger a futile widen. Report `unknown` instead.
	if count > 0 && trustDataAbsent(results) {
		return RoutingVerdict{
			Verdict:  VerdictUnknown,
			Basis:    VerdictBasis{ResultCount: count, WeakestDim: WeakestNone},
			Guidance: "Trust signals are unavailable for these results (degraded retrieval path); treat them as relevance-only — the trust verdict could not be computed.",
		}
	}
	mean := trustMean(results, cfg, now)
	ageCapped, topAgeDays := topHitAgeCapped(results, cfg, now)
	verdict := classifyVerdict(count, mean, ageCapped, cfg)
	dim := weakestDim(verdict, count, ageCapped, cfg)

	basis := VerdictBasis{
		ResultCount: count,
		TrustMean:   mean,
		AgeCapped:   ageCapped,
		WeakestDim:  dim,
	}
	if ageCapped {
		basis.TopHitAgeDays = topAgeDays
	}
	return RoutingVerdict{
		Verdict:  verdict,
		Basis:    basis,
		Guidance: guidanceFor(verdict, basis),
	}
}

// guidanceFor renders the deterministic, basis-parameterised guidance string.
// It names the weakness so the agent picks the right remedy (rephrase vs
// web-fetch vs answer-without-context). When the top hit is age-capped the
// guidance TAKES PRECEDENCE over a high trust_mean and states the cap reason
// (§3.5 F5) — an agent must act on (verdict + guidance), not the raw mean.
func guidanceFor(verdict string, basis VerdictBasis) string {
	// Age-cap message wins whenever it is the binding weakness, regardless of
	// the (possibly high) trust_mean the basis carries.
	if basis.WeakestDim == WeakestAgedDecision {
		return fmt.Sprintf("Top hit is a no-TTL decision/spec %d days old — re-confirm it is still current before relying on it; the DB widen cannot fix staleness.", basis.TopHitAgeDays)
	}
	switch verdict {
	case VerdictHigh:
		return "High-trust results — proceed using memory context."
	case VerdictLow:
		if basis.WeakestDim == WeakestResultCount {
			return "No memory results — answer without memory context, rephrase the query, or web-fetch/escalate."
		}
		return "Low-trust results — prefer a web-fetch or escalation over relying on these; the daemon already widened DB recall."
	default: // medium
		if basis.WeakestDim == WeakestResultCount {
			return "Few results — consider rephrasing or widening the query before relying on memory."
		}
		return "Moderate-trust results — usable, but verify key facts before relying on them."
	}
}
