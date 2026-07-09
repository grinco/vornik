package textsim

import "testing"

// TestJaccard locks in the threshold tuning that dispatcher's fuzzy
// dedup relies on. These cases are ported verbatim from
// internal/dispatcher/tools_handlers_test.go's
// TestJaccardTokenSimilarity, which pinned production cases (the
// T-af29 / T-6e62 incident from 2026-05-10) without merging
// genuinely-different requests.
func TestJaccard(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		// want is a tight expected range so an over-eager change
		// to the function's metric breaks the test.
		min, max float64
	}{
		{
			name: "identical_strings_return_1",
			a:    "ingest the cv content into project memory",
			b:    "ingest the cv content into project memory",
			min:  1.0, max: 1.0,
		},
		{
			name: "near_dup_with_one_inserted_phrase",
			a:    "ingest toby sheldon's cv content into project memory if any use tools from the swarm memory module",
			b:    "ingest toby sheldon's cv content into project memory use tools from the swarm memory module",
			min:  0.85, max: 1.0,
		},
		{
			name: "completely_different_requests_low_similarity",
			a:    "build a python web scraper for ebay listings",
			b:    "summarise the latest pull requests on github",
			min:  0.0, max: 0.20,
		},
		{
			name: "empty_input_returns_zero",
			a:    "", b: "anything at all",
			min: 0.0, max: 0.0,
		},
		{
			name: "subset_string_lower_than_threshold",
			a:    "scout the project for unused dependencies",
			b:    "list every package in go.mod and remove ones not imported anywhere",
			min:  0.0, max: 0.30,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Jaccard(c.a, c.b)
			if got < c.min || got > c.max {
				t.Errorf("Jaccard(%q, %q) = %v; want in [%v, %v]",
					c.a, c.b, got, c.min, c.max)
			}
		})
	}
}

// TestJaccard_DisjointIsZero ports
// dispatcher's TestJaccard_DisjointIsZero.
func TestJaccard_DisjointIsZero(t *testing.T) {
	if got := Jaccard("a b c", "d e f"); got != 0 {
		t.Errorf("expected 0 for disjoint sets, got %v", got)
	}
}

// TestJaccard_EmptyAfterTokenStrip ports
// dispatcher's TestJaccard_EmptyAfterTokenStrip: tokens of pure
// punctuation strip to empty so the metric returns 0 even though
// the strings themselves are non-empty.
func TestJaccard_EmptyAfterTokenStrip(t *testing.T) {
	if got := Jaccard("...", "abc"); got != 0 {
		t.Errorf("punctuation-only A should return 0, got %v", got)
	}
	if got := Jaccard("abc", "..."); got != 0 {
		t.Errorf("punctuation-only B should return 0, got %v", got)
	}
}

// TestJaccard_EmptyStringVsNonEmpty covers the brief's explicit
// Jaccard("","x")==0 case.
func TestJaccard_EmptyStringVsNonEmpty(t *testing.T) {
	if got := Jaccard("", "x"); got != 0 {
		t.Errorf("expected 0 for empty input, got %v", got)
	}
}

// TestJaccard_PunctuationCollapse covers the brief's explicit
// punctuation-collapse case: "memory." and "memory" tokenise to
// the same word, so similarity is 1.0.
func TestJaccard_PunctuationCollapse(t *testing.T) {
	if got := Jaccard("memory.", "memory"); got != 1.0 {
		t.Errorf("expected 1.0 for punctuation-only difference, got %v", got)
	}
}

// TestTokenSet_StripsPunctuation ports
// dispatcher's TestTokenSet_StripsPunctuation.
func TestTokenSet_StripsPunctuation(t *testing.T) {
	got := TokenSet("hello, world! (test)")
	want := map[string]bool{"hello": true, "world": true, "test": true}
	if len(got) != len(want) {
		t.Errorf("expected %d tokens, got %d: %+v", len(want), len(got), got)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing token %q in %+v", k, got)
		}
	}
}

// TestTokenSet_EmptyTokensSkipped ports
// dispatcher's TestTokenSet_EmptyTokensSkipped: all-punctuation
// tokens strip to empty and must not enter the set.
func TestTokenSet_EmptyTokensSkipped(t *testing.T) {
	got := TokenSet("... !!! ((( )))")
	if len(got) != 0 {
		t.Errorf("expected empty set, got %+v", got)
	}
}

// TestJaccardSets_OneEmptyMapIsZero covers the brief's explicit
// JaccardSets-with-one-empty-map case.
func TestJaccardSets_OneEmptyMapIsZero(t *testing.T) {
	nonEmpty := map[string]struct{}{"a": {}, "b": {}}
	empty := map[string]struct{}{}
	if got := JaccardSets(empty, nonEmpty); got != 0 {
		t.Errorf("expected 0 when left set is empty, got %v", got)
	}
	if got := JaccardSets(nonEmpty, empty); got != 0 {
		t.Errorf("expected 0 when right set is empty, got %v", got)
	}
	if got := JaccardSets(empty, empty); got != 0 {
		t.Errorf("expected 0 when both sets are empty, got %v", got)
	}
}

// TestJaccardSets_Basic exercises the intersection/union math
// directly against pre-tokenised sets, independent of TokenSet.
func TestJaccardSets_Basic(t *testing.T) {
	a := map[string]struct{}{"a": {}, "b": {}, "c": {}}
	b := map[string]struct{}{"b": {}, "c": {}, "d": {}}
	// intersection {b,c} = 2, union {a,b,c,d} = 4 -> 0.5
	if got := JaccardSets(a, b); got != 0.5 {
		t.Errorf("expected 0.5, got %v", got)
	}
}
