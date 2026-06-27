package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/registry"
)

// stubBarsServer returns an httptest server whose get_historical_bars
// response is `n` daily closes all equal to `close`, so SMA(any window
// <= n) == close. Lets the entry-gate-indicators test assert an exact
// SMA(50) without reimplementing the indicator math.
func stubBarsServer(t *testing.T, n int, close float64) *httptest.Server {
	t.Helper()
	bars := make([]map[string]any, n)
	for i := range bars {
		bars[i] = map[string]any{"close": close}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"bars": bars})
		envelope := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(inner)}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
}

// TestEntryGateIndicators_FetchesOnlyProposedLongOpens — the helper must
// fetch a deterministic SMA(50) for each LONG-OPEN proposal that is on
// the watchlist, and nothing else: closes/exits don't need a floor
// check, and symbols never proposed shouldn't cost a broker round-trip.
func TestEntryGateIndicators_FetchesOnlyProposedLongOpens(t *testing.T) {
	server := stubBarsServer(t, 60, 100.0) // 60 closes of 100 → SMA50 = 100
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:      "ibkr-trader",
		Trading: registry.ProjectTrading{Watchlist: []string{"NVDA", "AAPL", "MSFT"}},
	}
	result := []byte(`{
		"proposals": [
			{"symbol": "NVDA", "action": "BUY", "intent": "open", "limit_price": 95.0},
			{"symbol": "AAPL", "action": "SELL", "intent": "close", "limit_price": 90.0}
		]
	}`)

	got := exec.entryGateIndicators(context.Background(), project, result)

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 indicator row (NVDA long-open only), got %d: %#v", len(got), got)
	}
	ind, ok := got["NVDA"]
	if !ok {
		t.Fatalf("expected NVDA in indicator map, got keys %#v", got)
	}
	if ind.SMA50 != 100.0 {
		t.Errorf("NVDA SMA50 = %v, want 100.0", ind.SMA50)
	}
	if _, exists := got["AAPL"]; exists {
		t.Error("AAPL is a close/exit — must not be fetched for the entry gate")
	}
}

// TestEntryGateIndicators_NilWhenNoLongOpens — no open proposals (or a
// non-trading project) means no broker calls and a nil map, keeping the
// verifier a clean no-op.
func TestEntryGateIndicators_NilWhenNoLongOpens(t *testing.T) {
	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:      "ibkr-trader",
		Trading: registry.ProjectTrading{Watchlist: []string{"NVDA"}},
	}
	result := []byte(`{"proposals":[{"symbol":"NVDA","action":"SELL","intent":"close","limit_price":1}]}`)
	got := exec.entryGateIndicators(context.Background(), project, result)
	if got != nil {
		t.Errorf("expected nil map when there are no long opens, got %#v", got)
	}
}
