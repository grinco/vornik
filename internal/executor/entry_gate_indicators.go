package executor

import (
	"context"
	"sync"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/verifier"
)

// entryGateIndicatorsTimeout caps the post-step SMA(50) fetch. This
// runs after the strategist step completes (verifier path), off the
// trading hot path, and touches at most a couple of symbols (the
// long-open proposals), so a tight cap is fine — on timeout the map is
// simply incomplete and entry_gate_consistent abstains for the missing
// symbols rather than blocking.
const entryGateIndicatorsTimeout = 15 * time.Second

// entryGateIndicators computes a deterministic daily SMA(50) for each
// LONG-OPEN symbol the strategist proposed this step, for the
// entry_gate_consistent verifier. It re-fetches daily bars (reusing the
// same broker MCP path + Wilder/SMA math as the WATCHLIST_INDICATORS
// pre-warm) rather than threading the pre-warm's rendered table through
// the executor — SMA(50) is a daily figure, stable across the few
// minutes between the strategist's start and the verifier run, and
// re-deriving it keeps the verifier input self-contained.
//
// Scope discipline:
//   - Only long opens are fetched (closes/exits are exempt from the
//     entry floor, so they need no indicator).
//   - Only watchlist symbols are fetched (an off-watchlist ticker is
//     already rejected by proposals_match_watchlist; no point spending
//     a broker round-trip on it here).
//   - A per-symbol fetch failure drops that symbol silently; the
//     verifier abstains for symbols it has no SMA(50) for.
//
// Returns nil (no allocation, no broker calls) when there are no
// long-open proposals or the project has no watchlist — the common
// case on most ticks.
func (e *Executor) entryGateIndicators(ctx context.Context, project *registry.Project, resultBytes []byte) map[string]verifier.EntryGateIndicator {
	if project == nil || len(project.Trading.Watchlist) == 0 || len(resultBytes) == 0 {
		return nil
	}
	symbols := verifier.ProposedLongOpenSymbols(resultBytes)
	if len(symbols) == 0 {
		return nil
	}
	watchlist := make(map[string]struct{}, len(project.Trading.Watchlist))
	for _, s := range project.Trading.Watchlist {
		watchlist[s] = struct{}{}
	}

	brokerURL := brokerBaseURL()
	fetchCtx, cancel := context.WithTimeout(ctx, entryGateIndicatorsTimeout)
	defer cancel()

	var (
		mu  sync.Mutex
		out = make(map[string]verifier.EntryGateIndicator)
		wg  sync.WaitGroup
	)
	for _, sym := range symbols {
		if _, ok := watchlist[sym]; !ok {
			continue
		}
		wg.Add(1)
		go func(symbol string) {
			defer wg.Done()
			bars, err := fetchDailyBars(fetchCtx, brokerURL, project.ID, symbol)
			if err != nil {
				e.logger.Warn().Err(err).Str("project", project.ID).Str("symbol", symbol).
					Msg("entry-gate indicators: SMA(50) fetch failed; verifier will abstain for this symbol")
				return
			}
			closes := make([]float64, 0, len(bars))
			for _, b := range bars {
				closes = append(closes, b.Close)
			}
			sma50 := computeSMA(closes, 50)
			if sma50 <= 0 {
				// Too few bars for a meaningful SMA(50); leave the
				// symbol out so the verifier abstains rather than
				// comparing against a degenerate 0.
				return
			}
			mu.Lock()
			out[symbol] = verifier.EntryGateIndicator{SMA50: sma50}
			mu.Unlock()
		}(sym)
	}
	wg.Wait()

	if len(out) == 0 {
		return nil
	}
	return out
}
