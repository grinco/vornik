package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// orderListStub returns a fixed slice of TradingOrder rows. Count
// returns 0 — the soak-volume path is exercised elsewhere; we only
// care about the history-flatten branch here.
type orderListStub struct {
	rows []*persistence.TradingOrder
}

func (s *orderListStub) Record(_ context.Context, _ *persistence.TradingOrder) error { return nil }
func (s *orderListStub) Count(_ context.Context, _ persistence.TradingOrderFilter) (int64, error) {
	return 0, nil
}
func (s *orderListStub) List(_ context.Context, _ persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	return s.rows, nil
}

// fillListStub returns a fixed slice of TradingFill rows. SumVolume
// returns 0 because the soak-volume tile isn't under test.
type fillListStub struct {
	rows []*persistence.TradingFill
}

func (s *fillListStub) Record(_ context.Context, _ *persistence.TradingFill) error { return nil }
func (s *fillListStub) SumVolume(_ context.Context, _ persistence.TradingFillFilter) (float64, error) {
	return 0, nil
}
func (s *fillListStub) List(_ context.Context, _ persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	return s.rows, nil
}
func (s *fillListStub) MaxFilledAt(_ context.Context, _ string) (time.Time, error) {
	return time.Time{}, nil
}
func (s *fillListStub) PatchCommission(_ context.Context, _ string, _ float64) error     { return nil }
func (s *fillListStub) RecordShadow(_ context.Context, _ *persistence.TradingFill) error { return nil }
func (s *fillListStub) ListNullCommission(_ context.Context, _ time.Time) ([]*persistence.TradingFill, error) {
	return nil, nil
}

// brokerStub spins up the minimum /caps response so buildTradingPanel
// considers the project Enabled and reaches the history-population
// branches. Returned value is the test server URL; caller is
// responsible for Close via t.Cleanup.
func brokerStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{"mode": "paper"},
			"state":      map[string]any{"kill_switch": false},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestBuildTradingPanel_PopulatesRecentOrders pins the inline-table
// projection: every order row the repo returns becomes a
// TradingOrderRow with pointer fields collapsed and CSS classes
// resolved. The list is capped at recentOrdersLimit upstream by the
// PageSize argument — we don't re-test pagination here.
func TestBuildTradingPanel_PopulatesRecentOrders(t *testing.T) {
	limit := 145.32
	stop := 138.50
	brokerOID := "86287740"
	terminal := time.Now().UTC()
	orders := []*persistence.TradingOrder{
		{
			ID:            "ord-1",
			ProjectID:     "p1",
			Symbol:        "TSLA",
			Action:        "BUY",
			OrderType:     "LMT",
			Qty:           4,
			LimitPrice:    &limit,
			TimeInForce:   "DAY",
			Status:        "submitted",
			BrokerOrderID: &brokerOID,
			SubmittedAt:   time.Now().UTC().Add(-30 * time.Minute),
		},
		{
			ID:               "ord-2",
			ProjectID:        "p1",
			Symbol:           "NVDA",
			Action:           "SELL",
			OrderType:        "STP",
			Qty:              2,
			StopPrice:        &stop,
			TimeInForce:      "GTC",
			Status:           "filled",
			SubmittedAt:      time.Now().UTC().Add(-2 * time.Hour),
			TerminalAt:       &terminal,
			LastStatusReason: "filled at market",
		},
		nil, // ensure nil rows are skipped
	}
	s := &Server{
		logger:           zerolog.Nop(),
		tradingOrderRepo: &orderListStub{rows: orders},
	}
	project := &registry.Project{
		ID: "p1",
		MCP: registry.ProjectMCP{
			Servers: []registry.MCPServerConfig{{Name: "broker", URL: brokerStub(t)}},
		},
	}

	panel := s.buildTradingPanel(context.Background(), project)
	require.True(t, panel.Enabled)
	require.Len(t, panel.RecentOrders, 2, "nil rows are dropped")

	// First row — LMT BUY, still open. ActionClass=emerald, StatusClass=neutral.
	r := panel.RecentOrders[0]
	assert.Equal(t, "ord-1", r.ID)
	assert.Equal(t, "TSLA", r.Symbol)
	assert.Equal(t, "BUY", r.Action)
	assert.Equal(t, "LMT", r.OrderType)
	assert.InDelta(t, 145.32, r.LimitPrice, 1e-9)
	assert.InDelta(t, 0.0, r.StopPrice, 1e-9, "unset stop collapses to 0")
	assert.Equal(t, "86287740", r.BrokerOrderID)
	assert.Equal(t, "outcome-neutral", r.StatusClass)
	assert.Equal(t, "text-emerald-400", r.ActionClass)
	assert.True(t, r.TerminalAt.IsZero(), "open order has zero TerminalAt")

	// Second row — STP SELL, filled. ActionClass=rose, StatusClass=good.
	r = panel.RecentOrders[1]
	assert.Equal(t, "STP", r.OrderType)
	assert.InDelta(t, 138.50, r.StopPrice, 1e-9)
	assert.Equal(t, "outcome-good", r.StatusClass)
	assert.Equal(t, "text-rose-400", r.ActionClass)
	assert.False(t, r.TerminalAt.IsZero(), "terminal order carries TerminalAt")
	assert.Equal(t, "filled at market", r.StatusReason)
}

// TestBuildTradingPanel_PopulatesRecentFills pins the fill projection:
// notional is precomputed (qty × price) and the commission pointer
// collapses to 0 when unset.
func TestBuildTradingPanel_PopulatesRecentFills(t *testing.T) {
	commission := 1.25
	fills := []*persistence.TradingFill{
		{
			ID:            "fill-1",
			OrderID:       "ord-1",
			ProjectID:     "p1",
			Symbol:        "TSLA",
			Qty:           4,
			Price:         145.0,
			CommissionUSD: &commission,
			FilledAt:      time.Now().UTC().Add(-10 * time.Minute),
		},
		{
			ID:        "fill-2",
			OrderID:   "ord-2",
			ProjectID: "p1",
			Symbol:    "NVDA",
			Qty:       2,
			Price:     500.5,
			FilledAt:  time.Now().UTC().Add(-1 * time.Hour),
		},
		nil,
	}
	s := &Server{
		logger:           zerolog.Nop(),
		tradingOrderRepo: &orderListStub{}, // empty so we don't accidentally fall through
		tradingFillRepo:  &fillListStub{rows: fills},
	}
	project := &registry.Project{
		ID: "p1",
		MCP: registry.ProjectMCP{
			Servers: []registry.MCPServerConfig{{Name: "broker", URL: brokerStub(t)}},
		},
	}

	panel := s.buildTradingPanel(context.Background(), project)
	require.True(t, panel.Enabled)
	require.Len(t, panel.RecentFills, 2)

	r := panel.RecentFills[0]
	assert.Equal(t, "TSLA", r.Symbol)
	assert.InDelta(t, 4.0, r.Qty, 1e-9)
	assert.InDelta(t, 145.0, r.Price, 1e-9)
	assert.InDelta(t, 580.0, r.Notional, 1e-9, "notional must equal Qty × Price")
	assert.InDelta(t, 1.25, r.CommissionUSD, 1e-9)

	r = panel.RecentFills[1]
	assert.InDelta(t, 1001.0, r.Notional, 1e-9)
	assert.InDelta(t, 0.0, r.CommissionUSD, 1e-9, "unset commission collapses to 0")
}

// TestBuildTradingPanel_EmptyHistoryWhenReposNil — no order/fill
// repo wired ⇒ panel still renders, history slices are nil. The
// template's `{{if .Trading.RecentOrders}}` gate hides the section
// cleanly in this case.
func TestBuildTradingPanel_EmptyHistoryWhenReposNil(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "p1",
		MCP: registry.ProjectMCP{
			Servers: []registry.MCPServerConfig{{Name: "broker", URL: brokerStub(t)}},
		},
	}
	panel := s.buildTradingPanel(context.Background(), project)
	require.True(t, panel.Enabled)
	assert.Nil(t, panel.RecentOrders)
	assert.Nil(t, panel.RecentFills)
}

// TestOrderStatusClass walks every status enum branch + the default.
// Mirrors severityClass's test (if there were one): the visual
// vocabulary across the trading panel needs to stay consistent.
func TestOrderStatusClass(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"filled", "outcome-good"},
		{"submitted", "outcome-neutral"},
		{"partial", "outcome-neutral"},
		{"cancelled", "outcome-bad"},
		{"refused", "outcome-bad"},
		{"rejected", "outcome-bad"},
		{"pending_routing", "outcome-warn"}, // unknown → warn (not silent success)
		{"", "outcome-warn"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.want, orderStatusClass(c.in))
		})
	}
}

// TestOrderActionClass — direction-pill colours. Case + whitespace
// tolerant (broker upstream sometimes sends "buy" / " BUY" / etc.).
func TestOrderActionClass(t *testing.T) {
	assert.Equal(t, "text-emerald-400", orderActionClass("BUY"))
	assert.Equal(t, "text-emerald-400", orderActionClass(" buy "), "case + whitespace tolerant")
	assert.Equal(t, "text-rose-400", orderActionClass("SELL"))
	assert.Equal(t, "text-rose-400", orderActionClass("sell"))
	assert.Equal(t, "text-gray-300", orderActionClass(""), "empty falls to neutral")
	assert.Equal(t, "text-gray-300", orderActionClass("UNKNOWN_SIDE"))
}

// TestFlattenOrderRow_NilPointerCollapse pins the contract that
// optional pointer fields on TradingOrder (BrokerOrderID, LimitPrice,
// StopPrice, TerminalAt) all collapse to zero values on the row so
// the template never has to nil-check.
func TestFlattenOrderRow_NilPointerCollapse(t *testing.T) {
	o := &persistence.TradingOrder{
		ID:        "ord-x",
		Symbol:    "AAPL",
		Action:    "BUY",
		OrderType: "MKT",
		Qty:       1,
		Status:    "submitted",
	}
	row := flattenOrderRow(o)
	assert.Empty(t, row.BrokerOrderID)
	assert.Zero(t, row.LimitPrice)
	assert.Zero(t, row.StopPrice)
	assert.True(t, row.TerminalAt.IsZero())
}
