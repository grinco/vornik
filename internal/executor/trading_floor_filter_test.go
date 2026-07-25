package executor

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func tradingFloorProject(id string) *registry.Project {
	return &registry.Project{
		ID: id,
		Trading: registry.ProjectTrading{
			Watchlist:        []string{"AAPL", "NVO"},
			ProtectedSymbols: []string{"RSU-LOCKUP"},
			Scorecard:        registry.TradingScorecard{Enabled: true, MinEntryTotal: 3},
			Regime: registry.TradingRegime{
				Enabled: true, BlockLongInRiskOff: true, StaleBehavior: "block_opens",
				MinComponentCount: map[string]int{"us": 3},
			},
		},
	}
}

// The executor helper drops a sub-floor open (soft) via the shared filter.
func TestFilterTradingFloor_DropsSubFloorOpen(t *testing.T) {
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": tradingFloorProject("trade")}}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	in := []byte(`{"proposals":[{"symbol":"NVO","intent":"open","action":"BUY","region":"us","scorecard":{"total":0},"regime":{"label":"RISK_ON","component_count":3}}]}`)
	out, err := e.filterTradingFloor(&persistence.Task{ID: "t", ProjectID: "trade"}, in)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "NVO", "sub-floor open dropped")
	assert.Contains(t, string(out), `"has_proposals":false`)
}

// Protected-symbol close → hard error (step fails).
func TestFilterTradingFloor_ProtectedCloseHardFails(t *testing.T) {
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": tradingFloorProject("trade")}}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	in := []byte(`{"proposals":[{"symbol":"RSU-LOCKUP","intent":"close","action":"SELL","qty":9}]}`)
	_, err := e.filterTradingFloor(&persistence.Task{ID: "t", ProjectID: "trade"}, in)
	require.Error(t, err)
}

// Non-trading project (no watchlist) → bytes unchanged, no-op.
func TestFilterTradingFloor_NonTradingNoOp(t *testing.T) {
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{"p": {ID: "p"}}}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	in := []byte(`{"proposals":[{"symbol":"NVO","intent":"open","action":"BUY","scorecard":{"total":0},"regime":{}}]}`)
	out, err := e.filterTradingFloor(&persistence.Task{ID: "t", ProjectID: "p"}, in)
	require.NoError(t, err)
	assert.Equal(t, string(in), string(out))
}
