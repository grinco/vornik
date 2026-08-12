package memory

import (
	"testing"
	"time"
)

// Slice 0d of 2026-08-10-memory-benchmark-harness-design.md §4.4 — spreading
// selection across a window.
//
// The pathology: our corpus is written in batches. A RAG ingest lands a whole
// doc set under one timestamp, so a window query whose range covers that batch
// returns the batch and nothing else, even when the window spans a year.
//
// Selection is: per-bucket winner → winners SORTED BY SCORE → global fill.
// Step 2 being score-ordered rather than time-ordered is what makes the
// guarantee below unconditional; round-1 review (review-20260810-2989) caught
// that a time-ordered version drops the global best whenever the budget is
// smaller than the bucket count.

func TestWindowBuckets_Count(t *testing.T) {
	cases := []struct {
		name string
		days int
		want int
	}{
		{"one day", 1, 1},
		{"one week", 7, 1},
		{"two weeks", 14, 2},
		{"one month", 30, 5},
		{"one year caps at 8", 365, 8},
		{"ten years still caps", 3650, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := day(2024, 1, 1)
			to := from.AddDate(0, 0, tc.days)
			if got := windowBuckets(from, to); got != tc.want {
				t.Errorf("windowBuckets(%d days) = %d, want %d", tc.days, got, tc.want)
			}
		})
	}
}

// TestSpreadAcrossWindow_GlobalBestAtBudgetOne is THE regression anchor for the
// round-1 finding. Every bucket is populated and the budget is 1, which is the
// exact configuration under which the original time-ordered design dropped the
// best chunk. The global best sits in a LATE bucket so a time-ordered
// implementation would emit an early-bucket chunk instead and fail here.
func TestSpreadAcrossWindow_GlobalBestAtBudgetOne(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1) // 60 days → 8 buckets
	in := []SearchResult{
		{ChunkID: "early-weak", Score: 0.10, EventTime: day(2024, 1, 2)},
		{ChunkID: "early-mid", Score: 0.20, EventTime: day(2024, 1, 10)},
		{ChunkID: "mid", Score: 0.30, EventTime: day(2024, 1, 20)},
		{ChunkID: "mid2", Score: 0.40, EventTime: day(2024, 1, 30)},
		{ChunkID: "mid3", Score: 0.50, EventTime: day(2024, 2, 5)},
		{ChunkID: "late", Score: 0.60, EventTime: day(2024, 2, 12)},
		{ChunkID: "later", Score: 0.70, EventTime: day(2024, 2, 20)},
		{ChunkID: "GLOBAL-BEST", Score: 0.99, EventTime: day(2024, 2, 27)},
	}

	got := spreadAcrossWindow(in, from, to, 1)

	if len(got) != 1 {
		t.Fatalf("budget 1 returned %d results", len(got))
	}
	if got[0].ChunkID != "GLOBAL-BEST" {
		t.Errorf("budget=1 returned %q; the global best must never be dropped "+
			"in favour of temporal spread (design §4.4)", got[0].ChunkID)
	}
}

// TestSpreadAcrossWindow_OnePerPopulatedBucket — when the budget allows it, the
// selection covers the window instead of clustering on the densest stretch.
func TestSpreadAcrossWindow_OnePerPopulatedBucket(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1)

	// A dense batch at the start (the RAG-ingest pathology) plus two lone
	// chunks later. Without spreading, the batch would fill any budget.
	in := []SearchResult{
		{ChunkID: "batch1", Score: 0.90, EventTime: day(2024, 1, 2)},
		{ChunkID: "batch2", Score: 0.89, EventTime: day(2024, 1, 2)},
		{ChunkID: "batch3", Score: 0.88, EventTime: day(2024, 1, 2)},
		{ChunkID: "batch4", Score: 0.87, EventTime: day(2024, 1, 2)},
		{ChunkID: "lonely-mid", Score: 0.40, EventTime: day(2024, 2, 1)},
		{ChunkID: "lonely-late", Score: 0.30, EventTime: day(2024, 2, 25)},
	}

	got := spreadAcrossWindow(in, from, to, 3)

	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ChunkID] = true
	}
	if !ids["lonely-mid"] || !ids["lonely-late"] {
		var names []string
		for _, r := range got {
			names = append(names, r.ChunkID)
		}
		t.Errorf("selection %v missed a sparse bucket; a dense batch swamped the "+
			"window, which is the exact pathology §4.4 fixes", names)
	}
}

// TestSpreadAcrossWindow_EmptyBucketsSkippedNotPadded — a window over a genuinely
// sparse period must not be padded with weak hits to fill bucket slots.
func TestSpreadAcrossWindow_EmptyBucketsSkippedNotPadded(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1)
	in := []SearchResult{
		{ChunkID: "only", Score: 0.5, EventTime: day(2024, 1, 5)},
	}

	got := spreadAcrossWindow(in, from, to, 8)

	if len(got) != 1 {
		t.Errorf("got %d results from a 1-chunk corpus; empty buckets must be "+
			"skipped, not filled", len(got))
	}
}

// TestSpreadAcrossWindow_FillsRemainingBudgetFromGlobalRanking — after one per
// bucket, leftover budget goes to the next-best chunks overall, so a generous
// budget is not artificially capped at the bucket count.
//
// The budget must sit strictly between the bucket count (2) and the corpus size
// (5), or the function short-circuits on budget >= len(results) and this never
// reaches step 3 at all. An earlier version of this test used budget == len and
// therefore asserted nothing about the fill path it names.
func TestSpreadAcrossWindow_FillsRemainingBudgetFromGlobalRanking(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 15) // 14 days → 2 buckets
	in := []SearchResult{
		{ChunkID: "a", Score: 0.90, EventTime: day(2024, 1, 2)},
		{ChunkID: "b", Score: 0.80, EventTime: day(2024, 1, 3)},
		{ChunkID: "c", Score: 0.70, EventTime: day(2024, 1, 10)},
		{ChunkID: "d", Score: 0.60, EventTime: day(2024, 1, 11)},
		{ChunkID: "e", Score: 0.50, EventTime: day(2024, 1, 12)},
	}

	got := spreadAcrossWindow(in, from, to, 3)

	if len(got) != 3 {
		t.Fatalf("budget 3 returned %d results", len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.ChunkID] {
			t.Errorf("duplicate %q in selection", r.ChunkID)
		}
		seen[r.ChunkID] = true
	}
	// Step 1 takes a (bucket 0 best) and c (bucket 1 best); step 3 then fills
	// the last slot with the best remaining overall, which is b at 0.80.
	if !seen["a"] || !seen["c"] {
		t.Errorf("bucket winners missing from %v", seen)
	}
	if !seen["b"] {
		t.Errorf("global fill picked something other than the best remaining "+
			"chunk (b, 0.80); got %v", seen)
	}
}

// TestSpreadAcrossWindow_BudgetAtLeastCorpusIsIdentity — nothing to select when
// the budget already covers everything, so skip the work entirely.
func TestSpreadAcrossWindow_BudgetAtLeastCorpusIsIdentity(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1)
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5, EventTime: day(2024, 1, 5)},
		{ChunkID: "b", Score: 0.9, EventTime: day(2024, 2, 5)},
	}
	for _, budget := range []int{2, 3, 99} {
		got := spreadAcrossWindow(in, from, to, budget)
		if len(got) != 2 || got[0].ChunkID != "a" {
			t.Errorf("budget %d altered a fully-covered set: %+v", budget, got)
		}
	}
}

// TestSpreadAcrossWindow_InvertedWindowIsIdentity — a from after to cannot be
// bucketed; return the input rather than dividing by a negative span.
func TestSpreadAcrossWindow_InvertedWindowIsIdentity(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5, EventTime: day(2024, 1, 5)},
		{ChunkID: "b", Score: 0.9, EventTime: day(2024, 2, 5)},
		{ChunkID: "c", Score: 0.7, EventTime: day(2024, 2, 6)},
	}
	got := spreadAcrossWindow(in, day(2024, 3, 1), day(2024, 1, 1), 1)
	if len(got) != 3 {
		t.Errorf("inverted window returned %d results, want the input untouched", len(got))
	}
}

// TestSpreadAcrossWindow_UpperEdgeChunkBucketed — a chunk stamped exactly on the
// window's upper bound must land in the last bucket, not overflow past it.
func TestSpreadAcrossWindow_UpperEdgeChunkBucketed(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 15) // 2 buckets
	in := []SearchResult{
		{ChunkID: "low", Score: 0.9, EventTime: from},
		{ChunkID: "edge", Score: 0.4, EventTime: to},
		{ChunkID: "filler", Score: 0.1, EventTime: day(2024, 1, 3)},
	}

	got := spreadAcrossWindow(in, from, to, 2)

	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ChunkID] = true
	}
	if !ids["edge"] {
		t.Error("chunk on the upper window bound was not selected as its bucket's " +
			"winner — the boundary index likely overflowed past the last bucket")
	}
}

// TestSpreadAcrossWindow_TieBreakIsDeterministic — bucket winners are collected
// from a map, whose iteration order Go randomises. Equal scores must still
// produce a stable selection, or the same query returns different results run to
// run and the benchmark's numbers become noise.
func TestSpreadAcrossWindow_TieBreakIsDeterministic(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1)
	in := []SearchResult{
		{ChunkID: "b0", Score: 0.5, EventTime: day(2024, 1, 2)},
		{ChunkID: "b1", Score: 0.5, EventTime: day(2024, 1, 12)},
		{ChunkID: "b2", Score: 0.5, EventTime: day(2024, 1, 22)},
		{ChunkID: "b3", Score: 0.5, EventTime: day(2024, 2, 2)},
		{ChunkID: "b4", Score: 0.5, EventTime: day(2024, 2, 12)},
	}

	first := spreadAcrossWindow(append([]SearchResult(nil), in...), from, to, 2)
	for i := 0; i < 30; i++ {
		got := spreadAcrossWindow(append([]SearchResult(nil), in...), from, to, 2)
		for j := range got {
			if got[j].ChunkID != first[j].ChunkID {
				t.Fatalf("selection is not deterministic across runs: %q vs %q at %d",
					got[j].ChunkID, first[j].ChunkID, j)
			}
		}
	}
}

// TestSpreadAcrossWindow_UnknownEventTimeStillEligible — most of the corpus has
// no event time. Those chunks belong to no bucket, but excluding them from the
// global fill would make windowed recall silently blind to everything ingested
// before migration 157.
func TestSpreadAcrossWindow_UnknownEventTimeStillEligible(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1)
	in := []SearchResult{
		{ChunkID: "dated", Score: 0.50, EventTime: day(2024, 1, 5)},
		{ChunkID: "undated-strong", Score: 0.95},
	}

	got := spreadAcrossWindow(in, from, to, 2)

	found := false
	for _, r := range got {
		if r.ChunkID == "undated-strong" {
			found = true
		}
	}
	if !found {
		t.Error("a high-scoring chunk with unknown event time was dropped; " +
			"pre-migration chunks must stay reachable under a window")
	}
}

// TestSpreadAcrossWindow_NoWindowIsIdentity — no window, no buckets, no change.
func TestSpreadAcrossWindow_NoWindowIsIdentity(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5},
		{ChunkID: "b", Score: 0.9},
	}
	got := spreadAcrossWindow(in, time.Time{}, time.Time{}, 1)

	if len(got) != 2 || got[0].ChunkID != "a" {
		t.Errorf("unwindowed input altered: %+v", got)
	}
}

// TestSpreadAcrossWindow_BudgetZeroOrNegativeIsIdentity — a caller that supplied
// no budget gets everything, not nothing. Returning an empty slice would look
// like an empty corpus.
func TestSpreadAcrossWindow_BudgetZeroOrNegativeIsIdentity(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 3, 1)
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5, EventTime: day(2024, 1, 5)},
		{ChunkID: "b", Score: 0.9, EventTime: day(2024, 2, 5)},
	}
	for _, budget := range []int{0, -1} {
		got := spreadAcrossWindow(in, from, to, budget)
		if len(got) != 2 {
			t.Errorf("budget %d returned %d results, want all 2", budget, len(got))
		}
	}
}

// TestWindowBuckets_DegenerateSpans — a zero-width or inverted window has no
// meaningful division; return one bucket rather than zero or a negative count,
// either of which would make the caller's modulo arithmetic misbehave.
func TestWindowBuckets_DegenerateSpans(t *testing.T) {
	d := day(2024, 1, 1)
	if got := windowBuckets(d, d); got != 1 {
		t.Errorf("windowBuckets(zero span) = %d, want 1", got)
	}
	if got := windowBuckets(day(2024, 3, 1), day(2024, 1, 1)); got != 1 {
		t.Errorf("windowBuckets(inverted) = %d, want 1", got)
	}
	// Sub-day span: ceil rounds up to one bucket, never zero.
	if got := windowBuckets(d, d.Add(time.Hour)); got != 1 {
		t.Errorf("windowBuckets(1h) = %d, want 1", got)
	}
}
