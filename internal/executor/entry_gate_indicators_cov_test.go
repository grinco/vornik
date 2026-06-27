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

// TestEntryGateIndicatorsCov_NilAndEmptyGuards covers the early-out
// guards: nil project, empty watchlist, empty result bytes.
func TestEntryGateIndicatorsCov_NilAndEmptyGuards(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	ctx := context.Background()
	if got := e.entryGateIndicators(ctx, nil, []byte(`{}`)); got != nil {
		t.Error("nil project should yield nil")
	}
	proj := &registry.Project{ID: "p"} // no watchlist
	if got := e.entryGateIndicators(ctx, proj, []byte(`{}`)); got != nil {
		t.Error("empty watchlist should yield nil")
	}
	withWL := &registry.Project{ID: "p", Trading: registry.ProjectTrading{Watchlist: []string{"NVDA"}}}
	if got := e.entryGateIndicators(ctx, withWL, nil); got != nil {
		t.Error("empty result bytes should yield nil")
	}
}

// TestEntryGateIndicatorsCov_OffWatchlistSkipped covers the
// off-watchlist skip: a long-open proposal for a symbol NOT on the
// watchlist costs no broker call and is left out of the map.
func TestEntryGateIndicatorsCov_OffWatchlistSkipped(t *testing.T) {
	// Server should never be hit for the off-watchlist symbol; if it
	// is, it returns enough bars to populate — so an empty result map
	// proves the skip.
	server := stubBarsServer(t, 60, 100.0)
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:      "p",
		Trading: registry.ProjectTrading{Watchlist: []string{"AAPL"}}, // NVDA not listed
	}
	result := []byte(`{"proposals":[{"symbol":"NVDA","action":"BUY","intent":"open","limit_price":1}]}`)
	got := e.entryGateIndicators(context.Background(), project, result)
	if got != nil {
		t.Errorf("off-watchlist proposal should be skipped → nil map, got %#v", got)
	}
}

// TestEntryGateIndicatorsCov_FetchFailureAbstains covers the
// per-symbol fetch-failure branch: the broker errors, the symbol is
// dropped, and the overall map is nil (no usable indicators).
func TestEntryGateIndicatorsCov_FetchFailureAbstains(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broker down", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:      "p",
		Trading: registry.ProjectTrading{Watchlist: []string{"NVDA"}},
	}
	result := []byte(`{"proposals":[{"symbol":"NVDA","action":"BUY","intent":"open","limit_price":1}]}`)
	got := e.entryGateIndicators(context.Background(), project, result)
	if got != nil {
		t.Errorf("fetch failure should drop the symbol → nil map, got %#v", got)
	}
}

// TestEntryGateIndicatorsCov_TooFewBarsAbstains covers the SMA50<=0
// branch: fewer than 50 closes → computeSMA returns 0 → symbol left
// out so the verifier abstains rather than comparing against 0.
func TestEntryGateIndicatorsCov_TooFewBarsAbstains(t *testing.T) {
	server := stubBarsServer(t, 10, 100.0) // only 10 closes → SMA(50)=0
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:      "p",
		Trading: registry.ProjectTrading{Watchlist: []string{"NVDA"}},
	}
	result := []byte(`{"proposals":[{"symbol":"NVDA","action":"BUY","intent":"open","limit_price":1}]}`)
	got := e.entryGateIndicators(context.Background(), project, result)
	if got != nil {
		t.Errorf("degenerate SMA(50)=0 should be dropped → nil map, got %#v", got)
	}
}

// --- prewarm fetch + helper coverage ---

// TestBrokerBaseURLCov_Default covers the env-unset default branch.
func TestBrokerBaseURLCov_Default(t *testing.T) {
	t.Setenv("VORNIK_BROKER_BASE_URL", "")
	if got := brokerBaseURL(); got != "http://127.0.0.1:8788" {
		t.Errorf("default broker URL = %q", got)
	}
}

// TestFetchDailyBarsCov_ErrorPaths exercises the non-200, broker
// error envelope, empty content, and bad-JSON branches of
// fetchDailyBars.
func TestFetchDailyBarsCov_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	// Non-200 status.
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer s1.Close()
	if _, err := fetchDailyBars(ctx, s1.URL, "p", "X"); err == nil || !contains(err.Error(), "broker returned 502") {
		t.Errorf("non-200: %v", err)
	}

	// Broker error envelope.
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
	}))
	defer s2.Close()
	if _, err := fetchDailyBars(ctx, s2.URL, "p", "X"); err == nil || !contains(err.Error(), "rate limited") {
		t.Errorf("broker error envelope: %v", err)
	}

	// Empty content.
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []any{}}})
	}))
	defer s3.Close()
	if _, err := fetchDailyBars(ctx, s3.URL, "p", "X"); err == nil || !contains(err.Error(), "empty broker result") {
		t.Errorf("empty content: %v", err)
	}

	// Inner bars text not valid JSON.
	s4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "not json"}}},
		})
	}))
	defer s4.Close()
	if _, err := fetchDailyBars(ctx, s4.URL, "p", "X"); err == nil || !contains(err.Error(), "bars parse") {
		t.Errorf("bad inner JSON: %v", err)
	}

	// Top-level envelope not JSON.
	s5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("garbage"))
	}))
	defer s5.Close()
	if _, err := fetchDailyBars(ctx, s5.URL, "p", "X"); err == nil || !contains(err.Error(), "envelope parse") {
		t.Errorf("bad envelope JSON: %v", err)
	}

	// Empty projectID skips the X-Project-ID header branch.
	if _, err := fetchDailyBars(ctx, s1.URL, "", "X"); err == nil {
		t.Error("expected error even without project id")
	}
}

// TestFetchQuoteCov_ErrorPaths mirrors the fetchQuote error branches.
func TestFetchQuoteCov_ErrorPaths(t *testing.T) {
	ctx := context.Background()

	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer s1.Close()
	if _, err := fetchQuote(ctx, s1.URL, "p", "X"); err == nil || !contains(err.Error(), "broker returned 503") {
		t.Errorf("non-200: %v", err)
	}

	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
	}))
	defer s2.Close()
	if _, err := fetchQuote(ctx, s2.URL, "p", "X"); err == nil || !contains(err.Error(), "boom") {
		t.Errorf("broker error envelope: %v", err)
	}

	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []any{}}})
	}))
	defer s3.Close()
	if _, err := fetchQuote(ctx, s3.URL, "p", "X"); err == nil || !contains(err.Error(), "empty broker result") {
		t.Errorf("empty content: %v", err)
	}

	s4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "{bad"}}},
		})
	}))
	defer s4.Close()
	if _, err := fetchQuote(ctx, s4.URL, "p", "X"); err == nil || !contains(err.Error(), "quote parse") {
		t.Errorf("bad inner JSON: %v", err)
	}

	s5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("garbage"))
	}))
	defer s5.Close()
	if _, err := fetchQuote(ctx, s5.URL, "p", "X"); err == nil || !contains(err.Error(), "envelope parse") {
		t.Errorf("bad envelope JSON: %v", err)
	}
}

// TestBuildWatchlistBlocksCov_NilProjectGuards covers the nil/empty
// guards on both block builders.
func TestBuildWatchlistBlocksCov_NilProjectGuards(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	ctx := context.Background()
	if got := e.buildWatchlistIndicatorsBlock(ctx, nil); got != "" {
		t.Error("nil project → empty indicators block")
	}
	if got := e.buildWatchlistIndicatorsBlock(ctx, &registry.Project{ID: "p"}); got != "" {
		t.Error("empty watchlist → empty indicators block")
	}
	if got := e.buildWatchlistQuotesBlock(ctx, nil); got != "" {
		t.Error("nil project → empty quotes block")
	}
	if got := e.buildWatchlistQuotesBlock(ctx, &registry.Project{ID: "p"}); got != "" {
		t.Error("empty watchlist → empty quotes block")
	}
}

// TestBuildWatchlistIndicatorsBlockCov_TooFewBars covers the
// too_few_bars (<50 closes) row branch.
func TestBuildWatchlistIndicatorsBlockCov_TooFewBars(t *testing.T) {
	server := stubBarsServer(t, 10, 100.0) // 10 closes < 50
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)
	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{ID: "p", Trading: registry.ProjectTrading{Watchlist: []string{"NVDA"}}}
	got := e.buildWatchlistIndicatorsBlock(context.Background(), project)
	if got != "" {
		t.Errorf("a single too-few-bars symbol is the only one → all-failed → empty block, got:\n%s", got)
	}
}

// TestBuildWatchlistQuotesBlockCov_NoPriceData covers the
// zero-price → no_price_data row branch + the all-failed drop. A
// 200 response with zero last/bid/ask is treated as a fetch failure.
func TestBuildWatchlistQuotesBlockCov_NoPriceData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"symbol": "NVDA", "last": 0, "bid": 0, "ask": 0})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}},
		})
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)
	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{ID: "p", Trading: registry.ProjectTrading{Watchlist: []string{"NVDA"}}}
	got := e.buildWatchlistQuotesBlock(context.Background(), project)
	// Single symbol, zero prices → marked fetch_failed → all-failed →
	// empty block.
	if got != "" {
		t.Errorf("zero-price single symbol → all-failed → empty block, got:\n%s", got)
	}
}

// TestBuildWatchlistQuotesBlockCov_HappyAndPartial covers a populated
// quotes block (the ok-row render path, including delayed=yes) with
// a second fetch_failed row so the block is not dropped.
func TestBuildWatchlistQuotesBlockCov_HappyAndPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		args, _ := req["params"].(map[string]any)["arguments"].(map[string]any)
		symbol, _ := args["symbol"].(string)
		if symbol == "BAD" {
			http.Error(w, "no such symbol", http.StatusNotFound)
			return
		}
		inner, _ := json.Marshal(map[string]any{"symbol": symbol, "last": 101.5, "bid": 101.4, "ask": 101.6, "delayed": true})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}},
		})
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)
	e := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{ID: "p", Trading: registry.ProjectTrading{Watchlist: []string{"AAPL", "BAD"}}}
	got := e.buildWatchlistQuotesBlock(context.Background(), project)
	if !contains(got, "| AAPL |") || !contains(got, "$101.50") {
		t.Errorf("expected populated AAPL row with delayed=yes, got:\n%s", got)
	}
	if !contains(got, "| BAD |") || !contains(got, "fetch_failed") {
		t.Errorf("expected BAD fetch_failed row, got:\n%s", got)
	}
	if !contains(got, "yes") {
		t.Errorf("expected delayed=yes column, got:\n%s", got)
	}
}
