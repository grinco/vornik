package membench

import (
	"math"
	"testing"
)

// Tier-2 metrics from 2026-08-10-memory-benchmark-harness-design.md §5.7 —
// judge-free retrieval quality. These are what CI gates on, so they must be
// deterministic, cheap, and correct at the edges. Naming follows the RAGAS
// vocabulary already used in rag-ingest-pipeline-design.md §2.6 rather than
// inventing new terms.

const eps = 1e-9

func near(a, b float64) bool { return math.Abs(a-b) < eps }

func TestContextRecall(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		gold      []string
		want      float64
	}{
		{"all gold found", []string{"a", "b", "c"}, []string{"a", "b"}, 1.0},
		{"half found", []string{"a", "x"}, []string{"a", "b"}, 0.5},
		{"none found", []string{"x", "y"}, []string{"a", "b"}, 0.0},
		{"exact match", []string{"a"}, []string{"a"}, 1.0},
		// Duplicates in the retrieved set must not inflate recall: retrieving
		// the same gold document twice is one document found, not two.
		{"duplicate retrieval", []string{"a", "a"}, []string{"a", "b"}, 0.5},
		// Duplicates in the GOLD set must not deflate it either.
		{"duplicate gold", []string{"a"}, []string{"a", "a"}, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextRecall(tc.retrieved, tc.gold); !near(got, tc.want) {
				t.Errorf("ContextRecall(%v, %v) = %v, want %v",
					tc.retrieved, tc.gold, got, tc.want)
			}
		})
	}
}

// TestContextRecall_NoGoldIsNaN — an item with no gold documents has no
// recall to measure. Returning 0 would drag the aggregate down and make an
// unlabelled item look like a retrieval failure; NaN makes it skippable and
// forces the aggregator to decide explicitly.
func TestContextRecall_NoGoldIsNaN(t *testing.T) {
	if got := ContextRecall([]string{"a"}, nil); !math.IsNaN(got) {
		t.Errorf("ContextRecall with no gold = %v, want NaN", got)
	}
}

func TestContextPrecision(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		gold      []string
		want      float64
	}{
		{"all relevant", []string{"a", "b"}, []string{"a", "b"}, 1.0},
		{"half relevant", []string{"a", "x"}, []string{"a", "b"}, 0.5},
		{"none relevant", []string{"x", "y"}, []string{"a"}, 0.0},
		// Precision is DOCUMENT-level on both sides, because that is the
		// granularity the gold labels use.
		//
		// An earlier version divided by the retrieved CHUNK count, reasoning that a
		// duplicated gold hit wastes a budget slot. The first live baseline showed
		// why that is wrong: with one gold document and an 8-chunk budget, precision
		// was pinned at 1/8 = 0.125 in every category no matter how good retrieval
		// was, because the ceiling is gold_docs/budget. It could not distinguish
		// "the gold document plus seven junk chunks" from "all eight chunks drawn
		// from the gold document" — a large quality difference — so a CI gate on it
		// would have sat at 0.125 forever.
		//
		// Wasted budget is a real concern, but it is budget efficiency, not
		// precision, and conflating them cost the metric its only job.
		{"repeated gold document counts once", []string{"a", "a"}, []string{"a"}, 1.0},
		{"one gold among several documents", []string{"a", "x", "y", "z"}, []string{"a"}, 0.25},
		{"multi-chunk retrieval from one gold document", []string{"a", "a", "a"}, []string{"a"}, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextPrecision(tc.retrieved, tc.gold); !near(got, tc.want) {
				t.Errorf("ContextPrecision(%v, %v) = %v, want %v",
					tc.retrieved, tc.gold, got, tc.want)
			}
		})
	}
}

// TestContextPrecision_NothingRetrievedIsNaN — precision over an empty result
// set is undefined, not perfect and not zero. Returning 1.0 would reward a
// system that retrieves nothing at all, which is the degenerate way to game
// this metric.
func TestContextPrecision_NothingRetrievedIsNaN(t *testing.T) {
	if got := ContextPrecision(nil, []string{"a"}); !math.IsNaN(got) {
		t.Errorf("ContextPrecision over an empty result = %v, want NaN", got)
	}
}

func TestMRR(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		gold      []string
		want      float64
	}{
		{"first position", []string{"a", "x"}, []string{"a"}, 1.0},
		{"second position", []string{"x", "a"}, []string{"a"}, 0.5},
		{"third position", []string{"x", "y", "a"}, []string{"a"}, 1.0 / 3.0},
		{"not found", []string{"x", "y"}, []string{"a"}, 0.0},
		// Rank of the FIRST gold hit, so a later second hit is irrelevant.
		{"earliest gold wins", []string{"x", "b", "a"}, []string{"a", "b"}, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MRR(tc.retrieved, tc.gold); !near(got, tc.want) {
				t.Errorf("MRR(%v, %v) = %v, want %v",
					tc.retrieved, tc.gold, got, tc.want)
			}
		})
	}
}

// TestMRR_NoGoldIsNaN — same reasoning as recall: nothing to rank against.
func TestMRR_NoGoldIsNaN(t *testing.T) {
	if got := MRR([]string{"a"}, nil); !math.IsNaN(got) {
		t.Errorf("MRR with no gold = %v, want NaN", got)
	}
}

// TestMeanIgnoringNaN — the aggregator must skip undefined values rather than
// propagate them. One unlabelled item would otherwise turn a whole category's
// score into NaN and read as a total failure.
func TestMeanIgnoringNaN(t *testing.T) {
	got, n := MeanIgnoringNaN([]float64{1.0, math.NaN(), 0.5})
	if !near(got, 0.75) {
		t.Errorf("mean = %v, want 0.75", got)
	}
	if n != 2 {
		t.Errorf("counted %d values, want 2 (the NaN must not count)", n)
	}
}

// TestMeanIgnoringNaN_AllNaN — no measurable values means no mean. Reporting 0
// would be indistinguishable from a real score of zero.
func TestMeanIgnoringNaN_AllNaN(t *testing.T) {
	got, n := MeanIgnoringNaN([]float64{math.NaN(), math.NaN()})
	if !math.IsNaN(got) {
		t.Errorf("mean of all-NaN = %v, want NaN", got)
	}
	if n != 0 {
		t.Errorf("counted %d values, want 0", n)
	}
}

// TestMeanIgnoringNaN_Empty — same contract for an empty slice.
func TestMeanIgnoringNaN_Empty(t *testing.T) {
	if got, n := MeanIgnoringNaN(nil); !math.IsNaN(got) || n != 0 {
		t.Errorf("mean of empty = %v (n=%d), want NaN (n=0)", got, n)
	}
}
