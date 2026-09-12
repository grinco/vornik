package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
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

func TestEntryPolicyGatesApprovalsWithScorecardOff(t *testing.T) {
	p := tradingFloorProject("trade")
	p.Trading.Scorecard.Enabled = false
	p.Trading.EntryPolicy = trading.EntryPolicy{Enabled: true, AllowedSymbols: []string{"AAPL"}, LongOnly: true, MaxRiskUSD: 50, MaxEntriesPerTick: 1}
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": p}}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	task := &persistence.Task{ID: "t", ProjectID: "trade", Payload: []byte(`{"taskType":"trading"}`)}
	out, err := e.filterTradingEntryPolicy(task, []byte(`{"approved":[{"symbol":"NVO","intent":"open","action":"BUY"}],"has_approvals":true}`))
	require.NoError(t, err)
	assert.Contains(t, string(out), `"approved":[]`)
	assert.Contains(t, string(out), `"has_approvals":false`)
	state := &StepOutcome{Task: task, ResultBytes: []byte(`{"approved":[{"symbol":"NVO","intent":"open","action":"BUY"}],"has_approvals":true}`)}
	e.tradingFloorParticipant(context.Background(), state)
	require.NoError(t, state.Err)
	assert.Contains(t, string(state.ResultBytes), `"approved":[]`)
	task.Payload = []byte(`{"taskType":"research"}`)
	out, err = e.filterTradingEntryPolicy(task, []byte("Research prose without an order envelope"))
	require.NoError(t, err)
	assert.Equal(t, "Research prose without an order envelope", string(out))
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
