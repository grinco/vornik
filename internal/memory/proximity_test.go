package memory

import (
	"math"
	"testing"
	"time"
)

// Slice 0c of 2026-08-10-memory-benchmark-harness-design.md §4.3.
//
// Inside a window, a chunk nearer the window's centre gets a small boost. It is
// a SIGNAL, not a filter: it reorders, never excludes.
//
//	score = base × (1 + β × proximity),  proximity ∈ [0,1]

const eps = 1e-9

// TestTemporalProximity_CentreIsOne — the centre of the window is maximally
// proximate.
func TestTemporalProximity_CentreIsOne(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	centre := day(2024, 1, 6)
	if got := temporalProximity(centre, from, to); math.Abs(got-1.0) > eps {
		t.Errorf("proximity at centre = %v, want 1.0", got)
	}
}

// TestTemporalProximity_EdgesAreZero — both ends fall off to zero.
func TestTemporalProximity_EdgesAreZero(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	if got := temporalProximity(from, from, to); math.Abs(got) > eps {
		t.Errorf("proximity at lower edge = %v, want 0", got)
	}
	if got := temporalProximity(to, from, to); math.Abs(got) > eps {
		t.Errorf("proximity at upper edge = %v, want 0", got)
	}
}

// TestTemporalProximity_Symmetric — equal distances either side of centre score
// equally. An asymmetric falloff would quietly bias toward one end of every
// window.
func TestTemporalProximity_Symmetric(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	before := temporalProximity(day(2024, 1, 4), from, to)
	after := temporalProximity(day(2024, 1, 8), from, to)
	if math.Abs(before-after) > eps {
		t.Errorf("asymmetric falloff: %v vs %v", before, after)
	}
}

// TestTemporalProximity_ZeroTimeIsZero — an unknown event time gets neither
// boost nor penalty. Treating "unknown" as "far from centre" would push every
// legacy chunk down the ranking, which is a silent corpus-wide reordering.
func TestTemporalProximity_ZeroTimeIsZero(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	if got := temporalProximity(time.Time{}, from, to); got != 0 {
		t.Errorf("proximity for unknown event time = %v, want 0", got)
	}
}

// TestTemporalProximity_OutsideWindowIsZero — clamped, not negative. A negative
// proximity would turn the boost into a penalty and make the signal a de-facto
// filter, which §4.3 explicitly forbids.
func TestTemporalProximity_OutsideWindowIsZero(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	for _, ts := range []time.Time{day(2023, 12, 1), day(2024, 3, 1)} {
		if got := temporalProximity(ts, from, to); got != 0 {
			t.Errorf("proximity outside window (%v) = %v, want 0", ts, got)
		}
	}
}

// TestTemporalProximity_DegenerateWindow — a zero-width or inverted window must
// not divide by zero.
func TestTemporalProximity_DegenerateWindow(t *testing.T) {
	d := day(2024, 1, 1)
	if got := temporalProximity(d, d, d); got != 0 {
		t.Errorf("zero-width window proximity = %v, want 0", got)
	}
	if got := temporalProximity(d, day(2024, 2, 1), day(2024, 1, 1)); got != 0 {
		t.Errorf("inverted window proximity = %v, want 0", got)
	}
}

// TestApplyTemporalProximity_BetaZeroIsIdentity is the regression anchor named
// in the design: with β = 0 the ordering must be byte-identical to the
// pre-slice behaviour. This is what makes the feature safe to ship dark.
func TestApplyTemporalProximity_BetaZeroIsIdentity(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	in := []SearchResult{
		{ChunkID: "a", Score: 0.9, EventTime: day(2024, 1, 6)},
		{ChunkID: "b", Score: 0.8, EventTime: day(2024, 1, 2)},
		{ChunkID: "c", Score: 0.7, EventTime: time.Time{}},
	}
	want := append([]SearchResult(nil), in...)

	got := applyTemporalProximity(in, from, to, 0)

	if len(got) != len(want) {
		t.Fatalf("length changed: %d vs %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ChunkID != want[i].ChunkID || got[i].Score != want[i].Score {
			t.Errorf("β=0 altered result %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestApplyTemporalProximity_ReordersOnTie — the point of the signal: with equal
// base scores, the chunk closer to the window centre wins.
func TestApplyTemporalProximity_ReordersOnTie(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	in := []SearchResult{
		{ChunkID: "edge", Score: 0.5, EventTime: day(2024, 1, 2)},
		{ChunkID: "centre", Score: 0.5, EventTime: day(2024, 1, 6)},
	}

	got := applyTemporalProximity(in, from, to, 0.1)

	if got[0].ChunkID != "centre" {
		t.Errorf("order = %s,%s; want centre first", got[0].ChunkID, got[1].ChunkID)
	}
}

// TestApplyTemporalProximity_CannotOvertakeOnBigGap — β is small on purpose. A
// proximity boost must not overturn a large relevance gap, or the signal has
// become a filter by the back door.
func TestApplyTemporalProximity_CannotOvertakeOnBigGap(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 1, 11)
	in := []SearchResult{
		{ChunkID: "relevant-but-edge", Score: 0.90, EventTime: day(2024, 1, 1)},
		{ChunkID: "weak-but-central", Score: 0.70, EventTime: day(2024, 1, 6)},
	}

	got := applyTemporalProximity(in, from, to, defaultTemporalBeta)

	if got[0].ChunkID != "relevant-but-edge" {
		t.Errorf("proximity overturned a 0.20 relevance gap at β=%v; order = %s,%s",
			defaultTemporalBeta, got[0].ChunkID, got[1].ChunkID)
	}
}

// TestApplyTemporalProximity_NoWindowIsIdentity — with no window there is no
// centre, so there is nothing to be proximate to. Identity means the slice comes
// back exactly as supplied: this function's job is the proximity signal, and it
// must not take it upon itself to re-sort a caller's results as a side effect.
// The deliberately unsorted input is the point.
func TestApplyTemporalProximity_NoWindowIsIdentity(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5, EventTime: day(2024, 1, 2)},
		{ChunkID: "b", Score: 0.6, EventTime: day(2024, 1, 6)},
	}
	got := applyTemporalProximity(in, time.Time{}, time.Time{}, 0.5)

	if got[0].ChunkID != "a" || got[1].ChunkID != "b" {
		t.Errorf("unwindowed search reordered: %s,%s; want the input order a,b",
			got[0].ChunkID, got[1].ChunkID)
	}
	if got[0].Score != 0.5 || got[1].Score != 0.6 {
		t.Errorf("unwindowed search rescored: %v, %v; want 0.5, 0.6",
			got[0].Score, got[1].Score)
	}
}

// TestTemporalProximity_SubNanosecondWindow — a window so narrow that its
// half-span truncates to zero must not divide by zero.
func TestTemporalProximity_SubNanosecondWindow(t *testing.T) {
	from := day(2024, 1, 1)
	to := from.Add(1) // 1ns span → half-span truncates to 0
	if got := temporalProximity(from, from, to); got != 0 {
		t.Errorf("proximity in a 1ns window = %v, want 0 (no division by zero)", got)
	}
}

// TestTemporalProximity_AlwaysInUnitRange — whatever the window and timestamp,
// the signal stays in [0,1]. Out of range, it would either invert the boost into
// a penalty or amplify a score without bound.
func TestTemporalProximity_AlwaysInUnitRange(t *testing.T) {
	from, to := day(2024, 1, 1), day(2024, 12, 31)
	for d := 0; d < 400; d += 7 {
		ts := from.AddDate(0, 0, d)
		p := temporalProximity(ts, from, to)
		if p < 0 || p > 1 {
			t.Fatalf("proximity(%v) = %v, outside [0,1]", ts, p)
		}
	}
}
