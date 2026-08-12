package memory

import (
	"sort"
	"time"
)

// Slice 0c of https://docs.vornik.io
// §4.3 — temporal proximity as a scoring signal.
//
// When a search is bounded to a window, a chunk nearer the window's centre is
// slightly more likely to be what was meant: someone asking about "March 2024"
// is usually after the middle of that story, not its first or last day. But it
// is only a nudge. The signal REORDERS and never EXCLUDES, which is why it
// multiplies the score rather than filtering the set, and why the coefficient
// is deliberately small enough that it cannot overturn a real relevance gap.
//
//	score = base × (1 + β × proximity),   proximity ∈ [0,1]

// defaultTemporalBeta is the proximity coefficient. Small on purpose: at 0.1 a
// perfectly-centred chunk gains 10%, which settles ties and near-ties and
// cannot overturn a 0.20 relevance gap. Raising it far past this turns a
// ranking hint into a de-facto recency filter — the thing §4.3 exists to avoid.
const defaultTemporalBeta = 0.1

// temporalProximity scores how close ts sits to the centre of [from, to], as a
// triangular falloff: 1.0 at the centre, 0 at either edge.
//
// Returns 0 — no boost and no penalty — for the cases where the notion doesn't
// apply: an unknown (zero) event time, a timestamp outside the window, and a
// degenerate window. Unknown especially must not read as "far away": most of
// the corpus has no event time, and penalising it would silently reorder
// everything (design §4.1.1's concern, in the ranking rather than the filter).
func temporalProximity(ts, from, to time.Time) float64 {
	if ts.IsZero() || from.IsZero() || to.IsZero() {
		return 0
	}
	span := to.Sub(from)
	if span <= 0 {
		return 0
	}
	if ts.Before(from) || ts.After(to) {
		return 0
	}
	centre := from.Add(span / 2)
	// Distance from centre, normalised against the half-span, so the value
	// reaches 1 at the centre and 0 at both edges.
	dist := ts.Sub(centre)
	if dist < 0 {
		dist = -dist
	}
	half := span / 2
	if half <= 0 {
		return 0
	}
	p := 1 - float64(dist)/float64(half)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// applyTemporalProximity rescales each result by its proximity to the window
// centre and re-sorts. Returns the input unchanged when there is no window or
// beta is zero.
//
// beta == 0 reproducing the input ordering exactly is the regression anchor for
// this slice: it means the feature can ship dark and be enabled independently
// of the code landing.
func applyTemporalProximity(results []SearchResult, from, to time.Time, beta float64) []SearchResult {
	if beta == 0 || from.IsZero() || to.IsZero() || len(results) < 2 {
		return results
	}
	for i := range results {
		p := temporalProximity(results[i].EventTime, from, to)
		if p == 0 {
			continue
		}
		results[i].Score *= 1 + beta*p
	}
	// Stable, so equal scores keep their incoming relative order rather than
	// being shuffled by the sort — the incoming order is the fused relevance
	// ranking and is meaningful.
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

// SetTemporalBeta wires the proximity coefficient for windowed searches
// (design §4.3). 0 disables, and disabled reproduces the pre-slice ordering
// exactly. Nil-safe.
//
// Pass defaultTemporalBeta for the calibrated value; anything much larger turns
// a ranking hint into a recency filter.
func (s *Searcher) SetTemporalBeta(beta float64) {
	if s != nil {
		s.temporalBeta = beta
	}
}

// SetSpreadWindow enables bucket-spread selection over a bounded window
// (design §4.4). Off by default because it changes WHICH results a windowed
// search returns — an opt-in behaviour change, not a silent one. Nil-safe.
func (s *Searcher) SetSpreadWindow(on bool) {
	if s != nil {
		s.spreadWindow = on
	}
}
