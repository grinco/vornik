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

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// perfFillRepo / perfOrderRepo return scripted rows so buildTradingPanel can
// compute round-trip Performance without a DB.
type perfFillRepo struct{ fills []*persistence.TradingFill }

func (r *perfFillRepo) Record(context.Context, *persistence.TradingFill) error { return nil }
func (r *perfFillRepo) List(context.Context, persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	return r.fills, nil
}
func (r *perfFillRepo) SumVolume(context.Context, persistence.TradingFillFilter) (float64, error) {
	return 0, nil
}
func (r *perfFillRepo) MaxFilledAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}
func (r *perfFillRepo) PatchCommission(context.Context, string, float64) error       { return nil }
func (r *perfFillRepo) RecordShadow(context.Context, *persistence.TradingFill) error { return nil }
func (r *perfFillRepo) ListNullCommission(_ context.Context, _ time.Time) ([]*persistence.TradingFill, error) {
	return nil, nil
}

type perfOrderRepo struct{ orders []*persistence.TradingOrder }

func (r *perfOrderRepo) Record(context.Context, *persistence.TradingOrder) error { return nil }
func (r *perfOrderRepo) List(context.Context, persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	return r.orders, nil
}
func (r *perfOrderRepo) Count(context.Context, persistence.TradingOrderFilter) (int64, error) {
	return 0, nil
}

func perfBrokerStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{"mode": "paper"},
			"state":      map[string]any{"kill_switch": false},
		})
	}))
}

func perfProject(url string) *registry.Project {
	return &registry.Project{
		ID:  "p1",
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "broker", URL: url}}},
	}
}

// A BUY@100 then SELL@110 round-trip (10 shares) → +$100 realized, one win.
func TestBuildTradingPanel_AttachesRoundTripPerformance(t *testing.T) {
	broker := perfBrokerStub(t)
	defer broker.Close()
	now := time.Now().UTC()

	s := &Server{
		logger: zerolog.Nop(),
		tradingFillRepo: &perfFillRepo{fills: []*persistence.TradingFill{
			{ID: "f1", OrderID: "o1", ProjectID: "p1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: now.AddDate(0, 0, -3)},
			{ID: "f2", OrderID: "o2", ProjectID: "p1", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: now.AddDate(0, 0, -1)},
		}},
		tradingOrderRepo: &perfOrderRepo{orders: []*persistence.TradingOrder{
			{ID: "o1", Action: "BUY"},
			{ID: "o2", Action: "SELL"},
		}},
	}

	panel := s.buildTradingPanel(context.Background(), perfProject(broker.URL))

	assert.True(t, panel.Enabled)
	assert.Equal(t, 30, panel.PerfWindowDays, "project-detail panel computes a 30d window")
	assert.Equal(t, 1, panel.Perf.Trades)
	assert.Equal(t, 1, panel.Perf.Wins)
	assert.InDelta(t, 100.0, panel.Perf.NetRealizedUSD, 0.001)
	// No losing trades → profit factor is undefined, rendered as an em dash.
	assert.Equal(t, "—", panel.PerfProfitFactor)
}

// With no trading repos wired the panel still reports the window and a clean
// zero-trade Performance (no panic, no spurious numbers).
func TestBuildTradingPanel_PerformanceZeroWhenNoFills(t *testing.T) {
	broker := perfBrokerStub(t)
	defer broker.Close()

	s := &Server{logger: zerolog.Nop()}
	panel := s.buildTradingPanel(context.Background(), perfProject(broker.URL))

	assert.True(t, panel.Enabled)
	assert.Equal(t, 30, panel.PerfWindowDays)
	assert.Equal(t, 0, panel.Perf.Trades)
	assert.Equal(t, "—", panel.PerfProfitFactor)
}
