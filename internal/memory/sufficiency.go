package memory

import (
	"context"

	"vornik.io/vornik/internal/memoryfirewall"
)

// SufficiencyConfig governs scored-sufficiency iterative retrieval. The
// feature is doubly inert by default: Enabled defaults false, AND it only
// activates when the LLM reranker is wired (so the ScoreFloor is an absolute,
// calibrated [0,1] threshold rather than an un-normalized RRF value — see
// review-20260623-bd08 finding #1). When inactive, RecallSufficient performs
// exactly one search, identical to the single-shot path.
type SufficiencyConfig struct {
	Enabled    bool
	MinHighRel int     // "enough" = at least this many hits with Score >= ScoreFloor
	ScoreFloor float64 // absolute reranker relevance floor [0,1]
	MaxRounds  int     // hard cap; <=1 collapses to a single shot
}

// sufficiencyMaxRoundLimit caps the candidate pool a widening round may pull,
// bounding cost regardless of MaxRounds.
const sufficiencyMaxRoundLimit = 100

// SetSufficiency wires the scored-sufficiency config. Nil-safe.
func (s *Searcher) SetSufficiency(cfg SufficiencyConfig) {
	if s != nil {
		s.sufficiency = cfg
	}
}

// rerankerActive reports whether a real (non-Noop) reranker is wired. Only
// then are SearchResult.Score values calibrated [0,1] relevance scores, which
// the sufficiency predicate requires.
func (s *Searcher) rerankerActive() bool {
	if s == nil || s.reranker == nil {
		return false
	}
	if _, isNoop := s.reranker.(NoopReranker); isNoop {
		return false
	}
	return true
}

// RecallSufficient runs reranker-gated, round-isolated, bounded scored
// retrieval. The query string is identical across rounds (only the candidate
// pool widens), so the cached query embedding is reused — widening rounds add
// no embedding cost. When the feature is disabled or the reranker is inactive,
// it performs a single RecallWithContext call.
func (s *Searcher) RecallSufficient(
	ctx context.Context,
	projectID, query string,
	opts SearchOptions,
	reqCtx memoryfirewall.RequestContext,
) ([]SearchResult, error) {
	// Scored-sufficiency is the one path that opts into reranking — its
	// absolute score floor is only meaningful against calibrated reranker
	// scores. Setting it here (not on the interactive callers) is what scopes
	// the rerank latency to context assembly.
	opts.Rerank = true
	run := func(o SearchOptions) ([]SearchResult, error) {
		// Same query string every round → embedding cache hit.
		return s.RecallWithContext(ctx, projectID, query, o, reqCtx)
	}
	return sufficiencyLoop(opts, s.sufficiency, s.rerankerActive(), run)
}

// sufficiencyLoop is the pure, testable core. `run` executes one firewall-
// applied search round. It returns a SINGLE round's results (never a merge of
// rounds — RRF/rerank scores from different candidate pools are not comparable,
// review-20260623-bd08 finding #3): the first round that meets the predicate,
// else the round with the most high-relevance hits (round 1 wins ties). On a
// non-first-round error it returns the best round so far (finding #6).
func sufficiencyLoop(
	base SearchOptions,
	cfg SufficiencyConfig,
	rerankerActive bool,
	run func(SearchOptions) ([]SearchResult, error),
) ([]SearchResult, error) {
	first, err := run(base)
	if err != nil {
		return nil, err // round-1 error == the single-shot error
	}
	if !cfg.Enabled || !rerankerActive || cfg.MaxRounds <= 1 {
		return truncateResults(first, base.Limit), nil
	}

	best := first
	bestCount := highRelCount(first, cfg.ScoreFloor)
	if bestCount >= cfg.MinHighRel {
		return truncateResults(first, base.Limit), nil
	}

	for round := 2; round <= cfg.MaxRounds; round++ {
		r, rerr := run(widenOptions(base, round))
		if rerr != nil {
			break // return best so far — never a partial/merged set
		}
		c := highRelCount(r, cfg.ScoreFloor)
		if c >= cfg.MinHighRel {
			return truncateResults(r, base.Limit), nil // first sufficient round
		}
		if c > bestCount { // strictly greater only → round 1 wins ties
			best, bestCount = r, c
		}
	}
	return truncateResults(best, base.Limit), nil
}

// widenOptions grows the candidate pool for round N (N>=2) without touching
// the query string: larger Limit (capped) and the lenient scope fallthrough.
func widenOptions(base SearchOptions, round int) SearchOptions {
	o := base
	if o.Limit <= 0 {
		o.Limit = 10
	}
	o.Limit *= round
	if o.Limit > sufficiencyMaxRoundLimit {
		o.Limit = sufficiencyMaxRoundLimit
	}
	o.StrictScope = false
	return o
}

// highRelCount counts hits at/above the absolute relevance floor.
func highRelCount(results []SearchResult, floor float64) int {
	n := 0
	for _, r := range results {
		if r.Score >= floor {
			n++
		}
	}
	return n
}

// truncateResults clips to the caller's requested limit (results are already
// score-ordered by the reranker, so the top-N retains the high-relevance hits).
func truncateResults(results []SearchResult, limit int) []SearchResult {
	if limit > 0 && len(results) > limit {
		return results[:limit]
	}
	return results
}
