package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/registry"
)

// quotePrewarmTimeout caps how long the parallel fetcher waits
// for ALL symbols to come back. Pre-fix this was 6s, which
// truncated IBKR's delayed-feed snapshot (which can take 8-12s
// to deliver on paper accounts) and produced a wall of
// fetch_failed rows. Aligned with the broker sidecar's 15s
// snapshot wait + a 5s buffer for HTTP overhead and the parallel
// fan-out's worst-case scheduler delay. Symbols whose snapshot
// truly times out fall back to per-symbol get_quote at the
// strategist's request.
const quotePrewarmTimeout = 20 * time.Second

// buildWatchlistQuotesBlock fetches every watchlist symbol's
// latest quote in parallel and renders a compact markdown
// table the strategist can scan. Returns "" when the project
// has no configured watchlist or the broker is unreachable —
// the strategist's per-symbol get_quote fallback then takes
// over (back-compat).
//
// Best-effort: per-symbol failures are recorded in the block
// (status "fetch_failed") rather than failing the whole
// pre-warm, so a single delisted ticker doesn't blank the
// strategist's view.
//
// This helper deliberately avoids the daemon's MCP client
// machinery (which is per-project, per-server, mutex-locked,
// and would serialise the calls). HTTP+JSON-RPC against the
// broker's /message endpoint is an order of magnitude lighter
// for a 16-symbol fan-out.
func (e *Executor) buildWatchlistQuotesBlock(ctx context.Context, project *registry.Project) string {
	if project == nil || len(project.Trading.Watchlist) == 0 {
		return ""
	}
	brokerURL := brokerBaseURL()

	// Bounded by the watchlist size (typically 16); no need
	// for a worker pool. Each goroutine has its own ctx
	// derived from the timeout below so a stuck symbol
	// doesn't hold up the whole batch.
	probeCtx, cancel := context.WithTimeout(ctx, quotePrewarmTimeout)
	defer cancel()

	type quoteResult struct {
		symbol string
		last   float64
		bid    float64
		ask    float64
		dly    bool
		err    string
	}
	results := make([]quoteResult, len(project.Trading.Watchlist))
	var wg sync.WaitGroup
	for i, sym := range project.Trading.Watchlist {
		wg.Add(1)
		go func(idx int, symbol string) {
			defer wg.Done()
			q, err := fetchQuote(probeCtx, brokerURL, project.ID, symbol)
			if err != nil {
				results[idx] = quoteResult{symbol: symbol, err: truncateForPrompt(err.Error(), 60)}
				return
			}
			results[idx] = quoteResult{
				symbol: symbol,
				last:   q.Last,
				bid:    q.Bid,
				ask:    q.Ask,
				dly:    q.Delayed,
			}
		}(i, sym)
	}
	wg.Wait()

	// Treat zero-valued price fields as a fetch failure. IBKR's
	// delayed snapshot frequently returns 200 with last/bid/ask
	// populated as null when the snapshot doesn't arrive within
	// the sidecar's wait window — the wire shape parses fine but
	// the prices are zero. Rendering those as "$0.00" caused the
	// strategist to hallucinate proposals from training data
	// (observed task_20260505190701_d9c47de2d46feaf3 — the
	// Kimi model invented limit prices for symbols whose
	// pre-fetched row was $0.00 across all three fields).
	// Mark such rows fetch_failed so the strategist sees they're
	// unusable and skips the symbol cleanly.
	for i := range results {
		if results[i].err != "" {
			continue
		}
		if results[i].last == 0 && results[i].bid == 0 && results[i].ask == 0 {
			results[i].err = "no_price_data (broker returned 200 but last/bid/ask all empty)"
		}
	}

	// If every fetch failed, return empty so the strategist
	// falls back to per-symbol calls instead of seeing a wall
	// of "fetch_failed" rows.
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
			Msg("quote pre-warm: every symbol failed (or returned empty prices); strategist will fall back to per-symbol get_quote")
		return ""
	}

	// Sort alphabetically for deterministic prompt ordering —
	// makes diffing prompts across ticks possible.
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].symbol < results[j].symbol
	})

	var sb strings.Builder
	sb.WriteString("Pre-fetched quotes for the project's watchlist (one parallel batch). Use these in place of per-symbol mcp__broker__get_quote calls — the data is fresh as of the strategist's start. Re-fetch a single symbol via the tool only if a quote here shows fetch_failed or is suspiciously stale.\n\n")
	sb.WriteString("| symbol | last | bid | ask | delayed | status |\n")
	sb.WriteString("|--------|------|-----|-----|---------|--------|\n")
	for _, r := range results {
		status := "ok"
		if r.err != "" {
			status = "fetch_failed: " + r.err
			fmt.Fprintf(&sb, "| %s | — | — | — | — | %s |\n", r.symbol, status)
			continue
		}
		dly := "no"
		if r.dly {
			dly = "yes"
		}
		fmt.Fprintf(&sb, "| %s | $%.2f | $%.2f | $%.2f | %s | %s |\n",
			r.symbol, r.last, r.bid, r.ask, dly, status)
	}
	return sb.String()
}

// fetchQuote calls the broker's MCP tool/get_quote via JSON-RPC
// for a single symbol. Designed to be cheap + side-effect-free
// so the parallel fan-out in buildWatchlistQuotesBlock is safe.
//
// Project ID propagates as X-Project-ID per the broker's
// authorization contract; without it place_order would refuse
// (irrelevant here — this is a read), but quotes still observe
// the header when present so the broker's per-project audit
// is consistent.
type prewarmQuote struct {
	Symbol  string  `json:"symbol"`
	Last    float64 `json:"last"`
	Bid     float64 `json:"bid"`
	Ask     float64 `json:"ask"`
	Delayed bool    `json:"delayed"`
}

func fetchQuote(ctx context.Context, brokerURL, projectID, symbol string) (*prewarmQuote, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_quote",
			"arguments": map[string]any{"symbol": symbol},
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	// Parse JSON-RPC response. Broker returns
	// {jsonrpc, id, result: {content: [{type: "text", text:
	// "<json blob>"}]}}.
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
	var q prewarmQuote
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &q); err != nil {
		return nil, fmt.Errorf("quote parse: %w", err)
	}
	return &q, nil
}
