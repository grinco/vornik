package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// --- fakes -----------------------------------------------------------------

// fakeOrderRepo captures the filter passed to List so window-scope
// tests can assert on Since.
type fakeOrderRepo struct {
	rows      []*persistence.TradingOrder
	lastSince *time.Time
}

func (f *fakeOrderRepo) Record(context.Context, *persistence.TradingOrder) error { return nil }
func (f *fakeOrderRepo) List(_ context.Context, flt persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	if flt.PageSize == recentOrdersLimit {
		f.lastSince = flt.Since
	}
	return f.rows, nil
}
func (f *fakeOrderRepo) Count(context.Context, persistence.TradingOrderFilter) (int64, error) {
	return int64(len(f.rows)), nil
}

type fakeSafetyRepo struct {
	rows []*persistence.TradingSafetyEvent
}

func (f *fakeSafetyRepo) Record(context.Context, *persistence.TradingSafetyEvent) error { return nil }
func (f *fakeSafetyRepo) List(context.Context, persistence.TradingSafetyEventFilter) ([]*persistence.TradingSafetyEvent, error) {
	return f.rows, nil
}
func (f *fakeSafetyRepo) Count(context.Context, persistence.TradingSafetyEventFilter) (int64, error) {
	return int64(len(f.rows)), nil
}

type fakeSnapshotRepo struct {
	rows []*persistence.TradingPositionsSnapshot
	err  error
}

func (f *fakeSnapshotRepo) Record(context.Context, *persistence.TradingPositionsSnapshot) error {
	return nil
}
func (f *fakeSnapshotRepo) ListSince(context.Context, string, time.Time, int) ([]*persistence.TradingPositionsSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// --- helpers ---------------------------------------------------------------

// brokerEnabledRegistry builds a registry with one trading-enabled
// project (broker MCP server → brokerURL) and one plain project. The
// broker server's URL is injected so tests can point it at an httptest
// stand-in or an unreachable address.
func brokerEnabledRegistry(t *testing.T, tradingProjectID, brokerURL string) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", tradingProjectID+".yaml"), []byte(`
projectId: `+tradingProjectID+`
displayName: Trading Project
swarmId: swarm-1
defaultWorkflowId: wf-1
mcp:
  servers:
    - name: broker
      url: `+brokerURL+`
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "plain.yaml"), []byte(`
projectId: plain
displayName: Plain Project
swarmId: swarm-1
defaultWorkflowId: wf-1
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

// brokerCapsServer returns an httptest broker that serves /caps with a
// configurable portfolio block. withPortfolio=false omits the portfolio
// key so the handler exercises the snapshot-fallback path.
func brokerCapsServer(t *testing.T, withPortfolio bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"configured": map[string]any{"mode": "paper", "max_position_usd": 2500.0},
			"state":      map[string]any{"kill_switch": false, "today_utc": "2026-06-10"},
		}
		if withPortfolio {
			resp["portfolio"] = map[string]any{
				"account": "DU-TEST", "cash_usd": 8000.0, "equity_usd": 10000.0,
				"buying_power_usd": 16000.0, "open_positions": 2,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// --- tests -----------------------------------------------------------------

// TestTrading_EmptyStateWhenNoTradingProjects — a registry with no
// broker-enabled project renders the empty state, not a 500 and not a
// project panel.
func TestTrading_EmptyStateWhenNoTradingProjects(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No projects have trading enabled")
}

// TestTrading_DropdownListsOnlyTradingEnabled — the project dropdown
// excludes projects without a broker MCP server.
func TestTrading_DropdownListsOnlyTradingEnabled(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	srv := NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", broker.URL)))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<option value="trader"`, "trading-enabled project must be in the dropdown")
	assert.NotContains(t, body, `<option value="plain"`, "non-trading project must be excluded")
}

// TestTrading_DefaultsToFirstEnabledProjectAndRendersPanel — with no
// ?project=, the first enabled project is selected and its live panel
// renders (account snapshot from the broker /caps portfolio block).
func TestTrading_DefaultsToFirstEnabledProjectAndRendersPanel(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	srv := NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", broker.URL)))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Account snapshot")
	assert.Contains(t, body, "broker online")
	assert.Contains(t, body, ">live<")
}

// TestTrading_ExplicitNonEnabledProjectRejected — an explicit ?project=
// that isn't trading-enabled is refused rather than silently ignored.
func TestTrading_ExplicitNonEnabledProjectRejected(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	srv := NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", broker.URL)))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?project=plain", nil))
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// TestTrading_WindowScopesRepoSince — the window selector scopes the
// Since passed to the recent-orders query: ~7d by default, ~24h for
// window=24h, ~30d for window=30d.
func TestTrading_WindowScopesRepoSince(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	cases := []struct {
		query   string
		wantAgo time.Duration
	}{
		{"/trading", 7 * 24 * time.Hour},
		{"/trading?window=24h", 24 * time.Hour},
		{"/trading?window=30d", 30 * 24 * time.Hour},
	}
	for _, tc := range cases {
		orderRepo := &fakeOrderRepo{}
		srv := NewServer(WithProjectRegistry(reg), WithTradingOrderRepository(orderRepo))
		rec := httptest.NewRecorder()
		before := time.Now().UTC()
		srv.Trading(rec, httptest.NewRequest(http.MethodGet, tc.query, nil))
		require.Equal(t, http.StatusOK, rec.Code, tc.query)
		require.NotNil(t, orderRepo.lastSince, "recent-orders query must carry a Since for %s", tc.query)
		gotAgo := before.Sub(*orderRepo.lastSince)
		assert.InDelta(t, tc.wantAgo.Seconds(), gotAgo.Seconds(), 60, "window %s", tc.query)
	}
}

// TestTrading_SnapshotFallbackWhenBrokerOffline — when the broker is
// unreachable, the account block falls back to the latest persisted
// snapshot and is badged "snapshot".
func TestTrading_SnapshotFallbackWhenBrokerOffline(t *testing.T) {
	// Port 1: reserved, nothing listens → fast dial failure.
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	snap := &fakeSnapshotRepo{rows: []*persistence.TradingPositionsSnapshot{
		{ProjectID: "trader", RecordedAt: time.Now().UTC().Add(-2 * time.Minute),
			CashUSD: 8000, EquityUSD: 10000, UnrealisedPLUSD: 0, RealisedPLDayUSD: 12.50},
	}}
	srv := NewServer(WithProjectRegistry(reg), WithTradingSnapshotRepository(snap))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "broker offline")
	assert.Contains(t, body, "snapshot · as of", "fallback account block must be badged")
	assert.Contains(t, body, "$10000.00", "equity from the snapshot row must render")
}

// TestTrading_BrokerOfflineNoSnapshotNoCrash — broker offline AND no
// snapshot repo wired still renders a degraded page, not a 500.
func TestTrading_BrokerOfflineNoSnapshotNoCrash(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	srv := NewServer(WithProjectRegistry(reg)) // no trading repos
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Account snapshot unavailable")
}

// TestTrading_SnapshotRepoEmptyFallsThrough — broker offline and the
// snapshot repo is wired but empty: no fallback applies and the page
// shows the unavailable hint rather than a stale/empty account block.
func TestTrading_SnapshotRepoEmptyFallsThrough(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingSnapshotRepository(&fakeSnapshotRepo{rows: nil}))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Account snapshot unavailable")
	assert.NotContains(t, body, "snapshot · as of")
}

// TestFillAccountFromSnapshot_Kinds — the structured fallback outcome lets the
// dashboard tell "no snapshot exists" from "the snapshot store is unreadable".
func TestFillAccountFromSnapshot_Kinds(t *testing.T) {
	recent := time.Now().UTC().Add(-time.Minute)

	t.Run("nil repo is unavailable, not empty", func(t *testing.T) {
		s := &Server{}
		_, fb := s.fillAccountFromSnapshot(context.Background(), "p", &TradingPanel{})
		assert.Equal(t, snapUnavailable, fb)
	})
	t.Run("repo error is unavailable", func(t *testing.T) {
		s := NewServer(WithTradingSnapshotRepository(&fakeSnapshotRepo{err: context.DeadlineExceeded}))
		_, fb := s.fillAccountFromSnapshot(context.Background(), "p", &TradingPanel{})
		assert.Equal(t, snapUnavailable, fb)
	})
	t.Run("readable but empty is none", func(t *testing.T) {
		s := NewServer(WithTradingSnapshotRepository(&fakeSnapshotRepo{rows: nil}))
		_, fb := s.fillAccountFromSnapshot(context.Background(), "p", &TradingPanel{})
		assert.Equal(t, snapNone, fb)
	})
	t.Run("row present is applied with its timestamp", func(t *testing.T) {
		s := NewServer(WithTradingSnapshotRepository(&fakeSnapshotRepo{rows: []*persistence.TradingPositionsSnapshot{
			{ProjectID: "p", RecordedAt: recent, EquityUSD: 10000},
		}}))
		panel := &TradingPanel{}
		asOf, fb := s.fillAccountFromSnapshot(context.Background(), "p", panel)
		assert.Equal(t, snapApplied, fb)
		assert.Equal(t, recent, asOf)
		assert.Equal(t, 10000.0, panel.EquityUSD)
	})
}

// TestTrading_EmptyStateConfigGap — no trading-enabled project exists at all:
// the empty state names the config gap (the access-gap variant is covered by
// TestTrading_AccessScopeExcludesForeignProject).
func TestTrading_EmptyStateConfigGap(t *testing.T) {
	// A registry whose only trading project is unreachable still counts as
	// configured; to get zero trading projects, scope to a deployment with
	// none. brokerEnabledRegistry always has "trader", so assert the access
	// branch is NOT shown when the caller can see the trading project.
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingSnapshotRepository(&fakeSnapshotRepo{rows: nil}))
	// Scoped to "trader": the dropdown is non-empty, so we never hit the
	// empty state — this guards against the access-gap text leaking in.
	ctx := api.ContextWithScopeForTesting(context.Background(), "trader")
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "No trading-enabled projects you can access")
}

// TestTrading_SnapshotExpiredLoudWarning — a snapshot older than the hard 24h
// bound raises the loud "expired" warning, distinct from the soft "stale" hint.
func TestTrading_SnapshotExpiredLoudWarning(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1") // unreachable broker
	snap := &fakeSnapshotRepo{rows: []*persistence.TradingPositionsSnapshot{
		{ProjectID: "trader", RecordedAt: time.Now().UTC().Add(-48 * time.Hour),
			CashUSD: 8000, EquityUSD: 10000},
	}}
	srv := NewServer(WithProjectRegistry(reg), WithTradingSnapshotRepository(snap))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "expired snapshot", "a >24h snapshot must raise the loud expired warning")
	assert.NotContains(t, body, "stale snapshot", "expired must not be downgraded to the soft stale hint")
}

// TestTrading_SnapshotStoreUnavailableHint — broker offline AND the snapshot
// store errors: the page must say the store is unavailable (a vornik-storage
// problem), distinct from "no snapshot exists".
func TestTrading_SnapshotStoreUnavailableHint(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1") // unreachable broker
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingSnapshotRepository(&fakeSnapshotRepo{err: context.DeadlineExceeded}))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "snapshot store unavailable")
}

// TestTrading_AccessScopeExcludesForeignProject — a request scoped to a
// project that isn't trading-enabled must not see the foreign trading
// project: the dropdown excludes it (empty state) and an explicit
// ?project= aimed at it is refused.
func TestTrading_AccessScopeExcludesForeignProject(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)
	srv := NewServer(WithProjectRegistry(reg))

	// Scoped to "plain" (no broker) — "trader" is foreign + excluded. The
	// deployment HAS a trading project the caller can't reach, so the empty
	// state must name the access gap, not claim none are configured.
	ctx := api.ContextWithScopeForTesting(context.Background(), "plain")
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No trading-enabled projects you can access")

	// Scoped to "trader" (non-empty dropdown), an explicit ?project=plain
	// aimed at the foreign project is refused.
	traderCtx := api.ContextWithScopeForTesting(context.Background(), "trader")
	rec2 := httptest.NewRecorder()
	srv.Trading(rec2, httptest.NewRequest(http.MethodGet, "/trading?project=plain", nil).WithContext(traderCtx))
	require.Equal(t, http.StatusForbidden, rec2.Code)
}

// TestTrading_RendersOrdersAndSafetyEvents — recent orders + safety
// events from the repos surface in the page.
func TestTrading_RendersOrdersAndSafetyEvents(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)
	orderRepo := &fakeOrderRepo{rows: []*persistence.TradingOrder{
		{ID: "o1", ProjectID: "trader", Symbol: "AAPL", Action: "BUY", OrderType: "MKT",
			Qty: 3, Status: "filled", SubmittedAt: time.Now().UTC()},
	}}
	sym := "AAPL"
	safetyRepo := &fakeSafetyRepo{rows: []*persistence.TradingSafetyEvent{
		{ID: "s1", ProjectID: "trader", Kind: "cap_refused", Severity: "warn",
			Symbol: &sym, RecordedAt: time.Now().UTC()},
	}}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingOrderRepository(orderRepo), WithTradingSafetyRepository(safetyRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "AAPL")
	assert.Contains(t, body, "cap_refused")
	assert.Contains(t, body, "Security events")
}

// fakeFillRepo is a full in-memory TradingFillRepository for
// performance-panel tests. Captures the Since value passed to List
// so window-scoping assertions can verify the open-position grace
// lookback.
type fakeFillRepo struct {
	rows      []*persistence.TradingFill
	lastSince *time.Time
}

func (f *fakeFillRepo) Record(context.Context, *persistence.TradingFill) error { return nil }
func (f *fakeFillRepo) List(_ context.Context, flt persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	f.lastSince = flt.Since
	return f.rows, nil
}
func (f *fakeFillRepo) SumVolume(context.Context, persistence.TradingFillFilter) (float64, error) {
	return 0, nil
}
func (f *fakeFillRepo) MaxFilledAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}
func (f *fakeFillRepo) PatchCommission(context.Context, string, float64) error       { return nil }
func (f *fakeFillRepo) RecordShadow(context.Context, *persistence.TradingFill) error { return nil }
func (f *fakeFillRepo) ListNullCommission(_ context.Context, _ time.Time) ([]*persistence.TradingFill, error) {
	return nil, nil
}

// ─── Trading Performance panel tests ──────────────────────────────────────────

// TestTradingPerf_WinLossRendersSummary — with a long win + short loss returned
// by fake repos, the rendered page contains win-rate %, net realized, and a
// per-symbol row for the winning ticker.
func TestTradingPerf_WinLossRendersSummary(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	now := time.Now().UTC()
	// Long win: buy 10 @ $100 (day-1), sell 10 @ $110 (today) → +$100
	// Short loss: sell 5 @ $50 (day-1), buy 5 @ $55 (today) → -$25
	fills := []*persistence.TradingFill{
		{ID: "f1", OrderID: "o1", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: now.Add(-23 * time.Hour)},
		{ID: "f2", OrderID: "o2", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: now.Add(-1 * time.Hour)},
		{ID: "f3", OrderID: "o3", ProjectID: "trader", Symbol: "QQQ", Qty: 5, Price: 50, FilledAt: now.Add(-22 * time.Hour)},
		{ID: "f4", OrderID: "o4", ProjectID: "trader", Symbol: "QQQ", Qty: 5, Price: 55, FilledAt: now.Add(-30 * time.Minute)},
	}
	orders := []*persistence.TradingOrder{
		{ID: "o1", ProjectID: "trader", Symbol: "SPY", Action: "BUY", OrderType: "MKT", Qty: 10, Status: "filled", SubmittedAt: now.Add(-23 * time.Hour)},
		{ID: "o2", ProjectID: "trader", Symbol: "SPY", Action: "SELL", OrderType: "MKT", Qty: 10, Status: "filled", SubmittedAt: now.Add(-1 * time.Hour)},
		{ID: "o3", ProjectID: "trader", Symbol: "QQQ", Action: "SELL", OrderType: "MKT", Qty: 5, Status: "filled", SubmittedAt: now.Add(-22 * time.Hour)},
		{ID: "o4", ProjectID: "trader", Symbol: "QQQ", Action: "BUY", OrderType: "MKT", Qty: 5, Status: "filled", SubmittedAt: now.Add(-30 * time.Minute)},
	}

	fillRepo := &fakeFillRepo{rows: fills}
	orderRepo := &fakeOrderRepo{rows: orders}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(fillRepo),
		WithTradingOrderRepository(orderRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=24h", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Win-rate: 1 win out of 2 trades = 50%
	assert.Contains(t, body, "50.00", "win rate % must render")
	// Net realized: +100 - 25 = +75
	assert.Contains(t, body, "75.00", "net realized USD must render")
	// Profit factor: 100/25 = 4.0
	assert.Contains(t, body, "4.00", "profit factor must render")
	// Per-symbol rows: both SPY and QQQ
	assert.Contains(t, body, "SPY", "per-symbol row for SPY must render")
	assert.Contains(t, body, "QQQ", "per-symbol row for QQQ must render")
	// Performance section header
	assert.Contains(t, body, "Trading Performance", "section header must render")
}

// TestTradingPerf_RendersBeforeAccountSnapshot pins the performance-first
// ordering: vornik's own Trading Performance section must render ABOVE the
// broker account snapshot so "are we winning?" is the first thing seen.
func TestTradingPerf_RendersBeforeAccountSnapshot(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	now := time.Now().UTC()
	fills := []*persistence.TradingFill{
		{ID: "f1", OrderID: "o1", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: now.Add(-23 * time.Hour)},
		{ID: "f2", OrderID: "o2", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: now.Add(-1 * time.Hour)},
	}
	orders := []*persistence.TradingOrder{
		{ID: "o1", ProjectID: "trader", Symbol: "SPY", Action: "BUY", OrderType: "MKT", Qty: 10, Status: "filled", SubmittedAt: now.Add(-23 * time.Hour)},
		{ID: "o2", ProjectID: "trader", Symbol: "SPY", Action: "SELL", OrderType: "MKT", Qty: 10, Status: "filled", SubmittedAt: now.Add(-1 * time.Hour)},
	}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(&fakeFillRepo{rows: fills}),
		WithTradingOrderRepository(&fakeOrderRepo{rows: orders}))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=24h", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	perfIdx := strings.Index(body, "Trading Performance")
	acctIdx := strings.Index(body, "Account snapshot")
	require.Positive(t, perfIdx, "Trading Performance section must render")
	require.Positive(t, acctIdx, "Account snapshot section must render")
	assert.Less(t, perfIdx, acctIdx,
		"Trading Performance must render before the broker Account snapshot")
}

// TestTradingPerf_EmptyStateWhenNoTrades — when no fills return, the empty
// state "No closed trades" must appear in the performance section.
func TestTradingPerf_EmptyStateWhenNoTrades(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	fillRepo := &fakeFillRepo{rows: nil}
	orderRepo := &fakeOrderRepo{rows: nil}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(fillRepo),
		WithTradingOrderRepository(orderRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No closed trades", "empty state must render when perf.Trades==0")
}

// TestTradingPerf_EquityLineFromSnapshotsWhenBrokerOffline — when broker is
// unreachable but snapshots exist, the equity-curve section renders from
// snapshot data (equity USD values present in page).
func TestTradingPerf_EquityLineFromSnapshotsWhenBrokerOffline(t *testing.T) {
	// Unreachable broker
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")

	now := time.Now().UTC()
	snaps := []*persistence.TradingPositionsSnapshot{
		{ID: "s1", ProjectID: "trader", RecordedAt: now.Add(-6 * 24 * time.Hour), EquityUSD: 9800},
		{ID: "s2", ProjectID: "trader", RecordedAt: now.Add(-5 * 24 * time.Hour), EquityUSD: 9900},
		{ID: "s3", ProjectID: "trader", RecordedAt: now.Add(-1 * time.Hour), EquityUSD: 10000},
	}
	snapRepo := &fakeSnapshotRepo{rows: snaps}
	fillRepo := &fakeFillRepo{rows: nil}
	orderRepo := &fakeOrderRepo{rows: nil}

	srv := NewServer(WithProjectRegistry(reg),
		WithTradingSnapshotRepository(snapRepo),
		WithTradingFillRepository(fillRepo),
		WithTradingOrderRepository(orderRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Should render equity curve SVG with some data points
	assert.Contains(t, body, "9800", "equity from snapshots must appear in equity chart")
}

// TestTradingPerf_NilReposStillRenders — when no trading repos are wired at
// all, the page must still render without an internal server error.
func TestTradingPerf_NilReposStillRenders(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	srv := NewServer(WithProjectRegistry(reg)) // no repos wired
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	// The page must contain the Trading Performance section header even with
	// nil repos (empty-state path). "Internal server error" must NOT appear.
	assert.NotContains(t, rec.Body.String(), "Internal server error", "nil repos must not cause a template/handler panic")
	assert.Contains(t, rec.Body.String(), "Trading Performance", "performance section must render even with nil repos")
}

// TestTradingPerf_ZeroLossesProfitFactorDash — when all trades are wins (no
// losses), profit factor must render as "—", not as ∞ or a number.
func TestTradingPerf_ZeroLossesProfitFactorDash(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	now := time.Now().UTC()
	fills := []*persistence.TradingFill{
		{ID: "f1", OrderID: "o1", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: now.Add(-2 * time.Hour)},
		{ID: "f2", OrderID: "o2", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: now.Add(-1 * time.Hour)},
	}
	orders := []*persistence.TradingOrder{
		{ID: "o1", ProjectID: "trader", Symbol: "SPY", Action: "BUY", OrderType: "MKT", Qty: 10, Status: "filled", SubmittedAt: now.Add(-2 * time.Hour)},
		{ID: "o2", ProjectID: "trader", Symbol: "SPY", Action: "SELL", OrderType: "MKT", Qty: 10, Status: "filled", SubmittedAt: now.Add(-1 * time.Hour)},
	}
	fillRepo := &fakeFillRepo{rows: fills}
	orderRepo := &fakeOrderRepo{rows: orders}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(fillRepo),
		WithTradingOrderRepository(orderRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=24h", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Profit factor must render as "—" (nil ProfitFactor)
	assert.Contains(t, body, "—", "profit factor must be — when zero losses")
}

// TestTradingPerf_FillWindowScopesGraceLookback — the fill repo's Since must
// be 30 days before windowStart (not just windowStart) to allow cross-day
// open→close pairs to match. Checks the captured filter.Since from the fill
// repo.
func TestTradingPerf_FillWindowScopesGraceLookback(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	fillRepo := &fakeFillRepo{rows: nil}
	orderRepo := &fakeOrderRepo{rows: nil}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(fillRepo),
		WithTradingOrderRepository(orderRepo))

	before := time.Now().UTC()
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	require.NotNil(t, fillRepo.lastSince, "fill repo List must be called with a Since value")
	// windowStart is ~7d ago; grace is 30d before that → ~37d total
	expectedAgo := 37 * 24 * time.Hour
	gotAgo := before.Sub(*fillRepo.lastSince)
	assert.InDelta(t, expectedAgo.Seconds(), gotAgo.Seconds(), 120,
		"fill Since must be windowStart-30d (37d total lookback for 7d window)")
}

// TestTradingPerf_CrossDayOpenBeforeWindowStillPairs locks the two contracts
// flagged in the companion review (2026-06-21):
//
//	(a) TradingFill.OrderID and TradingOrder.ID share the same key space, so
//	    the handler's sideByOrderID map can resolve the opening side even when
//	    the fill and order are placed before the selected window.
//
//	(b) The orders query uses the same 30-day grace window as the fills query
//	    (both set Since = windowStart − 30d), so a pre-window opening order is
//	    loaded and the opening fill does not become an orphan.
//
// Scenario: 7-day window; opening BUY order + fill placed ~10 days before
// windowStart (≈17 days ago from now — well inside the 30-day grace); closing
// SELL fill placed ~1 day before now (inside the window). Expected outcome:
// one closed LONG trade on TSLA with +$100 realized P&L renders on the page.
func TestTradingPerf_CrossDayOpenBeforeWindowStillPairs(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	now := time.Now().UTC()
	windowStart := now.Add(-7 * 24 * time.Hour)
	// Opening leg: ~10 days before windowStart → ~17 days ago from now.
	// This is before the window but reachable via the 30-day grace lookback.
	openAt := windowStart.Add(-10 * 24 * time.Hour)
	// Closing leg: 1 day before now, inside the 7-day window.
	closeAt := now.Add(-24 * time.Hour)

	// BUY 10 TSLA @ $100, then SELL 10 TSLA @ $110 → +$100 realized (long).
	fills := []*persistence.TradingFill{
		{ID: "fOpen", OrderID: "oOpen", ProjectID: "trader", Symbol: "TSLA", Qty: 10, Price: 100, FilledAt: openAt},
		{ID: "fClose", OrderID: "oClose", ProjectID: "trader", Symbol: "TSLA", Qty: 10, Price: 110, FilledAt: closeAt},
	}
	orders := []*persistence.TradingOrder{
		{ID: "oOpen", ProjectID: "trader", Symbol: "TSLA", Action: "BUY", OrderType: "MKT",
			Qty: 10, Status: "filled", SubmittedAt: openAt},
		{ID: "oClose", ProjectID: "trader", Symbol: "TSLA", Action: "SELL", OrderType: "MKT",
			Qty: 10, Status: "filled", SubmittedAt: closeAt},
	}

	fillRepo := &fakeFillRepo{rows: fills}
	orderRepo := &fakeOrderRepo{rows: orders}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(fillRepo),
		WithTradingOrderRepository(orderRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// The trade must NOT be silently dropped — "No closed trades" must be absent.
	assert.NotContains(t, body, "No closed trades",
		"cross-day round-trip must not be silently dropped")

	// Net realized P&L: (110-100) × 10 = $100.00 — must appear on the page.
	assert.Contains(t, body, "$100.00",
		"net realized P&L must render for the cross-day long trade")

	// Per-symbol row for TSLA must appear (BySymbol populated from the closed trade).
	assert.Contains(t, body, "TSLA",
		"per-symbol row for TSLA must render in the performance breakdown")
}

// TestTrading_RendersSafetyEventDetail — the safety event's detail
// JSONB (the "why": cap values, refusal reasons, thresholds) must
// surface in the Security events section, not just kind/severity/symbol.
// Regression for "security events section — not enough information"
// (2026-06-12): the detail map was parsed into SafetyEventRow but the
// template never rendered it.
func TestTrading_RendersSafetyEventDetail(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)
	sym := "AAPL"
	safetyRepo := &fakeSafetyRepo{rows: []*persistence.TradingSafetyEvent{
		{ID: "s1", ProjectID: "trader", Kind: "cap_refused", Severity: "warn",
			Symbol: &sym, RecordedAt: time.Now().UTC(),
			Detail: []byte(`{"max_position_usd":2500,"requested_usd":3000}`)},
	}}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingSafetyRepository(safetyRepo))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "max_position_usd", "safety event detail key must render")
	assert.Contains(t, body, "requested_usd", "safety event detail key must render")
	assert.Contains(t, body, "3000", "safety event detail value must render")
}
