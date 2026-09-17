package memory

import (
	"math"
	"testing"
	"time"
)

// The re-rank itself (§5.5): score × freshness^recencyWeight × seriesWeight,
// re-sorted, then cut to the caller's limit.
//
// These are the properties an edit is most likely to break silently. The
// incident-level regression lives in repotest, against both drivers.

func rr(id string, score float64, ageDays, ttlDays int, superseded bool) SearchResult {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Duration(ageDays) * 24 * time.Hour)
	sr := SearchResult{ChunkID: id, Score: score, CreatedAt: created, SeriesSuperseded: superseded}
	if ttlDays > 0 {
		e := created.Add(time.Duration(ttlDays) * 24 * time.Hour)
		sr.ExpiresAt = &e
	}
	return sr
}

func ids(rs []SearchResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ChunkID)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var rerankNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// THE INCIDENT, at the scoring level: an 11-day-old digest outranked the
// current one. Series demotion must flip that, and it must flip it even though
// the stale chunk has the higher base score — which is what made the incident
// possible in the first place.
func TestApplyRecency_SupersededMemberLosesToFresh(t *testing.T) {
	cfg := DefaultRecencyConfig()
	in := []SearchResult{
		rr("stale", 0.0328, 11, 90, true), // rank 1 by RRF, superseded
		rr("fresh", 0.0300, 0, 90, false), // lower base score, current
	}
	got := applyRecency(in, cfg, rerankNow, 10)
	if !eq(ids(got), []string{"fresh", "stale"}) {
		t.Errorf("order = %v, want [fresh stale] — a superseded series member must lose the "+
			"undated query even from RRF rank 1", ids(got))
	}
}

// §4: a chunk with no TTL never decays, so ranking can never bury the design
// record however old it is. This is the guard for 51.5% of the store.
func TestApplyRecency_NoTTLChunksAreUntouched(t *testing.T) {
	cfg := DefaultRecencyConfig()
	in := []SearchResult{
		rr("spec-2yr", 0.0300, 730, 0, false), // no TTL, two years old
		rr("fresh-sum", 0.0328, 0, 30, false),
	}
	got := applyRecency(in, cfg, rerankNow, 10)
	// The fresh chunk still leads on its own higher base score, but the aged
	// spec must not have been demoted at all.
	for _, r := range got {
		if r.ChunkID == "spec-2yr" && math.Abs(r.Score-0.0300) > 1e-12 {
			t.Errorf("a no-TTL chunk was rescored %v from 0.0300 — ranking must never decay one", r.Score)
		}
	}
}

// Age reorders comparable results; it must not override a decisive relevance
// gap. This is what the §5.3b floor buys, expressed as behaviour.
func TestApplyRecency_AgeDoesNotOverturnADecisiveScoreGap(t *testing.T) {
	cfg := DefaultRecencyConfig()
	in := []SearchResult{
		rr("old-but-far-better", 0.0328, 89, 90, false), // nearly expired
		rr("fresh-but-weak", 0.0125, 0, 90, false),      // bottom of the pool
	}
	got := applyRecency(in, cfg, rerankNow, 10)
	if got[0].ChunkID != "old-but-far-better" {
		t.Errorf("order = %v — freshness must not outrank a 2.6x relevance gap; that is the "+
			"dominance the floor exists to prevent", ids(got))
	}
}

// Disabled means byte-identical: same order, same scores, nothing touched.
func TestApplyRecency_DisabledIsAPureNoOp(t *testing.T) {
	cfg := DefaultRecencyConfig()
	cfg.SetEnabled(false)
	in := []SearchResult{
		rr("stale", 0.0328, 11, 90, true),
		rr("fresh", 0.0300, 0, 90, false),
	}
	got := applyRecency(in, cfg, rerankNow, 10)
	if !eq(ids(got), []string{"stale", "fresh"}) {
		t.Errorf("order = %v, want the input order unchanged when disabled", ids(got))
	}
	if got[0].Score != 0.0328 || got[1].Score != 0.0300 {
		t.Error("scores were modified while disabled; the switch must be a true revert")
	}
}

// The over-fetched tail is cut AFTER re-ranking, not before — otherwise the
// re-rank can only reorder a set already chosen without it (§5.5).
func TestApplyRecency_CutsToTheCallerLimitAfterReordering(t *testing.T) {
	cfg := DefaultRecencyConfig()
	in := []SearchResult{
		rr("a", 0.0330, 89, 90, false),
		rr("b", 0.0320, 89, 90, false),
		rr("c", 0.0300, 0, 90, false), // freshest, third by base score
	}
	got := applyRecency(in, cfg, rerankNow, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want the caller's limit of 2", len(got))
	}
	if got[0].ChunkID != "c" {
		t.Errorf("order = %v — the freshest chunk must survive the cut, which only happens "+
			"if the limit is applied after the re-sort", ids(got))
	}
}

// Determinism: equal final scores break on chunk id, preserving the existing
// contract (rag-retrieval-and-graph-design.md §3.1).
func TestApplyRecency_TiesBreakOnChunkID(t *testing.T) {
	cfg := DefaultRecencyConfig()
	in := []SearchResult{
		rr("zzz", 0.0300, 5, 90, false),
		rr("aaa", 0.0300, 5, 90, false),
	}
	got := applyRecency(in, cfg, rerankNow, 10)
	if !eq(ids(got), []string{"aaa", "zzz"}) {
		t.Errorf("order = %v, want [aaa zzz] — equal scores must break deterministically on id", ids(got))
	}
}

// overFetchLimit is the §5.5 arithmetic: min(L × factor, poolCap). The table in
// the design is the contract, including that it degenerates to 1.0 at L >= cap.
func TestOverFetchLimit_MatchesTheDesignTable(t *testing.T) {
	cfg := DefaultRecencyConfig()
	cases := []struct{ limit, want int }{
		{1, 3},
		{10, 30},
		{13, 39},
		{20, 40},   // capped: effective factor 2.0, not 3
		{40, 40},   // effective factor 1.0 — a pure re-sort
		{100, 100}, // never below the caller's own limit
	}
	for _, tc := range cases {
		if got := overFetchLimit(tc.limit, cfg); got != tc.want {
			t.Errorf("overFetchLimit(%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
}

func TestOverFetchLimit_DisabledReturnsTheCallerLimit(t *testing.T) {
	cfg := DefaultRecencyConfig()
	cfg.SetEnabled(false)
	if got := overFetchLimit(10, cfg); got != 10 {
		t.Errorf("overFetchLimit(10) disabled = %d, want 10 — the switch must revert the "+
			"over-fetch too, or 'byte-identical' is false", got)
	}
}
