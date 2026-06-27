package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// TestBuildTradingPanel_DisabledWithoutBrokerServer — projects
// without a `broker` MCP server skip the panel entirely. Most
// projects (snake, janka, assistant) fall in this bucket.
func TestBuildTradingPanel_DisabledWithoutBrokerServer(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "non-trading",
		MCP: registry.ProjectMCP{
			Servers: []registry.MCPServerConfig{
				{Name: "ta", URL: "http://example.com"},
			},
		},
	}
	panel := s.buildTradingPanel(context.Background(), project)
	assert.False(t, panel.Enabled, "panel must skip when no broker server is configured")
}

// TestBuildTradingPanel_PopulatesFromBroker — happy path: the
// daemon hits the broker /caps endpoint, parses the response,
// and surfaces every field the template needs. Uses an httptest
// stand-in for the broker MCP.
func TestBuildTradingPanel_PopulatesFromBroker(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/caps", r.URL.Path)
		require.Equal(t, "p1", r.URL.Query().Get("project"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{
				"mode":                         "paper",
				"max_position_usd":             1000.0,
				"max_daily_turnover_usd":       5000.0,
				"max_orders_per_hour":          15,
				"max_orders_per_minute":        4,
				"drawdown_circuit_breaker_pct": 5.0,
				"require_stop_loss":            true,
				"default_stop_loss_pct":        8.0,
			},
			"state": map[string]any{
				"kill_switch":           false,
				"hwm_usd":               1_044_839.41,
				"today_utc":             "2026-05-03",
				"today_turnover_usd":    300.0,
				"orders_in_last_hour":   3,
				"orders_in_last_minute": 1,
				"project_id":            "p1",
			},
			"portfolio": map[string]any{
				"account":             "DUH-TEST",
				"cash_usd":            1_042_139.76,
				"equity_usd":          1_044_839.41,
				"buying_power_usd":    6_965_596.07,
				"unrealised_pl_usd":   0,
				"realised_pl_day_usd": 0,
				"open_positions":      0,
				"drawdown_pct":        0.0,
			},
		})
	}))
	defer broker.Close()

	s := &Server{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:  "p1",
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "broker", URL: broker.URL}}},
	}
	panel := s.buildTradingPanel(context.Background(), project)

	assert.True(t, panel.Enabled)
	assert.True(t, panel.BrokerReachable)
	assert.Equal(t, "paper", panel.Mode)
	assert.Equal(t, 1000.0, panel.MaxPositionUSD)
	assert.Equal(t, 5000.0, panel.MaxDailyTurnoverUSD)
	assert.Equal(t, 15, panel.MaxOrdersPerHour)
	assert.Equal(t, 4, panel.MaxOrdersPerMinute)
	assert.Equal(t, 5.0, panel.DrawdownCircuitBreakerPct)
	assert.True(t, panel.RequireStopLoss)
	assert.Equal(t, 8.0, panel.DefaultStopLossPct)

	assert.Equal(t, 1_044_839.41, panel.HWMUSD)
	assert.Equal(t, "2026-05-03", panel.TodayUTC)
	assert.Equal(t, 300.0, panel.TodayTurnoverUSD)
	assert.Equal(t, 3, panel.OrdersInLastHour)

	assert.True(t, panel.PortfolioReachable)
	assert.Equal(t, "DUH-TEST", panel.Account)
	assert.Equal(t, 1_044_839.41, panel.EquityUSD)
}

// TestBuildTradingPanel_BrokerUnreachable — Enabled stays true
// (the project IS configured for trading) but BrokerReachable
// is false and an error string is captured for the panel header.
func TestBuildTradingPanel_BrokerUnreachable(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	project := &registry.Project{
		ID: "p1",
		// Port 1 is reserved + nobody listens — fast dial failure.
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "broker", URL: "http://127.0.0.1:1"}}},
	}
	panel := s.buildTradingPanel(context.Background(), project)
	assert.True(t, panel.Enabled, "trading IS configured")
	assert.False(t, panel.BrokerReachable, "but the broker can't be reached")
	assert.NotEmpty(t, panel.BrokerError, "an error string must be captured for the operator")
}

// TestBuildTradingPanel_SendsProjectCapsHeader — the panel must
// attach X-Project-Caps when fetching /caps so the broker echoes
// the project YAML's caps in the `configured` block (rather than
// the broker process's env-var defaults). Without this header the
// UI would silently render permissive defaults even when the
// project YAML has tight caps configured.
func TestBuildTradingPanel_SendsProjectCapsHeader(t *testing.T) {
	var seenHeader string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Project-Caps")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{"mode": "paper"},
			"state":      map[string]any{},
			"portfolio":  nil,
		})
	}))
	defer broker.Close()

	s := &Server{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:  "ibkr-trader",
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "broker", URL: broker.URL}}},
		Trading: registry.ProjectTrading{
			Mode:       "paper",
			KillSwitch: false,
			Caps: registry.TradingCaps{
				MaxPositionUSD:            1000,
				MaxDailyTurnoverUSD:       5000,
				MaxOrdersPerHour:          15,
				MaxOrdersPerMinute:        4,
				DrawdownCircuitBreakerPct: 5,
			},
		},
	}
	_ = s.buildTradingPanel(context.Background(), project)

	require.NotEmpty(t, seenHeader, "trading panel must attach X-Project-Caps so /caps surfaces project YAML caps, not broker env defaults")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(seenHeader), &decoded))
	assert.Equal(t, 1000.0, decoded["max_position_usd"])
	assert.Equal(t, 5000.0, decoded["max_daily_turnover_usd"])
	assert.Equal(t, 15.0, decoded["max_orders_per_hour"])
	assert.Equal(t, 4.0, decoded["max_orders_per_minute"])
	assert.Equal(t, 5.0, decoded["drawdown_circuit_breaker_pct"])
}

// TestBuildTradingPanel_PortfolioOnlyNullDoesntBreakPanel — the
// caps + state still populate when the portfolio block is null
// (sidecar offline but broker MCP itself responding).
func TestBuildTradingPanel_PortfolioOnlyNullDoesntBreakPanel(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{"mode": "paper"},
			"state":      map[string]any{"kill_switch": false},
			"portfolio":  nil,
		})
	}))
	defer broker.Close()
	s := &Server{logger: zerolog.Nop()}
	project := &registry.Project{
		ID:  "p1",
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "broker", URL: broker.URL}}},
	}
	panel := s.buildTradingPanel(context.Background(), project)
	assert.True(t, panel.Enabled)
	assert.True(t, panel.BrokerReachable)
	assert.False(t, panel.PortfolioReachable, "portfolio block was null")
	assert.Equal(t, "paper", panel.Mode)
}
