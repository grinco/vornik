package memory

import (
	"math"
	"sort"
	"time"
)

// Slice 0d of https://docs.vornik.io
// §4.4 — spreading selection across a time window.
//
// The pathology this fixes is specific to how our corpus is written: RAG ingest
// lands a whole document set in one pass, under one timestamp. A window query
// whose range covers that batch therefore returns the batch and nothing else,
// even when the window spans a year — "what happened in 2024" answers with
// whichever week we happened to bulk-import.

// maxWindowBuckets caps the division. Beyond about eight slices the spread stops
// buying coverage and starts costing relevance, because each additional bucket
// forces one more merely-locally-best chunk into a fixed budget.
const maxWindowBuckets = 8

// bucketDays is the target width of one bucket. A week is the granularity at
// which project work actually clusters, so it keeps a month's window at ~4-5
// slices rather than 30.
const bucketDays = 7

// windowBuckets returns how many slices to divide [from, to] into:
// min(maxWindowBuckets, ceil(days/bucketDays)), and never less than one.
func windowBuckets(from, to time.Time) int {
	span := to.Sub(from)
	if span <= 0 {
		return 1
	}
	days := span.Hours() / 24
	n := int(math.Ceil(days / bucketDays))
	if n < 1 {
		return 1
	}
	if n > maxWindowBuckets {
		return maxWindowBuckets
	}
	return n
}

// spreadAcrossWindow selects up to budget results, covering the window rather
// than clustering on its densest stretch.
//
// Three steps, in this order:
//
//  1. Take the highest-scoring chunk from each POPULATED bucket. Empty buckets
//     are skipped, never padded — a genuinely sparse period should return few
//     results, not weak ones.
//  2. Emit those winners in SCORE order until the budget is spent.
//  3. Fill any remaining budget from the global ranking, skipping chunks already
//     selected.
//
// Step 2 is load-bearing and was wrong in the design's first draft, which
// emitted winners in TIME order. Round-1 review found the consequence: with a
// budget smaller than the bucket count, time-ordered emission stops partway
// through the window and never reaches step 3, so the single most relevant chunk
// is dropped whenever it sits in a late bucket.
//
// Score-ordering makes the guarantee unconditional rather than caveated. The
// global best is necessarily the top-scoring chunk of whichever bucket contains
// it, so it always survives step 1; and being the highest-scoring winner, it is
// always emitted first in step 2 — at any budget, including 1.
//
// Chunks with an unknown (zero) event time belong to no bucket but remain
// eligible in step 3. Excluding them would make windowed recall blind to
// everything ingested before migration 157, which is most of the corpus.
func spreadAcrossWindow(results []SearchResult, from, to time.Time, budget int) []SearchResult {
	if from.IsZero() || to.IsZero() || budget <= 0 || len(results) <= 1 {
		return results
	}
	span := to.Sub(from)
	if span <= 0 || budget >= len(results) {
		return results
	}

	winners := bucketWinners(results, from, span, windowBuckets(from, to))

	picked := make([]SearchResult, 0, budget)
	taken := make(map[int]bool, budget)
	for _, i := range winners {
		if len(picked) == budget {
			break
		}
		picked = append(picked, results[i])
		taken[i] = true
	}
	if len(picked) < budget {
		picked = fillFromGlobalRanking(results, picked, taken, budget)
	}
	return picked
}

// bucketIndexOf returns which slice of the window ts falls into, or -1 when it
// belongs to none: an unknown (zero) event time, or a timestamp outside the
// window. Undated chunks are the common case and are handled by the global fill,
// not here.
func bucketIndexOf(ts, from time.Time, span time.Duration, n int) int {
	if ts.IsZero() || ts.Before(from) || ts.After(from.Add(span)) {
		return -1
	}
	idx := int(float64(n) * float64(ts.Sub(from)) / float64(span))
	if idx >= n {
		// A timestamp exactly on the upper bound lands on n; clamp it into the
		// last bucket rather than overflowing past the end.
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}

// bucketWinners returns the indices of the highest-scoring result in each
// populated bucket, ordered BY SCORE (not by time).
//
// The score ordering is what makes the §4.4 guarantee unconditional: the global
// best is necessarily its own bucket's winner, and being the highest-scoring
// winner it is emitted first at any budget, including 1. Round-1 review
// (review-20260810-2989) found that ordering these by time drops the global best
// whenever the budget is smaller than the bucket count.
func bucketWinners(results []SearchResult, from time.Time, span time.Duration, n int) []int {
	bestIdx := make(map[int]int, n)
	for i := range results {
		b := bucketIndexOf(results[i].EventTime, from, span, n)
		if b < 0 {
			continue
		}
		if cur, ok := bestIdx[b]; !ok || results[i].Score > results[cur].Score {
			bestIdx[b] = i
		}
	}
	winners := make([]int, 0, len(bestIdx))
	for _, i := range bestIdx {
		winners = append(winners, i)
	}
	sortByScoreThenIndex(results, winners)
	return winners
}

// fillFromGlobalRanking tops picked up to budget with the best not-yet-selected
// results overall, so a generous budget is not artificially capped at the bucket
// count. This is also the only path by which chunks with an unknown event time
// are reachable under a window — excluding them would make windowed recall blind
// to everything ingested before migration 157, which is most of the corpus.
func fillFromGlobalRanking(results []SearchResult, picked []SearchResult, taken map[int]bool, budget int) []SearchResult {
	order := make([]int, len(results))
	for i := range order {
		order[i] = i
	}
	sortByScoreThenIndex(results, order)
	for _, i := range order {
		if len(picked) == budget {
			break
		}
		if taken[i] {
			continue
		}
		picked = append(picked, results[i])
		taken[i] = true
	}
	return picked
}

// sortByScoreThenIndex orders idx descending by the score of results[idx],
// breaking ties on the index itself.
//
// The tie-break is not cosmetic: bucket winners are collected from a map, and Go
// randomises map iteration order. Without a deterministic tie-break the same
// query returns different results run to run, which would turn the benchmark's
// numbers into noise.
func sortByScoreThenIndex(results []SearchResult, idx []int) {
	sort.SliceStable(idx, func(a, b int) bool {
		if results[idx[a]].Score != results[idx[b]].Score {
			return results[idx[a]].Score > results[idx[b]].Score
		}
		return idx[a] < idx[b]
	})
}
