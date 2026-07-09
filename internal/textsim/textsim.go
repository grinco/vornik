// Package textsim provides shared token-set Jaccard similarity
// primitives used to detect near-duplicate free-text prompts
// across the codebase (dispatcher task dedup, autonomy failure
// cooldown, and — incoming — backlog-deposit dedup). Consumers
// keep their own tokenizers where behavior differs (e.g. autonomy's
// stop-word filtering); this package owns only the shared math.
package textsim

import "strings"

// TokenSet splits s on whitespace, strips leading + trailing ASCII
// punctuation from each token, and returns each distinct non-empty
// result. Punctuation stripping is what makes "memory" and
// "memory." (or "(if" and "if") collapse to the same token, so the
// Jaccard metric isn't fooled by trivial punctuation differences in
// two paraphrased prompts. Caller is responsible for casing +
// whitespace normalisation.
func TokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range strings.Fields(s) {
		t = strings.Trim(t, ".,;:!?()[]{}\"'`")
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

// JaccardSets returns the size of the intersection over the size of
// the union of two pre-tokenised sets — |A∩B| / |A∪B|. Returns 0
// when either set is empty so callers don't trip on a "100% match"
// against the empty set.
func JaccardSets(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var intersect int
	for tok := range a {
		if _, ok := b[tok]; ok {
			intersect++
		}
	}
	union := len(a) + len(b) - intersect
	return float64(intersect) / float64(union)
}

// Jaccard returns the size of the intersection over the size of the
// union of word sets in a and b. Operates on lower-case
// whitespace-tokenised strings; punctuation is stripped per-token
// via TokenSet. Returns 0 when either input is empty (or tokenises
// to empty) so callers don't trip on a "100% match" against the
// empty token set.
func Jaccard(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	return JaccardSets(TokenSet(a), TokenSet(b))
}
