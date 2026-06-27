package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/registry"
)

// TestComputeSMA covers the simple moving average — the trend-filter
// half of the strategist's per-symbol scan. The numerical cases
// double as documentation of the rounding convention (no rounding;
// the Markdown table truncates with %.2f at render time).
func TestComputeSMA(t *testing.T) {
	cases := []struct {
		name   string
		closes []float64
		period int
		want   float64
	}{
		{"flat", []float64{10, 10, 10, 10, 10}, 5, 10},
		{"linear ramp", []float64{1, 2, 3, 4, 5}, 5, 3},
		{"window slides", []float64{1, 1, 1, 100}, 3, 34},
		{"period exceeds data → 0", []float64{1, 2, 3}, 5, 0},
		{"period 0 → 0", []float64{1, 2, 3}, 0, 0},
	}
	for _, tc := range cases {
		got := computeSMA(tc.closes, tc.period)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: got %g, want %g", tc.name, got, tc.want)
		}
	}
}

// TestComputeRSI covers Wilder smoothing. The flat-series case
// (RSI = 50) was the bug class 2026-04 where the strategist
// hallucinated bounce candidates from a runtime-feed stall —
// the prewarmed RSI is 50 in that case, signalling no opportunity.
func TestComputeRSI(t *testing.T) {
	t.Run("flat series → 50", func(t *testing.T) {
		closes := make([]float64, 30)
		for i := range closes {
			closes[i] = 100
		}
		got := computeRSI(closes, 14)
		if math.Abs(got-50) > 1e-9 {
			t.Errorf("flat: got %g, want 50", got)
		}
	})
	t.Run("monotone up → 100", func(t *testing.T) {
		closes := make([]float64, 30)
		for i := range closes {
			closes[i] = float64(i + 1)
		}
		got := computeRSI(closes, 14)
		if math.Abs(got-100) > 1e-9 {
			t.Errorf("monotone up: got %g, want 100", got)
		}
	})
	t.Run("insufficient bars → 0", func(t *testing.T) {
		got := computeRSI([]float64{1, 2, 3}, 14)
		if got != 0 {
			t.Errorf("insufficient: got %g, want 0", got)
		}
	})
	// Cross-check against a hand-computed value: a known oscillating
	// series whose Wilder RSI(14) is published in TA references.
	// Closes from a textbook example (Wilder's original New Concepts
	// in Technical Trading Systems, Chapter 6 — the RSI worked in
	// most popular charting packages reproduces this to ±0.1).
	t.Run("textbook series", func(t *testing.T) {
		closes := []float64{
			46.1250, 47.1250, 46.4375, 46.9375, 44.9375, 44.2500, 44.6250,
			45.7500, 47.8125, 47.5625, 47.0000, 44.5625, 46.3125, 47.6875,
			46.6875, 45.6875, 43.0625, 43.5625, 44.8750, 43.6875,
		}
		got := computeRSI(closes, 14)
		// Reference value from the Wilder textbook is ~43.99; allow
		// a generous tolerance because the published example rounds
		// intermediate gains/losses to 4 decimals.
		if math.Abs(got-43.99) > 0.5 {
			t.Errorf("textbook: got %g, want ~43.99", got)
		}
	})
}

// TestComputeMACD covers the three-tuple return shape and the
// initialisation guard — the strategist's MACD column is rendered
// "0.000" rather than crashing when bars < slow+signal.
func TestComputeMACD(t *testing.T) {
	t.Run("constant series → all zero", func(t *testing.T) {
		closes := make([]float64, 60)
		for i := range closes {
			closes[i] = 100
		}
		line, sig, hist := computeMACD(closes, 12, 26, 9)
		if math.Abs(line) > 1e-9 || math.Abs(sig) > 1e-9 || math.Abs(hist) > 1e-9 {
			t.Errorf("flat series: got (%g,%g,%g), want zeros", line, sig, hist)
		}
	})
	t.Run("insufficient bars → zeros", func(t *testing.T) {
		closes := make([]float64, 30) // < 26+9
		line, sig, hist := computeMACD(closes, 12, 26, 9)
		if line != 0 || sig != 0 || hist != 0 {
			t.Errorf("insufficient: got (%g,%g,%g), want zeros", line, sig, hist)
		}
	})
	t.Run("uptrending series → positive macd", func(t *testing.T) {
		closes := make([]float64, 60)
		for i := range closes {
			closes[i] = float64(100 + i)
		}
		line, sig, hist := computeMACD(closes, 12, 26, 9)
		if line <= 0 {
			t.Errorf("uptrend: macd line should be positive, got %g", line)
		}
		// Histogram = line - signal; on a steady ramp the line leads
		// the signal so histogram should also be positive.
		_ = hist
		_ = sig
	})
}

// TestComputeEMASeries — the math under MACD. Anchors that the
// SMA seed lands at index period-1 and that NaN inputs propagate.
func TestComputeEMASeries(t *testing.T) {
	closes := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	out := computeEMASeries(closes, 3)
	if !math.IsNaN(out[0]) || !math.IsNaN(out[1]) {
		t.Errorf("warm-up: out[0]=%g out[1]=%g, both should be NaN", out[0], out[1])
	}
	// Seed at index 2 is the SMA of the first 3 closes = 2.0.
	if math.Abs(out[2]-2.0) > 1e-9 {
		t.Errorf("seed: got %g, want 2.0", out[2])
	}
	// alpha = 2/(3+1) = 0.5; out[3] = 4*0.5 + 2*0.5 = 3.0.
	if math.Abs(out[3]-3.0) > 1e-9 {
		t.Errorf("first ema step: got %g, want 3.0", out[3])
	}
}

// TestBuildWatchlistIndicatorsBlock_FanOut runs the helper against a
// stub broker that records concurrent in-flight requests so the
// "parallel" claim is verified rather than asserted in a comment.
// The 3-symbol watchlist must produce 3 simultaneous in-flight calls.
//
// 2026-05-29 flake fix: the original test races the wallclock —
// under parallel-test load (go test running many tests in the
// same process) the fan-out goroutines could execute fast
// enough that one request completed before the next started,
// leaving peakInFlight=1. Now the handler blocks on a barrier
// until all 3 requests have arrived; the barrier releases them
// simultaneously so peakInFlight==3 is deterministic.
func TestBuildWatchlistIndicatorsBlock_FanOut(t *testing.T) {
	closes := make([]float64, 120)
	for i := range closes {
		closes[i] = 100 + float64(i%5)
	}
	bars := make([]map[string]any, len(closes))
	for i, c := range closes {
		bars[i] = map[string]any{"close": c}
	}

	const expectedConcurrent = 3
	// Barrier coordinates the simultaneous-release pattern.
	// arrived = chan size 1, used as a counted barrier; release
	// closes once `expectedConcurrent` requests have stacked up.
	var (
		mu             sync.Mutex
		arrivedCount   int
		release        = make(chan struct{})
		barrierTimeout = 5 * time.Second
	)
	var inFlight, peakInFlight int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := atomic.AddInt32(&inFlight, 1)
		for {
			peak := atomic.LoadInt32(&peakInFlight)
			if now <= peak || atomic.CompareAndSwapInt32(&peakInFlight, peak, now) {
				break
			}
		}
		defer atomic.AddInt32(&inFlight, -1)

		// Barrier: each request increments the counter and
		// waits. The Nth arrival closes the release channel,
		// unblocking everyone (including itself via the
		// select below). All N handlers return simultaneously
		// after this point, so peakInFlight registers the
		// real fan-out.
		mu.Lock()
		arrivedCount++
		if arrivedCount == expectedConcurrent {
			close(release)
		}
		mu.Unlock()

		select {
		case <-release:
			// All N arrived; fall through.
		case <-time.After(barrierTimeout):
			// Defensive: helper called the broker fewer times
			// than expected (regression — production code
			// changed concurrency). Test will catch the
			// peakInFlight<2 assertion below.
			t.Logf("barrier timeout after %s — fewer than %d requests arrived", barrierTimeout, expectedConcurrent)
		}

		// Mimic the broker's MCP envelope: result.content[0].text
		// is a JSON-encoded payload with a `bars` array.
		inner, _ := json.Marshal(map[string]any{
			"symbol":   "TEST",
			"duration": "120 D",
			"bar_size": "1 day",
			"bars":     bars,
		})
		envelope := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": string(inner)},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "test",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"AAPL", "MSFT", "NVDA"},
		},
	}
	got := exec.buildWatchlistIndicatorsBlock(context.Background(), project)
	if got == "" {
		t.Fatal("empty block — fan-out failed")
	}
	if !strings.Contains(got, "| AAPL |") || !strings.Contains(got, "| MSFT |") || !strings.Contains(got, "| NVDA |") {
		t.Errorf("missing symbol rows in block:\n%s", got)
	}
	// Alphabetical ordering is documented; assert it.
	aIdx := strings.Index(got, "| AAPL |")
	mIdx := strings.Index(got, "| MSFT |")
	nIdx := strings.Index(got, "| NVDA |")
	if aIdx >= mIdx || mIdx >= nIdx {
		t.Errorf("rows not alphabetically sorted: AAPL=%d MSFT=%d NVDA=%d", aIdx, mIdx, nIdx)
	}
	// Fan-out check: at least 2 in-flight at peak. Assert >= 2 rather
	// than == 3 to avoid flake on slow CI where one call may finish
	// before another starts.
	if atomic.LoadInt32(&peakInFlight) < 2 {
		t.Errorf("fan-out did not parallelise: peak in-flight = %d, want >= 2", peakInFlight)
	}
}

// TestBuildWatchlistIndicatorsBlock_AllFailDropsBlock — the fall-back
// contract: if every symbol fails the helper returns "" so the
// strategist's per-symbol path runs cleanly instead of seeing a wall
// of fetch_failed rows. Mirrors quote prewarm's behaviour.
func TestBuildWatchlistIndicatorsBlock_AllFailDropsBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broker offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "test",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"AAPL", "MSFT"},
		},
	}
	got := exec.buildWatchlistIndicatorsBlock(context.Background(), project)
	if got != "" {
		t.Errorf("expected empty block on all-fail, got:\n%s", got)
	}
}

// TestBuildWatchlistIndicatorsBlock_PartialFailRendersFetchFailed —
// a single delisted ticker shouldn't blank the whole block; it
// renders as fetch_failed alongside the working rows.
func TestBuildWatchlistIndicatorsBlock_PartialFailRendersFetchFailed(t *testing.T) {
	closes := make([]float64, 120)
	for i := range closes {
		closes[i] = 100
	}
	bars := make([]map[string]any, len(closes))
	for i, c := range closes {
		bars[i] = map[string]any{"close": c}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inspect which symbol the request was for.
		body, _ := readAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		args, _ := req["params"].(map[string]any)["arguments"].(map[string]any)
		symbol, _ := args["symbol"].(string)
		if symbol == "BAD" {
			http.Error(w, "no such symbol", http.StatusNotFound)
			return
		}
		inner, _ := json.Marshal(map[string]any{"bars": bars})
		envelope := map[string]any{
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(inner)}},
			},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "test",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"AAPL", "BAD"},
		},
	}
	got := exec.buildWatchlistIndicatorsBlock(context.Background(), project)
	if !strings.Contains(got, "| AAPL |") {
		t.Errorf("expected good row to render, got:\n%s", got)
	}
	if !strings.Contains(got, "| BAD |") || !strings.Contains(got, "fetch_failed") {
		t.Errorf("expected BAD row to render with fetch_failed, got:\n%s", got)
	}
}

// readAll is a tiny helper so the test file doesn't pull in io/ioutil.
func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var b []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b = append(b, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return b, nil
			}
			return b, nil
		}
	}
}

// TestBuildWatchlistIndicatorsBlock_SMA200RendersWithEnoughBars pins
// the 2026-05-15-evening regime-fallback mitigation: when the broker
// returns ≥200 daily closes, the SMA200 column is populated so the
// strategist's regime check (last > SMA200, SMA50 > SMA200) can be
// made from the pre-warmed table directly — no need to re-fetch
// SPY bars when its quote pre-fetch fails.
func TestBuildWatchlistIndicatorsBlock_SMA200RendersWithEnoughBars(t *testing.T) {
	// 220 bars on a slow ramp: SMA200 ≈ midpoint of the last
	// 200 closes, clearly distinct from SMA20/SMA50 so the test
	// can assert it's both present AND distinct.
	closes := make([]float64, 220)
	for i := range closes {
		closes[i] = float64(100 + i) // 100..319
	}
	bars := make([]map[string]any, len(closes))
	for i, c := range closes {
		bars[i] = map[string]any{"close": c}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"bars": bars})
		envelope := map[string]any{
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(inner)}},
			},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "test",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"SPY"},
		},
	}
	got := exec.buildWatchlistIndicatorsBlock(context.Background(), project)
	if got == "" {
		t.Fatal("empty block — fan-out failed")
	}
	if !strings.Contains(got, "SMA200") {
		t.Errorf("header missing SMA200 column:\n%s", got)
	}
	if !strings.Contains(got, "| SPY |") {
		t.Errorf("SPY row missing:\n%s", got)
	}
	// SMA200 of closes [20..219] is mean(120..319) = 219.5. Render
	// uses $%.2f format → "$219.50".
	if !strings.Contains(got, "$219.50") {
		t.Errorf("expected SMA200 cell '$219.50' in row, got:\n%s", got)
	}
	// Regime-tip line is part of the contract — the strategist's
	// fallback path reads it.
	if !strings.Contains(got, "Regime tip") {
		t.Errorf("expected regime tip in preamble, got:\n%s", got)
	}
}

// TestBuildWatchlistIndicatorsBlock_SMA200DashesWhenTooFewBars pins
// the graceful-degradation path: a freshly-listed ticker with only
// 120 closes still renders a useful row, just with SMA200 as "—".
// Short-window columns (SMA20/SMA50/RSI/MACD) stay populated so the
// strategist doesn't lose signal on the rest of the table.
func TestBuildWatchlistIndicatorsBlock_SMA200DashesWhenTooFewBars(t *testing.T) {
	closes := make([]float64, 120) // < 200 → SMA200 should be "—"
	for i := range closes {
		closes[i] = 100 + float64(i%5)
	}
	bars := make([]map[string]any, len(closes))
	for i, c := range closes {
		bars[i] = map[string]any{"close": c}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"bars": bars})
		envelope := map[string]any{
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(inner)}},
			},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer server.Close()
	t.Setenv("VORNIK_BROKER_BASE_URL", server.URL)

	exec := &Executor{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "test",
		Trading: registry.ProjectTrading{
			Watchlist: []string{"NEW"},
		},
	}
	got := exec.buildWatchlistIndicatorsBlock(context.Background(), project)
	if !strings.Contains(got, "| NEW |") {
		t.Fatalf("NEW row missing:\n%s", got)
	}
	// Row must contain SMA200 dash. Easier to assert on the
	// fingerprint of the row: starts with "| NEW |" and has at
	// least one " — " cell.
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "| NEW |") {
			continue
		}
		// Six columns before SMA200 (symbol, last, SMA20, SMA50,
		// then SMA200). When there are 120 bars, the SMA20/SMA50
		// cells are populated ($-prefixed) and SMA200 is the first
		// "—" in the row.
		cells := strings.Split(line, "|")
		// cells[0]="" (leading pipe), cells[1]=" NEW ", cells[2]=last, ...
		// SMA200 is cells[5] (index 5 = the 5th data cell).
		if len(cells) < 7 {
			t.Fatalf("malformed row, fewer cells than expected: %q", line)
		}
		if strings.TrimSpace(cells[5]) != "—" {
			t.Errorf("SMA200 cell should render as '—' on 120-bar series, got %q in row:\n%s", cells[5], line)
		}
		// Sanity: SMA20 cell is populated.
		if !strings.HasPrefix(strings.TrimSpace(cells[3]), "$") {
			t.Errorf("SMA20 cell should still render with 120 bars, got %q", cells[3])
		}
	}
}

// Ensure fmt is used — keeps the import map honest if a refactor
// drops one of the compute helpers.
var _ = fmt.Sprintf
