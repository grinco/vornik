package memory

import (
	"testing"
	"time"
)

// The opt-in half of slice 0b (design §4.2): a caller sets ParseQueryWindow and
// the searcher derives bounds from the query text — but an explicit bound is
// never second-guessed.

// TestResolveQueryWindow_OptInDerivesBounds — with the flag on and no explicit
// bounds, a temporal query yields a window.
func TestResolveQueryWindow_OptInDerivesBounds(t *testing.T) {
	opts := SearchOptions{ParseQueryWindow: true}
	got := resolveQueryWindow("what shipped in 2023?", opts, refNow)

	if !got.FromDate.Equal(day(2023, 1, 1)) {
		t.Errorf("FromDate = %v, want %v", got.FromDate, day(2023, 1, 1))
	}
	if !got.ToDate.Equal(endOf(2023, 12, 31)) {
		t.Errorf("ToDate = %v, want %v", got.ToDate, endOf(2023, 12, 31))
	}
}

// TestResolveQueryWindow_OffByDefault — the zero value must not parse. Deriving
// windows for every caller would silently narrow existing recalls, which is a
// behaviour change nobody asked for.
func TestResolveQueryWindow_OffByDefault(t *testing.T) {
	got := resolveQueryWindow("what shipped in 2023?", SearchOptions{}, refNow)
	if !got.FromDate.IsZero() || !got.ToDate.IsZero() {
		t.Errorf("zero SearchOptions derived a window %v..%v; want none",
			got.FromDate, got.ToDate)
	}
}

// TestResolveQueryWindow_ExplicitBoundsWin — a caller that passed dates has
// already decided. Overriding them from query prose would make the explicit
// argument unreliable, which is worse than not having the feature.
func TestResolveQueryWindow_ExplicitBoundsWin(t *testing.T) {
	from := day(2020, 6, 1)
	to := endOf(2020, 6, 30)
	opts := SearchOptions{ParseQueryWindow: true, FromDate: from, ToDate: to}

	got := resolveQueryWindow("what shipped in 2023?", opts, refNow)

	if !got.FromDate.Equal(from) || !got.ToDate.Equal(to) {
		t.Errorf("explicit bounds overridden: got %v..%v, want %v..%v",
			got.FromDate, got.ToDate, from, to)
	}
}

// TestResolveQueryWindow_PartialExplicitBoundWins — one bound set is still an
// explicit decision. Filling in the other from prose would produce a window the
// caller never expressed.
func TestResolveQueryWindow_PartialExplicitBoundWins(t *testing.T) {
	from := day(2020, 6, 1)
	opts := SearchOptions{ParseQueryWindow: true, FromDate: from}

	got := resolveQueryWindow("what shipped in 2023?", opts, refNow)

	if !got.FromDate.Equal(from) {
		t.Errorf("FromDate = %v, want the caller's %v", got.FromDate, from)
	}
	if !got.ToDate.IsZero() {
		t.Errorf("ToDate = %v, want it left unbounded", got.ToDate)
	}
}

// TestResolveQueryWindow_NonTemporalQueryUnchanged — the flag on a query with no
// date expression must leave the search unbounded.
func TestResolveQueryWindow_NonTemporalQueryUnchanged(t *testing.T) {
	opts := SearchOptions{ParseQueryWindow: true}
	got := resolveQueryWindow("how does the scheduler lease tasks?", opts, refNow)

	if !got.FromDate.IsZero() || !got.ToDate.IsZero() {
		t.Errorf("non-temporal query bounded to %v..%v; want unbounded",
			got.FromDate, got.ToDate)
	}
}

// TestResolveQueryWindow_PreservesOtherOptions — the helper returns a modified
// copy and must not drop unrelated fields on the way through.
func TestResolveQueryWindow_PreservesOtherOptions(t *testing.T) {
	opts := SearchOptions{
		ParseQueryWindow: true,
		Limit:            17,
		RepoScope:        "github.com/grinco/vornik",
		StrictScope:      true,
	}
	got := resolveQueryWindow("in 2023", opts, refNow)

	if got.Limit != 17 || got.RepoScope != opts.RepoScope || !got.StrictScope {
		t.Errorf("unrelated options mutated: %+v", got)
	}
	if got.FromDate.IsZero() {
		t.Error("window not applied")
	}
	_ = time.Time{}
}
