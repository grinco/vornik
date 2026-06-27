package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/registry"
)

// brokerStub returns a per-symbol JSON-RPC response. Used to
// drive the fetchQuote / buildWatchlistQuotesBlock unit tests
// without a running broker.
type brokerStub struct {
	server *httptest.Server
	// quoteByText is the per-symbol JSON blob the broker returns
	// in the JSON-RPC text content. Lets the test simulate
	// IBKR's quirks: live quote with prices, delayed quote with
	// prices, delayed quote with NO prices (the zero-bug
	// scenario).
	quoteByText map[string]string
}

func newBrokerStub(t *testing.T, quoteByText map[string]string) *brokerStub {
	t.Helper()
	s := &brokerStub{quoteByText: quoteByText}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sym, _ := rpc.Params.Arguments["symbol"].(string)
		text, ok := s.quoteByText[sym]
		if !ok {
			text = `{"symbol":"` + sym + `","delayed":true}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		})
	}))
	t.Cleanup(s.server.Close)
	return s
}

// TestBuildWatchlistQuotesBlock_ZeroPricesMarkedFetchFailed —
// the regression that broke task_20260505190701: IBKR's delayed
// snapshot returned `{symbol, delayed:true}` with NO last/bid/ask
// fields, which decoded into zero-valued floats. Pre-fix the table
// rendered "$0.00" rows that the strategist treated as real quotes
// and hallucinated proposals against. Post-fix any row whose
// last/bid/ask are ALL zero is marked fetch_failed so the
// strategist sees the data is unusable and skips the symbol.
func TestBuildWatchlistQuotesBlock_ZeroPricesMarkedFetchFailed(t *testing.T) {
	stub := newBrokerStub(t, map[string]string{
		// AAPL: empty delayed quote (the production bug shape).
		"AAPL": `{"symbol":"AAPL","delayed":true}`,
		// NVDA: live, populated quote.
		"NVDA": `{"symbol":"NVDA","last":140.50,"bid":140.48,"ask":140.52,"delayed":false}`,
	})
	t.Setenv("VORNIK_BROKER_BASE_URL", stub.server.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "ibkr-trader",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"AAPL", "NVDA"},
		},
	}
	block := e.buildWatchlistQuotesBlock(context.Background(), project)

	assert.Contains(t, block, "NVDA",
		"valid quote must render in the table")
	assert.Contains(t, block, "$140.50",
		"populated last price must appear")

	// AAPL must be marked fetch_failed, not "$0.00".
	assert.Contains(t, block, "AAPL")
	assert.Contains(t, block, "no_price_data",
		"empty delayed quote must surface as fetch_failed so the strategist skips the symbol")
	assert.NotContains(t, block, "| AAPL | $0.00 |",
		"zero-priced row must NOT render as a usable quote")
}

// TestBuildWatchlistQuotesBlock_AllSymbolsEmptyReturnsBlank —
// when every symbol's quote is empty, the helper returns ""
// rather than a wall-of-failure table. The strategist falls back
// to per-symbol get_quote, which has its own retry semantics.
func TestBuildWatchlistQuotesBlock_AllSymbolsEmptyReturnsBlank(t *testing.T) {
	stub := newBrokerStub(t, map[string]string{
		"AAPL": `{"symbol":"AAPL","delayed":true}`,
		"MSFT": `{"symbol":"MSFT","delayed":true}`,
	})
	t.Setenv("VORNIK_BROKER_BASE_URL", stub.server.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "ibkr-trader",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"AAPL", "MSFT"},
		},
	}
	block := e.buildWatchlistQuotesBlock(context.Background(), project)

	assert.Empty(t, block,
		"empty block must signal 'use the per-symbol fallback' rather than rendering a useless table")
}

// TestBuildWatchlistQuotesBlock_PartialFailureRendersUsableRow —
// a mix of populated + empty quotes: the populated rows render
// normally, the empty rows render fetch_failed.
func TestBuildWatchlistQuotesBlock_PartialFailureRendersUsableRow(t *testing.T) {
	stub := newBrokerStub(t, map[string]string{
		"AAPL": `{"symbol":"AAPL","delayed":true}`,
		"NVDA": `{"symbol":"NVDA","last":140.50,"bid":140.48,"ask":140.52,"delayed":false}`,
		"MSFT": `{"symbol":"MSFT","delayed":true}`,
	})
	t.Setenv("VORNIK_BROKER_BASE_URL", stub.server.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "ibkr-trader",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"AAPL", "NVDA", "MSFT"},
		},
	}
	block := e.buildWatchlistQuotesBlock(context.Background(), project)

	assert.Contains(t, block, "$140.50", "NVDA's live quote must render")
	assert.Contains(t, block, "no_price_data",
		"AAPL/MSFT must be marked unusable")
}
