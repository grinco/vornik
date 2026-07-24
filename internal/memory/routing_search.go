package memory

import (
	"context"
	"encoding/json"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// widenRoutingMaxLimit caps the candidate pool a widen round may pull,
// bounding cost regardless of MaxRounds (mirrors sufficiencyMaxRoundLimit).
const widenRoutingMaxLimit = 100

// routingConfig returns the effective (defaulted) routing config.
func (s *Searcher) routingConfig() RetrievalRoutingConfig {
	return s.cfg.RetrievalRouting.applyDefaults()
}

// RecallWithRouting is the confidence-based retrieval routing entry point
// (P3). When opts.Routing is set it runs the verdict-predicated widen loop,
// computes the retrieval_trust_verdict on the selected set, applies the
// firewall, writes the trust_verdict trace row, and returns the verdict
// alongside the results. When opts.Routing is NOT set it is exactly
// RecallWithContext with a nil verdict — no widen, no trace, byte-identical.
//
// The daemon NEVER web-fetches or makes any external call here; the only
// automatic action is the bounded, idempotent, DB-only widen.
func (s *Searcher) RecallWithRouting(
	ctx context.Context,
	projectID, query string,
	opts SearchOptions,
	reqCtx memoryfirewall.RequestContext,
) ([]SearchResult, *RoutingVerdict, error) {
	cfg := s.routingConfig()
	// Master kill-switch (review M-2 / §6 reversibility): when routing is off
	// at the call site OR disabled in config, behave exactly as routing-off —
	// no verdict, no widen, byte-identical results.
	if !opts.Routing || !cfg.Enabled {
		results, err := s.RecallWithContext(ctx, projectID, query, opts, reqCtx)
		return results, nil, err
	}

	best, verdict, err := s.widenLoop(ctx, projectID, query, opts, cfg)
	if err != nil {
		return nil, nil, err
	}
	// Firewall pass over the SELECTED set (policy is orthogonal to trust; the
	// verdict is computed on the DB-recall set, matching §3.3's pseudocode).
	// NOTE (review M-1): in firewall ENFORCE mode the pass can drop chunks, so
	// verdict.Basis (ResultCount/TrustMean) describes the pre-firewall recall
	// set and may over-count what the caller ultimately receives. This is
	// deliberate per §3.3; the verdict rates retrieval trust, not the
	// post-policy visible slice.
	filtered := s.applyFirewall(ctx, projectID, best, reqCtx)
	s.writeTrustVerdictTrace(ctx, projectID, verdict, cfg)
	return filtered, verdict, nil
}

// widenLoop wires searchInternal into the pure widenLoopCore.
func (s *Searcher) widenLoop(
	ctx context.Context,
	projectID, query string,
	opts SearchOptions,
	cfg RetrievalRoutingConfig,
) ([]SearchResult, *RoutingVerdict, error) {
	run := func(o SearchOptions) ([]SearchResult, error) {
		return s.searchInternal(ctx, projectID, query, o)
	}
	return widenLoopCore(opts, cfg, time.Now(), run)
}

// widenLoopCore is the pure, testable heart of the verdict-predicated widen.
// It borrows sufficiencyLoop's SAFETY properties only — bounded rounds
// (MaxRounds), best-so-far retention, round-error → return prior best — but
// NOT its reranker-gated predicate: it re-evaluates the VERDICT each round
// and widens only while verdict==low (§3.3), independent of the reranker.
// `run` executes one DB-only search round; it is the ONLY action taken —
// there is never an external / web call under any verdict.
func widenLoopCore(
	base SearchOptions,
	cfg RetrievalRoutingConfig,
	now time.Time,
	run func(SearchOptions) ([]SearchResult, error),
) ([]SearchResult, *RoutingVerdict, error) {
	bestSet, err := run(base)
	if err != nil {
		return nil, nil, err // round-1 error == the single-shot error
	}
	bestV := cfg.EvaluateVerdict(bestSet, now)

	rounds := 0
	for bestV.Verdict == VerdictLow && rounds < cfg.MaxRounds && cfg.WidenEnabled {
		rounds++
		cand, cerr := run(widenRoutingOptions(base, rounds))
		if cerr != nil {
			break // return best so far — never errors the call (§3.3)
		}
		candV := cfg.EvaluateVerdict(cand, now)
		if betterRound(candV, bestV) {
			bestSet, bestV = cand, candV
		}
	}
	bestV.WidenRounds = rounds
	return bestSet, &bestV, nil
}

// betterRound implements the best-so-far selection predicate (§3.3 F3): a
// candidate wins iff it has a strictly higher verdict rank, or the same rank
// with a strictly higher trust_mean. A later WORSE round never replaces a
// better earlier one, and ties keep the earlier round.
func betterRound(cand, best RoutingVerdict) bool {
	cr, br := verdictRank(cand.Verdict), verdictRank(best.Verdict)
	if cr != br {
		return cr > br
	}
	return cand.Basis.TrustMean > best.Basis.TrustMean
}

// widenRoutingOptions grows the candidate pool for widen round N without
// touching the query string (so the cached query embedding is reused) and
// relaxes scope. Routing stays set so the loop keeps its semantics, but the
// widen calls searchInternal directly (no recursion).
func widenRoutingOptions(base SearchOptions, round int) SearchOptions {
	o := base
	if o.Limit <= 0 {
		o.Limit = 10
	}
	o.Limit *= (round + 1)
	if o.Limit > widenRoutingMaxLimit {
		o.Limit = widenRoutingMaxLimit
	}
	o.StrictScope = false
	return o
}

// writeTrustVerdictTrace persists the trust_verdict stage row (§3.6). Best-
// effort: a nil sink or a write error is logged (at warn) and never bubbles —
// the search already succeeded and the trace is an analytics channel.
func (s *Searcher) writeTrustVerdictTrace(ctx context.Context, projectID string, v *RoutingVerdict, cfg RetrievalRoutingConfig) {
	if s == nil || s.traceSink == nil || v == nil {
		return
	}
	params := trustVerdictParams(v, cfg)
	blob, merr := json.Marshal(params)
	if merr != nil {
		s.logger.Warn().Err(merr).Str("project_id", projectID).
			Msg("memory: trust_verdict trace marshal failed")
		return
	}
	row := &persistence.MemorySearchStage{
		ProjectID:  projectID,
		Stage:      "trust_verdict",
		Parameters: blob,
	}
	if werr := s.traceSink.RecordStage(ctx, row); werr != nil {
		s.logger.Warn().Err(werr).Str("project_id", projectID).
			Msg("memory: trust_verdict trace write failed (search itself succeeded)")
	}
}

// trustVerdictParams builds the §3.6 parameters map: the verdict + basis and
// the exact tuning params used, so the tuning loop has a replayable record.
func trustVerdictParams(v *RoutingVerdict, cfg RetrievalRoutingConfig) map[string]any {
	return map[string]any{
		"verdict":      v.Verdict,
		"trust_mean":   v.Basis.TrustMean,
		"result_count": v.Basis.ResultCount,
		"age_capped":   v.Basis.AgeCapped,
		"weakest_dim":  v.Basis.WeakestDim,
		"widen_rounds": v.WidenRounds,
		"weights": map[string]float64{
			"w_status": cfg.WStatus,
			"w_conf":   cfg.WConf,
			"w_fresh":  cfg.WFresh,
		},
		"thresholds": map[string]float64{
			"high": cfg.HighThreshold,
			"low":  cfg.LowThreshold,
		},
		"K":          cfg.K,
		"minResults": cfg.MinResults,
	}
}
