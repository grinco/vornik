package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/registry"
)

// barsPrewarmTimeout caps the parallel daily-bars fetch. Wider than
// the quote pre-warm because get_historical_bars is heavier on the
// IBKR side (one round-trip per symbol for 120 daily bars) and
// IBKR's delayed-feed snapshot can take 8-12s on paper accounts
// before the historical request is honoured. The strategist's
// fallback path (per-symbol get_historical_bars + TA tools) still
// works if this times out, so the cap is conservative rather than
// strict — it's better to wait an extra few seconds than to push
// the model into an 80-call sequential drill-down.
const barsPrewarmTimeout = 30 * time.Second

// buildWatchlistIndicatorsBlock fetches each watchlist symbol's daily
// bars in parallel and renders a per-symbol indicator table the
// strategist scans before any per-symbol drill-down. Saves the ~16
// sequential get_historical_bars + 64 sequential TA tool calls (~8
// seconds wall-clock per tick on a 16-symbol watchlist) AND moves the
// indicator math out of the LLM's tool budget so the iteration cap
// is preserved for the genuine reasoning steps (deep-dive enrichment,
// risk reasoning).
//
// Best-effort, mirroring buildWatchlistQuotesBlock: per-symbol failures
// show fetch_failed in the table; if every fetch fails the function
// returns "" and the strategist falls back to the per-symbol path.
//
// Indicators computed in-process (Go arithmetic, deterministic):
//
//   - SMA(20), SMA(50): trend filter
//   - SMA(200): regime filter (last > SMA200 = BULL bias; SMA50 >
//     SMA200 also for momentum-continuation enablement). Added
//     2026-05-15 evening so the strategist's regime check stays in
//     the pre-warmed table instead of falling back to per-symbol
//     bars + manual SMA200 when SPY's quote pre-fetch fails (IBKR
//     market-data hiccup that ballooned the strategist context
//     from 28k to 300k tokens and tripped the step timeout).
//   - RSI(14) with Wilder smoothing: oversold/overbought
//   - MACD(12,26,9): macd line, signal line, histogram (trend
//     confirmation)
//
// The math matches services/broker-ta/src/vornik_broker_ta — same
// Wilder convention for RSI, same SMA-seeded EMA for MACD — so the
// pre-warmed table is a drop-in for what the strategist would have
// computed via tool calls. (Adding a new indicator here = one
// computeXxx helper + one column.)
//
// This helper deliberately avoids the daemon's MCP client machinery
// for the same reason as quote pre-warm: per-project, per-server
// mutex serialises calls. HTTP+JSON-RPC against the broker's
// /message endpoint is far lighter for a 16-symbol fan-out.
func (e *Executor) buildWatchlistIndicatorsBlock(ctx context.Context, project *registry.Project) string {
	if project == nil || len(project.Trading.Watchlist) == 0 {
		return ""
	}
	brokerURL := brokerBaseURL()

	probeCtx, cancel := context.WithTimeout(ctx, barsPrewarmTimeout)
	defer cancel()

	type indicatorRow struct {
		symbol string
		last   float64
		sma20  float64
		sma50  float64
		sma200 float64 // 0 when len(closes) < 200; rendered as "—"
		rsi14  float64
		macd   float64
		sig    float64
		hist   float64
		err    string
	}
	results := make([]indicatorRow, len(project.Trading.Watchlist))
	var wg sync.WaitGroup
	for i, sym := range project.Trading.Watchlist {
		wg.Add(1)
		go func(idx int, symbol string) {
			defer wg.Done()
			bars, err := fetchDailyBars(probeCtx, brokerURL, project.ID, symbol)
			if err != nil {
				results[idx] = indicatorRow{symbol: symbol, err: truncateForPrompt(err.Error(), 60)}
				return
			}
			closes := make([]float64, 0, len(bars))
			for _, b := range bars {
				closes = append(closes, b.Close)
			}
			// Need ≥50 bars for SMA(50) to be meaningful and ≥35
			// (slow+signal = 26+9) for MACD to clear its
			// initialisation. 50 is the binding floor.
			if len(closes) < 50 {
				results[idx] = indicatorRow{
					symbol: symbol,
					err:    fmt.Sprintf("too_few_bars (%d, need >=50 for SMA50)", len(closes)),
				}
				return
			}
			row := indicatorRow{symbol: symbol, last: closes[len(closes)-1]}
			row.sma20 = computeSMA(closes, 20)
			row.sma50 = computeSMA(closes, 50)
			// computeSMA returns 0 when len(closes) < period, which
			// renders as "—" downstream — better than failing the
			// whole row when only the longest window is short.
			row.sma200 = computeSMA(closes, 200)
			row.rsi14 = computeRSI(closes, 14)
			row.macd, row.sig, row.hist = computeMACD(closes, 12, 26, 9)
			results[idx] = row
		}(i, sym)
	}
	wg.Wait()

	// If every fetch failed, drop the block so the strategist falls
	// back cleanly. Same heuristic as buildWatchlistQuotesBlock.
	allFailed := true
	for _, r := range results {
		if r.err == "" {
			allFailed = false
			break
		}
	}
	if allFailed {
		e.logger.Warn().
			Str("project", project.ID).
			Int("symbols", len(project.Trading.Watchlist)).
			Str("broker_url", brokerURL).
			Msg("indicator pre-warm: every symbol failed; strategist will fall back to per-symbol get_historical_bars + TA tools")
		return ""
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].symbol < results[j].symbol
	})

	var sb strings.Builder
	sb.WriteString("Pre-computed daily indicators for the project's watchlist (one parallel batch). Use these in place of per-symbol mcp__broker__get_historical_bars + mcp__ta__sma/rsi/macd calls — values are computed against the most recent ~300 daily bars at the strategist's start (enough history for SMA200). Re-fetch a single symbol via the tools only if its row shows fetch_failed.\n\n")
	sb.WriteString("Regime tip: when SPY's quote pre-fetch fails, the regime check (`last > SMA200` for BULL bias; `SMA50 > SMA200` enables momentum-continuation) can be made from this table directly — no need to re-fetch SPY bars.\n\n")
	sb.WriteString("| symbol | last | SMA20 | SMA50 | SMA200 | RSI14 | MACD | signal | hist | status |\n")
	sb.WriteString("|--------|------|-------|-------|--------|-------|------|--------|------|--------|\n")
	for _, r := range results {
		if r.err != "" {
			fmt.Fprintf(&sb, "| %s | — | — | — | — | — | — | — | — | fetch_failed: %s |\n", r.symbol, r.err)
			continue
		}
		// SMA200 renders "—" when the series was too short (closes <
		// 200). Fresh tickers can still produce a useful row from
		// the shorter-window columns; the regime check is the only
		// thing that loses signal.
		sma200Cell := "—"
		if r.sma200 > 0 {
			sma200Cell = fmt.Sprintf("$%.2f", r.sma200)
		}
		fmt.Fprintf(&sb, "| %s | $%.2f | $%.2f | $%.2f | %s | %.1f | %.3f | %.3f | %.3f | ok |\n",
			r.symbol, r.last, r.sma20, r.sma50, sma200Cell, r.rsi14, r.macd, r.sig, r.hist)
	}
	return sb.String()
}

// barClose is the minimal projection from the broker's bar payload
// that the indicator math needs. Defined locally so the prewarm
// helper doesn't depend on the broker package's full Bar struct
// (avoids dragging the broker module into agent-image builds).
type barClose struct {
	Close float64 `json:"close"`
}

// brokerBaseURL returns the broker MCP base URL from the environment,
// falling back to the loopback default the compose stack publishes.
// Shared by the in-process indicator helpers (quote pre-warm, bars
// pre-warm, entry-gate SMA fetch) so the default lives in one place.
func brokerBaseURL() string {
	if u := os.Getenv("VORNIK_BROKER_BASE_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8788"
}

// fetchDailyBars calls the broker's MCP get_historical_bars tool via
// JSON-RPC for a single symbol with duration=300 D, bar_size=1 day.
// 300 calendar days ≈ 210 trading days, enough headroom for SMA200
// (added 2026-05-15 evening) plus the 50/RSI/MACD windows that
// already worked on 120 days. Cheap + side-effect-free so the
// parallel fan-out is safe.
//
// Project ID propagates as X-Project-ID per the broker's authorization
// contract — historical bar reads observe the header for per-project
// audit consistency.
func fetchDailyBars(ctx context.Context, brokerURL, projectID, symbol string) ([]barClose, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "get_historical_bars",
			"arguments": map[string]any{
				"symbol":   symbol,
				"duration": "300 D",
				"bar_size": "1 day",
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brokerURL+"/message", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set("X-Project-ID", projectID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("broker returned %d", resp.StatusCode)
	}
	// 120 daily bars × ~80 bytes = ~10KB; cap at 4MB to absorb any
	// future fields without an unbounded read.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("envelope parse: %w", err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("broker error: %s", envelope.Error.Message)
	}
	if len(envelope.Result.Content) == 0 {
		return nil, fmt.Errorf("empty broker result")
	}
	var inner struct {
		Bars []barClose `json:"bars"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &inner); err != nil {
		return nil, fmt.Errorf("bars parse: %w", err)
	}
	return inner.Bars, nil
}

// computeSMA returns the simple moving average over the last `period`
// closes. Returns 0 when period <= 0 or insufficient data — callers
// gate on len(closes) before calling, so 0 is only reachable through
// programmer error and is reported as "0.00" in the table (rather
// than crashing the whole batch).
func computeSMA(closes []float64, period int) float64 {
	if period <= 0 || len(closes) < period {
		return 0
	}
	sum := 0.0
	for _, c := range closes[len(closes)-period:] {
		sum += c
	}
	return sum / float64(period)
}

// computeRSI returns the Wilder-smoothed Relative Strength Index for
// the last bar. Matches the broker-ta service's `rsi` tool semantics
// — the strategist would have computed the same number via that
// tool, just with HTTP overhead.
//
// Conventions:
//   - Period <= 0 OR len(closes) <= period → 0 (insufficient data)
//   - All-flat series (no gains, no losses) → 50 (neutral)
//   - All-gain series (no losses) → 100 (max overbought)
//
// Algorithm: simple moving average for the seed gain/loss across the
// first `period` returns, then Wilder smoothing for each subsequent
// return. RSI = 100 - 100/(1+RS) where RS = avgGain/avgLoss.
func computeRSI(closes []float64, period int) float64 {
	if period <= 0 || len(closes) <= period {
		return 0
	}
	var gainSum, lossSum float64
	for i := 1; i <= period; i++ {
		d := closes[i] - closes[i-1]
		switch {
		case d > 0:
			gainSum += d
		case d < 0:
			lossSum -= d
		}
	}
	avgGain := gainSum / float64(period)
	avgLoss := lossSum / float64(period)
	for i := period + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		gain, loss := 0.0, 0.0
		switch {
		case d > 0:
			gain = d
		case d < 0:
			loss = -d
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
	}
	if avgLoss == 0 {
		if avgGain == 0 {
			return 50
		}
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}

// computeMACD returns (macdLine, signalLine, histogram) for the
// MACD(fast,slow,signal) configuration. Caller passes 12,26,9 for the
// retail-charting convention. Returns zeros when the series is too
// short to leave the EMA initialisation window — the table renders
// 0.000 rather than crashing the batch.
func computeMACD(closes []float64, fast, slow, signal int) (float64, float64, float64) {
	if len(closes) < slow+signal {
		return 0, 0, 0
	}
	fastEMA := computeEMASeries(closes, fast)
	slowEMA := computeEMASeries(closes, slow)
	// The signal EMA seeds from the SMA of its first `signal`
	// inputs, so a leading run of NaN MACD values (the slow EMA's
	// initialisation window) would poison the signal series
	// forever. Strip the NaN prefix and compute the signal EMA
	// over the valid portion only — standard MACD convention.
	var validMACD []float64
	for i := range closes {
		if math.IsNaN(fastEMA[i]) || math.IsNaN(slowEMA[i]) {
			continue
		}
		validMACD = append(validMACD, fastEMA[i]-slowEMA[i])
	}
	if len(validMACD) < signal {
		return 0, 0, 0
	}
	sigSeries := computeEMASeries(validMACD, signal)
	macdLine := validMACD[len(validMACD)-1]
	sigLine := sigSeries[len(sigSeries)-1]
	if math.IsNaN(macdLine) || math.IsNaN(sigLine) {
		return 0, 0, 0
	}
	return macdLine, sigLine, macdLine - sigLine
}

// computeEMASeries returns the exponential moving average series for
// `values`, with NaN entries until the SMA seed at index period-1.
// Mirrors the broker-ta service's `ema` semantics: alpha = 2/(period+1),
// SMA-seeded at the (period-1)-th index. NaN inputs propagate so the
// MACD histogram stays NaN until both fast and slow EMAs are warm.
func computeEMASeries(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period <= 0 || len(values) < period {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	alpha := 2.0 / float64(period+1)
	seedValid := true
	var seed float64
	for i := 0; i < period; i++ {
		if math.IsNaN(values[i]) {
			seedValid = false
		}
		seed += values[i]
	}
	seed /= float64(period)
	for i := 0; i < len(out); i++ {
		switch {
		case i < period-1:
			out[i] = math.NaN()
		case i == period-1:
			if seedValid {
				out[i] = seed
			} else {
				out[i] = math.NaN()
			}
		case math.IsNaN(out[i-1]) || math.IsNaN(values[i]):
			out[i] = math.NaN()
		default:
			out[i] = values[i]*alpha + out[i-1]*(1-alpha)
		}
	}
	return out
}
