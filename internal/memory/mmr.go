package memory

import (
	"strings"
	"unicode"
)

// applyMMR re-orders `results` using Maximal Marginal Relevance so the
// returned slice trades pure relevance against diversity. After the
// reranker has sorted by relevance, near-duplicates often cluster at
// the top — same chunk written by two artifacts, or supersession that
// missed because the source/task didn't match. MMR sweeps these into
// the tail and lifts diverse-but-still-relevant candidates up.
//
// Formula: argmax over remaining candidates of
//
//	λ·rel(d) − (1−λ)·max_{s∈selected} sim(d, s)
//
// where rel(d) is the candidate's relevance position (we use its
// already-sorted index as a proxy: rank 0 = rel 1.0, last rank = 0.0)
// and sim(d, s) is token-set Jaccard between contents.
//
// lambda in [0, 1]:
//   - 1.0 → pure relevance (input unchanged)
//   - 0.0 → pure diversity (no relevance signal)
//   - 0.6–0.8 is the sweet spot for prose-heavy corpora.
//
// Pure function: no DB, no embeddings, no extra LLM call. The token
// Jaccard surrogate is conservative — it overstates similarity for
// chunks that share boilerplate (license headers, navigation) but the
// dedup gate already filters exact duplicates, so the residual noise
// is a fair price for a $0 reordering pass.
func applyMMR(results []SearchResult, lambda float64) []SearchResult {
	if len(results) < 3 {
		return results
	}
	if lambda <= 0 {
		lambda = 0.7
	}
	if lambda > 1 {
		lambda = 1
	}

	n := len(results)
	tokens := make([]map[string]struct{}, n)
	for i, r := range results {
		tokens[i] = tokenSet(r.Content)
	}
	rel := make([]float64, n)
	for i := range results {
		rel[i] = 1.0 - float64(i)/float64(n-1) // pos 0 → 1, pos N-1 → 0
	}

	// MMR loop: pick the first (always the top-relevance row), then
	// iteratively pick the next-best by MMR score.
	selected := make([]int, 0, n)
	picked := make([]bool, n)
	selected = append(selected, 0)
	picked[0] = true

	for len(selected) < n {
		bestIdx := -1
		bestScore := -1e9
		for i := 0; i < n; i++ {
			if picked[i] {
				continue
			}
			// Max similarity to anything already picked.
			maxSim := 0.0
			for _, s := range selected {
				sim := jaccard(tokens[i], tokens[s])
				if sim > maxSim {
					maxSim = sim
				}
			}
			score := lambda*rel[i] - (1-lambda)*maxSim
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		selected = append(selected, bestIdx)
		picked[bestIdx] = true
	}

	out := make([]SearchResult, n)
	for i, idx := range selected {
		out[i] = results[idx]
	}
	return out
}

// tokenSet returns the set of lower-cased alphanumeric tokens of length
// ≥3 from s. Length-3 floor strips most stopwords and punctuation noise
// without an explicit stopword list; the Jaccard ratio is stable enough
// at that resolution for the diversification decision.
func tokenSet(s string) map[string]struct{} {
	out := make(map[string]struct{})
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			out[b.String()] = struct{}{}
		}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return out
}

// jaccard returns |A ∩ B| / |A ∪ B|. Empty inputs return 0 — interpret
// as "no overlap" rather than "perfectly similar".
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	// Iterate the smaller map for the intersection.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for k := range small {
		if _, ok := large[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
