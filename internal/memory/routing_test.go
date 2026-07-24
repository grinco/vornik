package memory

import (
	"strings"
	"testing"
	"time"
)

// Confidence-based retrieval routing (P3) — verdict computation tests.
// These pin the §5 test-plan cases against the pure EvaluateVerdict core.

// mkResult builds a SearchResult with the trust fields the verdict reads.
// ageDays sets CreatedAt = now-ageDays; class/status/confidence are the
// verdict inputs. Score is deliberately irrelevant to the verdict.
func mkResult(class, status string, confidence float64, ageDays int, now time.Time) SearchResult {
	return SearchResult{
		ChunkID:          "c",
		ContentClass:     class,
		ValidationStatus: status,
		Confidence:       confidence,
		CreatedAt:        now.Add(-time.Duration(ageDays) * 24 * time.Hour),
		Score:            0.5,
	}
}

func defCfg() RetrievalRoutingConfig { return DefaultRetrievalRoutingConfig() }

func TestEvaluateVerdict_EmptySet_Low(t *testing.T) {
	now := time.Now()
	v := defCfg().EvaluateVerdict(nil, now)
	if v.Verdict != VerdictLow {
		t.Fatalf("empty set: want low, got %s", v.Verdict)
	}
	if v.Basis.WeakestDim != WeakestResultCount {
		t.Fatalf("empty set: want weakest=result_count, got %s", v.Basis.WeakestDim)
	}
	if v.Basis.ResultCount != 0 {
		t.Fatalf("empty set: want result_count 0, got %d", v.Basis.ResultCount)
	}
}

// TestEvaluateVerdict_TrustDataAbsent_Unknown — review I-1: a non-empty set
// with no trust signals (zero CreatedAt + empty status + zero confidence, the
// signature of a non-projected fallback search path) must yield `unknown`, NOT
// a spurious `low` (which would mislead the agent and fire a futile widen).
func TestEvaluateVerdict_TrustDataAbsent_Unknown(t *testing.T) {
	now := time.Now()
	// Fallback-shaped rows: only ChunkID/Content/Score populated; all trust
	// fields zero-valued (CreatedAt zero, status "", confidence 0).
	rs := []SearchResult{{ChunkID: "a", Score: 0.9}, {ChunkID: "b", Score: 0.8}}
	v := defCfg().EvaluateVerdict(rs, now)
	if v.Verdict != VerdictUnknown {
		t.Fatalf("trust-absent set: want unknown, got %s", v.Verdict)
	}
	if v.Verdict == VerdictLow {
		t.Fatal("trust-absent must not manufacture low (would trigger widen)")
	}
	if v.Basis.ResultCount != 2 {
		t.Fatalf("want result_count 2, got %d", v.Basis.ResultCount)
	}
	// A single row with any real trust signal means the projection ran — rate it.
	mixed := []SearchResult{mkResult(string(ClassResearch), statusVerified, 0.5, 0, now)}
	if v := defCfg().EvaluateVerdict(mixed, now); v.Verdict == VerdictUnknown {
		t.Fatal("a set with real trust fields must not be classified unknown")
	}
}

func TestEvaluateVerdict_AllVerifiedFresh_High(t *testing.T) {
	now := time.Now()
	// fresh verified research conf=0.5 → trust 0.90 (LLD worked point).
	rs := []SearchResult{
		mkResult(string(ClassResearch), statusVerified, 0.5, 0, now),
		mkResult(string(ClassResearch), statusVerified, 0.5, 1, now),
	}
	v := defCfg().EvaluateVerdict(rs, now)
	if v.Verdict != VerdictHigh {
		t.Fatalf("fresh verified: want high, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	if v.Basis.WeakestDim != WeakestNone {
		t.Fatalf("high: want weakest=none, got %s", v.Basis.WeakestDim)
	}
}

// false-high regression — the Moltbook stale-fact failure mode: an old
// verified DECISION as the authoritative top hit must NOT be certified high
// (that is exactly the "stale-fact-as-planning-fact" bug this feature exists
// to stop). It must land at medium with age_capped=true, weakest=aged_decision.
func TestEvaluateVerdict_FalseHigh_AgedVerifiedDecision_TopHit_Medium(t *testing.T) {
	now := time.Now()
	// decision = no-TTL class; age 200d > 180d cap. Verified conf 0.9.
	rs := []SearchResult{mkResult(string(ClassDecision), statusVerified, 0.9, 200, now)}
	v := defCfg().EvaluateVerdict(rs, now)
	if v.Verdict != VerdictMedium {
		t.Fatalf("aged verified decision top hit: want medium (NOT high), got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	if !v.Basis.AgeCapped {
		t.Fatalf("want age_capped=true")
	}
	if v.Basis.WeakestDim != WeakestAgedDecision {
		t.Fatalf("want weakest=aged_decision, got %s", v.Basis.WeakestDim)
	}
}

// age-cap scope (F4): the cap triggers on the TOP hit only. An aged no-TTL
// chunk at rank 5 behind four fresh-verified hits must NOT trigger the cap.
func TestEvaluateVerdict_AgeCapRank5_DoesNotTrigger(t *testing.T) {
	now := time.Now()
	rs := []SearchResult{
		mkResult(string(ClassResearch), statusVerified, 0.5, 0, now),
		mkResult(string(ClassResearch), statusVerified, 0.5, 0, now),
		mkResult(string(ClassResearch), statusVerified, 0.5, 0, now),
		mkResult(string(ClassResearch), statusVerified, 0.5, 0, now),
		mkResult(string(ClassDecision), statusVerified, 0.9, 200, now), // aged, rank 5
	}
	v := defCfg().EvaluateVerdict(rs, now)
	if v.Basis.AgeCapped {
		t.Fatalf("rank-5 aged chunk must NOT trigger the cap")
	}
	if v.Verdict != VerdictHigh {
		t.Fatalf("four fresh-verified + one aged rank-5: want high, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
}

// cold-start (F2): a fresh UNVERIFIED set with count>=minResults is medium
// (usable), NOT low; a fresh VERIFIED set is high. Confidence held equal so
// only validation_status moves the verdict.
func TestEvaluateVerdict_ColdStart_FreshUnverified_Medium(t *testing.T) {
	now := time.Now()
	unv := []SearchResult{mkResult(string(ClassResearch), statusUnverified, 0.5, 0, now)}
	if v := defCfg().EvaluateVerdict(unv, now); v.Verdict != VerdictMedium {
		t.Fatalf("fresh unverified: want medium (NOT low), got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	ver := []SearchResult{mkResult(string(ClassResearch), statusVerified, 0.5, 0, now)}
	if v := defCfg().EvaluateVerdict(ver, now); v.Verdict != VerdictHigh {
		t.Fatalf("fresh verified: want high, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
}

// aged top-hit + weak set (F-C): an aged no-TTL top hit AND mean<lowThreshold
// resolves to LOW (not medium) — low is reached via the trust gate, so the
// widen SHOULD fire and weakest_dim is trust_mean (NOT aged_decision).
func TestEvaluateVerdict_AgedTopHit_WeakSet_Low_TrustMean(t *testing.T) {
	now := time.Now()
	rs := []SearchResult{
		mkResult(string(ClassDecision), statusVerified, 0.9, 200, now), // aged top hit, trust ~0.84
		mkResult(string(ClassResearch), statusLegacy, 0.5, 200, now),   // legacy stale → trust 0.24
		mkResult(string(ClassResearch), statusLegacy, 0.5, 200, now),
		mkResult(string(ClassResearch), statusLegacy, 0.5, 200, now),
		mkResult(string(ClassResearch), statusLegacy, 0.5, 200, now),
	}
	v := defCfg().EvaluateVerdict(rs, now)
	if v.Verdict != VerdictLow {
		t.Fatalf("aged top hit + weak set: want low, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	if v.Basis.WeakestDim != WeakestTrustMean {
		t.Fatalf("want weakest=trust_mean (low via trust gate), got %s", v.Basis.WeakestDim)
	}
}

// boundary (F-F): mean == lowThreshold (0.40) is MEDIUM, not low (the low
// boundary is exclusive). A legacy research chunk 18d old has trust exactly
// 0.6*0.4 + 0 + 0.2*(1-18/90) = 0.24 + 0.2*0.8 = 0.40.
func TestEvaluateVerdict_BoundaryMeanEqualsLow_Medium(t *testing.T) {
	now := time.Now()
	rs := []SearchResult{mkResult(string(ClassResearch), statusLegacy, 0.5, 18, now)}
	v := defCfg().EvaluateVerdict(rs, now)
	if got := v.Basis.TrustMean; got < 0.399 || got > 0.401 {
		t.Fatalf("precondition: want mean≈0.40, got %.4f", got)
	}
	if v.Verdict != VerdictMedium {
		t.Fatalf("mean==low boundary: want medium, got %s", v.Verdict)
	}
}

// legacy confTerm (F-D): a legacy chunk contributes 0 from confTerm
// regardless of its stored confidence.
func TestConfTerm_LegacyIsZero(t *testing.T) {
	if got := confTerm(statusLegacy, 0.9, 0.5); got != 0 {
		t.Fatalf("legacy confTerm: want 0, got %v", got)
	}
	if got := confTerm(statusLegacy, 0.1, 0.5); got != 0 {
		t.Fatalf("legacy confTerm (low conf): want 0, got %v", got)
	}
	// A legacy chunk's trust is invariant to its confidence.
	now := time.Now()
	hi := trustFor(mkResult(string(ClassResearch), statusLegacy, 0.9, 0, now), defCfg(), now)
	lo := trustFor(mkResult(string(ClassResearch), statusLegacy, 0.1, 0, now), defCfg(), now)
	if hi != lo {
		t.Fatalf("legacy trust must be confidence-invariant: %.4f vs %.4f", hi, lo)
	}
}

// confidence leg discounted (not zeroed): unverified conf 0.9 contributes
// discount·0.9; verified 0.9 contributes 0.9. Gradient exists, verified>unverified>0.
func TestConfTerm_UnverifiedDiscountedGradient(t *testing.T) {
	unv := confTerm(statusUnverified, 0.9, 0.5)
	ver := confTerm(statusVerified, 0.9, 0.5)
	if unv <= 0 {
		t.Fatalf("unverified confTerm must be >0 (discounted, not zeroed), got %v", unv)
	}
	if !(unv < ver) {
		t.Fatalf("verified confTerm (%v) must exceed unverified (%v)", ver, unv)
	}
	if unv != 0.45 {
		t.Fatalf("unverified confTerm: want 0.45 (0.5·0.9), got %v", unv)
	}
}

// weakest_dim precedence (F-E): on a MEDIUM verdict where the set is
// simultaneously low-count (count<minResults) AND aged-top-hit, aged_decision
// wins the precedence (aged_decision > result_count > trust_mean). Requires
// minResults>1 to make count a live weakness.
func TestEvaluateVerdict_WeakestDimPrecedence_AgedWins(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	cfg.MinResults = 3 // valid: 3 <= K(5)
	rs := []SearchResult{
		mkResult(string(ClassDecision), statusVerified, 0.9, 200, now), // aged top hit ~0.84
		mkResult(string(ClassResearch), statusUnverified, 0.2, 200, now),
	}
	v := cfg.EvaluateVerdict(rs, now)
	if v.Verdict != VerdictMedium {
		t.Fatalf("precondition: want medium, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	if v.Basis.WeakestDim != WeakestAgedDecision {
		t.Fatalf("precedence: want aged_decision, got %s", v.Basis.WeakestDim)
	}
}

// result_count precedes trust_mean on a medium verdict when NOT aged.
func TestEvaluateVerdict_WeakestDim_ResultCountBeatsTrustMean(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	cfg.MinResults = 3
	// Two fresh-unverified (trust ~0.61 each) → mean in medium band, not aged,
	// count 2 < minResults 3.
	rs := []SearchResult{
		mkResult(string(ClassResearch), statusUnverified, 0.5, 0, now),
		mkResult(string(ClassResearch), statusUnverified, 0.5, 0, now),
	}
	v := cfg.EvaluateVerdict(rs, now)
	if v.Verdict != VerdictMedium {
		t.Fatalf("precondition medium, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	if v.Basis.WeakestDim != WeakestResultCount {
		t.Fatalf("want weakest=result_count, got %s", v.Basis.WeakestDim)
	}
}

// Verdict invariant to Rerank: the verdict is computed only from trust fields,
// never from Score. Two identical trust sets with wildly different Scores must
// yield an identical verdict + basis.
func TestEvaluateVerdict_InvariantToScore(t *testing.T) {
	now := time.Now()
	a := mkResult(string(ClassResearch), statusVerified, 0.5, 0, now)
	b := a
	a.Score, b.Score = 0.01, 99.0 // reranker would reorder/re-score; trust identical
	va := defCfg().EvaluateVerdict([]SearchResult{a}, now)
	vb := defCfg().EvaluateVerdict([]SearchResult{b}, now)
	if va.Verdict != vb.Verdict || va.Basis != vb.Basis {
		t.Fatalf("verdict must be invariant to Score: %+v vs %+v", va, vb)
	}
}

// trustMean divides by the actual count, never by K when fewer results exist.
func TestTrustMean_DividesByActualCount(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	rs := []SearchResult{mkResult(string(ClassResearch), statusVerified, 0.5, 0, now)} // trust 0.90
	got := trustMean(rs, cfg, now)
	if got < 0.899 || got > 0.901 {
		t.Fatalf("single fresh-verified: want mean≈0.90 (÷1, not ÷K), got %.4f", got)
	}
}

// Guidance takes precedence over a high trust_mean when age-capped (§3.5 F5):
// the medium/age-capped guidance must name the staleness, not read as usable.
func TestGuidance_AgeCappedPrecedence(t *testing.T) {
	now := time.Now()
	rs := []SearchResult{mkResult(string(ClassDecision), statusVerified, 0.9, 200, now)}
	v := defCfg().EvaluateVerdict(rs, now)
	if v.Basis.TrustMean < 0.70 {
		t.Fatalf("precondition: want a high raw trust_mean, got %.3f", v.Basis.TrustMean)
	}
	if v.Guidance == "" {
		t.Fatal("expected non-empty guidance")
	}
	if !strings.Contains(v.Guidance, "re-confirm") {
		t.Fatalf("age-capped guidance must state the cap reason, got %q", v.Guidance)
	}
}

// --- config defaults + invariant ---

func TestRetrievalRoutingConfig_Defaults(t *testing.T) {
	d := DefaultRetrievalRoutingConfig()
	if d.K != 5 || d.MinResults != 1 || d.HighThreshold != 0.70 || d.LowThreshold != 0.40 {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	if d.WStatus != 0.6 || d.WConf != 0.2 || d.WFresh != 0.2 {
		t.Fatalf("unexpected weights: %+v", d)
	}
	if d.MaxRounds != 3 || !d.WidenEnabled {
		t.Fatalf("unexpected widen defaults: %+v", d)
	}
	if d.NoTTLAgeCap != 180*24*time.Hour || d.NoTTLStaleFreshness != 0.3 {
		t.Fatalf("unexpected no-ttl defaults: %+v", d)
	}
}

func TestRetrievalRoutingConfig_ApplyDefaults_FillsZeros(t *testing.T) {
	got := (RetrievalRoutingConfig{}).applyDefaults()
	if got.K != 5 || got.MinResults != 1 || !got.WidenEnabled {
		t.Fatalf("applyDefaults did not fill zeros: %+v", got)
	}
}

func TestRetrievalRoutingConfig_ApplyDefaults_ExplicitWidenFalseSticks(t *testing.T) {
	c := RetrievalRoutingConfig{}
	c.SetWidenEnabled(false)
	if got := c.applyDefaults(); got.WidenEnabled {
		t.Fatal("explicit widen_enabled=false must survive applyDefaults")
	}
}

// minResults > K → config-load error (invariant).
func TestRetrievalRoutingConfig_Validate_MinResultsGreaterThanK(t *testing.T) {
	if err := (RetrievalRoutingConfig{K: 5, MinResults: 6}).Validate(); err == nil {
		t.Fatal("want config-load error for min_results(6) > k(5)")
	}
	if err := (RetrievalRoutingConfig{K: 5, MinResults: 5}).Validate(); err != nil {
		t.Fatalf("min_results==k must be valid, got %v", err)
	}
	// unset min_results (0→default 1) with unset k (0→default 5): valid.
	if err := (RetrievalRoutingConfig{}).Validate(); err != nil {
		t.Fatalf("all-default must be valid, got %v", err)
	}
}
