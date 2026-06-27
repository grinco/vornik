// 2026.7.0 F8 — LLM-free consolidation pass.
//
// The LLM classifier handles fresh chunks one at a time and pays
// per-call cost. The dual side — "give me the gist of project X
// in one screen" — doesn't need an LLM at all; classic term-
// frequency extraction works well enough at human scale and is
// effectively free to run.
//
// Design:
//
//  1. Tokenize chunk content (lowercase, strip punctuation, drop
//     stopwords, drop tokens shorter than the minTokenLength).
//  2. Sum frequencies across all chunks in the project (or a
//     scoped subset — e.g. one content_class).
//  3. Rank terms by raw frequency; return the top-N as a
//     ProjectGist.
//
// No LLM call; no embedding lookup; no schema write. The struct
// is pure-Go so consumers (CLI, web UI, MCP) can render however
// they like.
//
// Cost analysis (informal): ~1 ms per 100 chunks on a single
// goroutine. A 10k-chunk project consolidates in ~100 ms. Cheap
// enough that the periodic loop can fire every minute without
// dragging the daemon.

package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ProjectGist is the per-project summary the consolidator
// produces. Each Term row pairs the term itself with its raw
// frequency across the corpus snapshot.
type ProjectGist struct {
	ProjectID string
	// ChunksScanned is how many chunks contributed to this gist.
	// Powers the operator-facing "based on N chunks" footer.
	ChunksScanned int
	// Terms is the top-N (ranked by frequency, tiebreaker alpha).
	Terms []TermFrequency
}

// TermFrequency is one entry in the gist's ranked list.
type TermFrequency struct {
	Term  string
	Count int
}

// Consolidator drives the LLM-free per-project gist pass.
// Holds a Repository reference for chunk fetching; everything
// else is pure-Go so the type is trivially testable.
type Consolidator struct {
	Repo *Repository
	// MinTokenLength filters out single-character noise. 3 is
	// the rule-of-thumb default — keeps "NVDA" / "SPY" but drops
	// "a", "is", "of". Zero falls back to 3.
	MinTokenLength int
	// TopN bounds the ranked list returned in ProjectGist. Zero
	// falls back to 25.
	TopN int
}

// NewConsolidator returns a Consolidator with sensible defaults.
func NewConsolidator(repo *Repository) *Consolidator {
	return &Consolidator{Repo: repo, MinTokenLength: 3, TopN: 25}
}

// stopwords is the small English stopword set the tokenizer
// applies. Intentionally short — the goal is to drop the most
// common conversational filler, not to compete with a full NLP
// stopword list. Extend as production output shows clear
// "this term is noise" patterns.
var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "but": {}, "not": {},
	"you": {}, "all": {}, "can": {}, "her": {}, "was": {}, "one": {},
	"our": {}, "out": {}, "his": {}, "has": {}, "had": {}, "use": {},
	"any": {}, "may": {}, "say": {}, "she": {}, "way": {}, "who": {},
	"this": {}, "that": {}, "with": {}, "from": {}, "have": {}, "they": {},
	"will": {}, "your": {}, "what": {}, "when": {}, "make": {}, "like": {},
	"time": {}, "just": {}, "him": {}, "know": {}, "take": {}, "into": {},
	"some": {}, "could": {}, "them": {}, "than": {}, "then": {}, "look": {},
	"only": {}, "come": {}, "its": {}, "over": {}, "think": {}, "also": {},
	"back": {}, "after": {}, "two": {}, "how": {}, "work": {},
	"first": {}, "well": {}, "even": {}, "want": {}, "because": {},
	"these": {}, "give": {}, "most": {},
}

// Tokenize splits text into normalized tokens for frequency
// counting. Pure function — exported for unit tests and reuse
// by other ranking surfaces. Empty input returns nil so callers
// don't have to nil-check before iterating.
func Tokenize(text string, minLength int) []string {
	if minLength <= 0 {
		minLength = 3
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	// Replace any non-letter, non-digit character with a space
	// in one pass so strings.Fields catches the resulting words.
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < minLength {
			continue
		}
		if _, isStop := stopwords[w]; isStop {
			continue
		}
		out = append(out, w)
	}
	return out
}

// FrequencyMap counts occurrences of each token across the given
// chunk-content strings. Pure helper — extracted so the Repo-
// dependent ConsolidateProject path stays a thin wrapper.
func FrequencyMap(contents []string, minTokenLength int) map[string]int {
	freq := make(map[string]int, 128)
	for _, c := range contents {
		for _, tok := range Tokenize(c, minTokenLength) {
			freq[tok]++
		}
	}
	return freq
}

// TopTerms ranks a frequency map by count (desc), tiebreaking
// alphabetically so the output is deterministic. Caps at n; a
// zero or negative n falls back to 25.
func TopTerms(freq map[string]int, n int) []TermFrequency {
	if n <= 0 {
		n = 25
	}
	out := make([]TermFrequency, 0, len(freq))
	for term, count := range freq {
		out = append(out, TermFrequency{Term: term, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Term < out[j].Term
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// ConsolidateProject is the operator-facing entry point: list
// chunks for the project, tokenise, rank, return a gist. Best-
// effort — when the repo is nil or the project has no chunks,
// returns an empty ProjectGist rather than an error so consumers
// can render "no data" cleanly.
func (c *Consolidator) ConsolidateProject(ctx context.Context, projectID string, scanLimit int) (*ProjectGist, error) {
	if c == nil || c.Repo == nil {
		return &ProjectGist{ProjectID: projectID}, nil
	}
	minLen := c.MinTokenLength
	if minLen <= 0 {
		minLen = 3
	}
	topN := c.TopN
	if topN <= 0 {
		topN = 25
	}
	if scanLimit <= 0 {
		scanLimit = 1000
	}
	contents, err := c.Repo.ListChunkContents(ctx, projectID, scanLimit)
	if err != nil {
		return nil, fmt.Errorf("consolidator: list chunks: %w", err)
	}
	gist := &ProjectGist{
		ProjectID:     projectID,
		ChunksScanned: len(contents),
		Terms:         TopTerms(FrequencyMap(contents, minLen), topN),
	}
	return gist, nil
}
