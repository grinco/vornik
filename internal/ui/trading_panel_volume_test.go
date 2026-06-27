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

// stubFillRepo is a narrow in-memory implementation of
// persistence.TradingFillRepository sufficient to exercise the
// soak-panel volume switch. Record/List aren't called by the
// volume path; SumVolume is the surface under test.
type stubFillRepo struct {
	sumByWindow map[string]float64
}

func (s *stubFillRepo) Record(ctx context.Context, fill *persistence.TradingFill) error {
	return nil
}

func (s *stubFillRepo) List(ctx context.Context, filter persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	return nil, nil
}

func (s *stubFillRepo) SumVolume(ctx context.Context, filter persistence.TradingFillFilter) (float64, error) {
	if filter.Since == nil || filter.ProjectID == nil {
		return 0, nil
	}
	now := time.Now().UTC()
	if filter.Since.After(now.Add(-24 * time.Hour)) {
		return s.sumByWindow["today"], nil
	}
	return s.sumByWindow["7d"], nil
}

func (s *stubFillRepo) MaxFilledAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}
func (s *stubFillRepo) PatchCommission(context.Context, string, float64) error       { return nil }
func (s *stubFillRepo) RecordShadow(context.Context, *persistence.TradingFill) error { return nil }
func (s *stubFillRepo) ListNullCommission(_ context.Context, _ time.Time) ([]*persistence.TradingFill, error) {
	return nil, nil
}

// stubOrderRepo gives buildTradingPanel an order count source so
// the panel reaches the volume code path. Not the focus of this
// test — just enough to keep buildTradingPanel happy.
type stubOrderRepo struct{}

func (s *stubOrderRepo) Record(ctx context.Context, order *persistence.TradingOrder) error {
	return nil
}
func (s *stubOrderRepo) List(ctx context.Context, filter persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	return nil, nil
}
func (s *stubOrderRepo) Count(ctx context.Context, filter persistence.TradingOrderFilter) (int64, error) {
	return 0, nil
}

// TestBuildTradingPanel_VolumeFromFillsRepoWhenWired — when the
// fill repo is wired, the soak panel's volume tile reflects
// SumVolume from trading_fills (the precise post-fill source)
// rather than the trading_orders LMT-price estimate (which over-
// counted because orders inflate via limit_price the moment
// they're submitted, before fills confirm).
func TestBuildTradingPanel_VolumeFromFillsRepoWhenWired(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{"mode": "paper"},
			"state":      map[string]any{"kill_switch": false},
		})
	}))
	defer broker.Close()

	s := &Server{
		logger:           zerolog.Nop(),
		tradingOrderRepo: &stubOrderRepo{},
		tradingFillRepo: &stubFillRepo{
			sumByWindow: map[string]float64{
				"today": 1234.56,
				"7d":    9876.54,
			},
		},
	}
	project := &registry.Project{
		ID: "p1",
		MCP: registry.ProjectMCP{
			Servers: []registry.MCPServerConfig{{Name: "broker", URL: broker.URL}},
		},
	}
	panel := s.buildTradingPanel(context.Background(), project)

	assert.True(t, panel.Enabled)
	assert.Equal(t, 1234.56, panel.Soak.VolumeTodayUSD,
		"volume today must come from trading_fills SumVolume")
	assert.Equal(t, 9876.54, panel.Soak.Volume7dUSD,
		"volume 7d must come from trading_fills SumVolume")
}

// TestBuildTradingPanel_VolumeFallbackWhenFillRepoNil — without a
// fill repo wired, the panel falls back to the legacy
// trading_orders LMT-price estimate. Both calc paths coexist —
// deployments that haven't enabled Phase-3 ingestion keep their
// existing tile populated.
func TestBuildTradingPanel_VolumeFallbackWhenFillRepoNil(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": map[string]any{"mode": "paper"},
			"state":      map[string]any{"kill_switch": false},
		})
	}))
	defer broker.Close()

	s := &Server{
		logger:           zerolog.Nop(),
		tradingOrderRepo: &stubOrderRepo{}, // empty list → estimate is 0
		// tradingFillRepo: nil — fallback path active
	}
	project := &registry.Project{
		ID: "p1",
		MCP: registry.ProjectMCP{
			Servers: []registry.MCPServerConfig{{Name: "broker", URL: broker.URL}},
		},
	}
	panel := s.buildTradingPanel(context.Background(), project)

	assert.True(t, panel.Enabled)
	assert.Equal(t, 0.0, panel.Soak.VolumeTodayUSD,
		"empty order list under legacy fallback yields zero volume")
}
