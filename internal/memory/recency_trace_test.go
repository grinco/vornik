package memory

import (
	"testing"
	"time"
)

// §5.7's observability contract. The re-rank changes what an operator sees, so
// a ranking regression has to be diagnosable AFTER the fact rather than
// reproducible only by argument.
//
// The two fields that matter most are the ones §5.5 forced: reporting the
// CONFIGURED over-fetch factor on a query where the candidate cap made it 1.0
// would be "a number with no scope reads as a guarantee" — the trace has to
// carry the effective pool, not the intent.

func TestRecencyTraceParams_ReportsEffectivePoolNotTheConfiguredFactor(t *testing.T) {
	cfg := DefaultRecencyConfig()

	// A small limit: the configured factor applies in full.
	small := recencyTraceParams(cfg, 5, 0, 0, false)
	if small["over_fetch_configured"] != 3 {
		t.Errorf("over_fetch_configured = %v, want 3", small["over_fetch_configured"])
	}
	if small["pool_size_effective"] != 15 {
		t.Errorf("pool_size_effective = %v, want 15 (5 x 3, under the cap)", small["pool_size_effective"])
	}
	if small["pool_cap_clipped"] != false {
		t.Error("a query under the cap must not report clipping")
	}

	// A limit at the cap: the factor is 1.0 in reality, and the trace must
	// say so rather than repeating the configured 3.
	big := recencyTraceParams(cfg, 40, 0, 0, true)
	if big["pool_size_effective"] != 40 {
		t.Errorf("pool_size_effective = %v, want 40 (clipped by the per-arm cap)", big["pool_size_effective"])
	}
	if big["pool_cap_clipped"] != true {
		t.Error("a query clipped by the candidate cap must SAY so; reporting the configured " +
			"factor here would claim reach the mechanism does not have")
	}
}

func TestRecencyTraceParams_CarriesTheTuningInputs(t *testing.T) {
	cfg := DefaultRecencyConfig()
	p := recencyTraceParams(cfg, 10, 4, 2, false)
	if p["reordered_count"] != 4 {
		t.Errorf("reordered_count = %v, want 4", p["reordered_count"])
	}
	if p["top_shift"] != 2 {
		t.Errorf("top_shift = %v, want 2", p["top_shift"])
	}
	if p["enabled"] != true {
		t.Error("the trace must record whether the feature was on for this query")
	}
	// The floor is computed, not configured (§5.3b) — the trace records the
	// value actually used, or a tuning session is reading the wrong number.
	if f, ok := p["min_freshness"].(float64); !ok || f < 0.39 || f > 0.41 {
		t.Errorf("min_freshness = %v, want the computed ~0.40", p["min_freshness"])
	}
}

// countReordered is what feeds reordered_count and top_shift. Both are computed
// against the PRE-rerank order, so a no-op re-rank reports zeros and is
// distinguishable from a disabled one (which writes no row at all).
func TestCountReordered_MeasuresAgainstTheOriginalOrder(t *testing.T) {
	now := time.Now().UTC()
	before := []SearchResult{
		{ChunkID: "a", Score: 0.3, CreatedAt: now},
		{ChunkID: "b", Score: 0.2, CreatedAt: now},
		{ChunkID: "c", Score: 0.1, CreatedAt: now},
	}
	// Unchanged order → nothing to report.
	if n, shift := countReordered(before, before); n != 0 || shift != 0 {
		t.Errorf("identical order: reordered=%d shift=%d, want 0/0", n, shift)
	}
	// c jumps to the front: three positions change, and the new rank-1 moved
	// two places.
	after := []SearchResult{before[2], before[0], before[1]}
	n, shift := countReordered(before, after)
	if n != 3 {
		t.Errorf("reordered = %d, want 3", n)
	}
	if shift != 2 {
		t.Errorf("top_shift = %d, want 2 — the new rank-1 came from index 2", shift)
	}
}

// A shorter result set after the cut must not be read as a reorder of the tail.
func TestCountReordered_HandlesTheCut(t *testing.T) {
	now := time.Now().UTC()
	before := []SearchResult{
		{ChunkID: "a", Score: 0.3, CreatedAt: now},
		{ChunkID: "b", Score: 0.2, CreatedAt: now},
		{ChunkID: "c", Score: 0.1, CreatedAt: now},
	}
	after := []SearchResult{before[0]} // over-fetched, then cut to 1
	if n, shift := countReordered(before, after); n != 0 || shift != 0 {
		t.Errorf("cut-only: reordered=%d shift=%d, want 0/0 — truncation is not reordering", n, shift)
	}
}
