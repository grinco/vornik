// Tests for the 2026.7.0 F8 LLM-free consolidation pass. The
// tokenize + frequency-map + top-terms primitives are pure
// functions — tested directly. The Repo-dependent
// ConsolidateProject path uses the sqlmock harness shared with
// the other repository tests.

package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTokenize_BasicNormalisation pins the tokeniser's contract:
// lowercase, punctuation → space, drop stopwords, drop tokens
// shorter than minLength. The contract is load-bearing for
// consumer ranking — a regression here changes every gist.
func TestTokenize_BasicNormalisation(t *testing.T) {
	got := Tokenize("The NVDA price moved 5% today; SPY didn't.", 3)
	// "the" is a stopword and must be dropped.
	for _, want := range []string{"nvda", "price", "moved", "today", "spy", "didn"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("token %q not found in %v", want, got)
		}
	}
	for _, forbidden := range []string{"the", "5"} {
		for _, g := range got {
			if g == forbidden {
				t.Errorf("token %q must have been dropped (stopword or too short)", forbidden)
			}
		}
	}
}

// TestTokenize_EmptyStringReturnsNil — callers should not need
// a nil-check on the result; an empty input returns nil, not a
// zero-length empty slice that confuses range loops.
func TestTokenize_EmptyStringReturnsNil(t *testing.T) {
	if got := Tokenize("   ", 3); got != nil {
		t.Errorf("empty input must return nil, got %v", got)
	}
}

// TestTokenize_MinLengthBoundary — tokens shorter than minLength
// are dropped. Pins the boundary at exact-equal so a future
// off-by-one regression surfaces.
func TestTokenize_MinLengthBoundary(t *testing.T) {
	got := Tokenize("ab abc abcd", 3)
	// "ab" drops; "abc" and "abcd" survive.
	if strings.Join(got, ",") != "abc,abcd" {
		t.Errorf("min-length boundary wrong: got %v, want [abc abcd]", got)
	}
}

// TestFrequencyMap_AggregatesAcrossChunks — the chunk-spanning
// behaviour is the whole point: "NVDA" mentioned across 3 chunks
// surfaces with frequency 3, not 1.
func TestFrequencyMap_AggregatesAcrossChunks(t *testing.T) {
	freq := FrequencyMap([]string{
		"NVDA earnings beat",
		"NVDA up 5%",
		"NVDA closed at record high",
		"SPY flat",
	}, 3)
	if freq["nvda"] != 3 {
		t.Errorf("freq[nvda] = %d, want 3", freq["nvda"])
	}
	if freq["spy"] != 1 {
		t.Errorf("freq[spy] = %d, want 1", freq["spy"])
	}
}

// TestTopTerms_DeterministicOrderingByCountThenAlpha — same
// frequency → alphabetical tie-break. Locks deterministic
// output so the operator's "what's been mentioned" tile doesn't
// shuffle on every refresh.
func TestTopTerms_DeterministicOrderingByCountThenAlpha(t *testing.T) {
	freq := map[string]int{
		"banana":  3,
		"apple":   3, // same count as banana → alpha tiebreak puts apple first
		"cherry":  1,
		"avocado": 5,
	}
	got := TopTerms(freq, 3)
	want := []TermFrequency{
		{Term: "avocado", Count: 5},
		{Term: "apple", Count: 3},
		{Term: "banana", Count: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %+v, want %+v", i, got[i], w)
		}
	}
}

// TestTopTerms_NDefaultsAndClamping — zero/negative n falls
// back to 25; len(freq) < n returns the full slice without
// padding.
func TestTopTerms_NDefaultsAndClamping(t *testing.T) {
	freq := map[string]int{"a": 1, "b": 2}
	if got := TopTerms(freq, 0); len(got) != 2 {
		t.Errorf("n=0 fallback must NOT truncate when freq has fewer than 25 entries; got %d", len(got))
	}
	if got := TopTerms(freq, -7); len(got) != 2 {
		t.Errorf("negative n must use default-25, not truncate to 0; got %d", len(got))
	}
}

// TestNewConsolidator_Defaults — anchors the public constructor's
// default values so a refactor doesn't silently change the
// MinTokenLength / TopN contract.
func TestNewConsolidator_Defaults(t *testing.T) {
	c := NewConsolidator(nil)
	if c.MinTokenLength != 3 {
		t.Errorf("MinTokenLength = %d, want 3", c.MinTokenLength)
	}
	if c.TopN != 25 {
		t.Errorf("TopN = %d, want 25", c.TopN)
	}
}

// TestConsolidateProject_NilRepoReturnsEmptyGist — consumers
// rendering "no data" depend on a non-nil zero-valued gist
// rather than a nil pointer + error, so they don't have to
// nil-guard on every render path.
func TestConsolidateProject_NilRepoReturnsEmptyGist(t *testing.T) {
	var c *Consolidator
	got, err := c.ConsolidateProject(context.Background(), "p", 100)
	if err != nil {
		t.Fatalf("nil consolidator: got err %v", err)
	}
	if got == nil {
		t.Fatal("nil consolidator: got nil gist (want zero-value gist)")
	}
}

// TestConsolidateProject_HappyPath — wire the consolidator to a
// sqlmock repo, return three NVDA chunks, assert the gist
// surfaces "nvda" as the top term with count 3.
func TestConsolidateProject_HappyPath(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	c := NewConsolidator(r)

	rows := mock.NewRows([]string{"content"}).
		AddRow("NVDA earnings beat consensus").
		AddRow("NVDA up 5% today").
		AddRow("NVDA closed at record high")
	mock.ExpectQuery("SELECT content").
		WithArgs("proj", 100).
		WillReturnRows(rows)

	got, err := c.ConsolidateProject(context.Background(), "proj", 100)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ChunksScanned != 3 {
		t.Errorf("ChunksScanned = %d, want 3", got.ChunksScanned)
	}
	if len(got.Terms) == 0 || got.Terms[0].Term != "nvda" {
		t.Errorf("top term = %+v, want first entry = nvda", got.Terms)
	}
	if got.Terms[0].Count != 3 {
		t.Errorf("nvda count = %d, want 3", got.Terms[0].Count)
	}
}

// TestConsolidateProject_RepoErrorBubbles — the ConsolidateProject
// caller (operator-triggered "give me the gist") must see DB
// problems, not a silent empty result; otherwise a broken DB
// looks like an empty project.
func TestConsolidateProject_RepoErrorBubbles(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	c := NewConsolidator(r)
	mock.ExpectQuery("SELECT content").
		WillReturnError(errors.New("connection dropped"))
	_, err := c.ConsolidateProject(context.Background(), "proj", 100)
	if err == nil {
		t.Fatal("expected error to bubble")
	}
}

// TestListChunkContents_DefaultLimit — a zero limit falls back
// to 1000 so callers passing an unset value can't accidentally
// dump the whole project's memory.
func TestListChunkContents_DefaultLimit(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT content").
		WithArgs("p", 1000).
		WillReturnRows(mock.NewRows([]string{"content"}))
	if _, err := r.ListChunkContents(context.Background(), "p", 0); err != nil {
		t.Fatalf("err: %v", err)
	}
}
