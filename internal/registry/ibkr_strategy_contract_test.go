package registry

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestIBKRStrategyV2RegistryContract(t *testing.T) {
	projectPath := "../../configs/projects/ibkr-trader.yaml.template"
	swarmPath := "../../configs/swarms/ibkr-trader-swarm.md"
	if stage := os.Getenv("IBKR_STRATEGY_CHECK_DIR"); stage != "" {
		projectPath, swarmPath = stage+"/project.yaml", stage+"/swarm.md"
	}
	data, err := os.ReadFile(projectPath)
	require.NoError(t, err)
	var p Project
	require.NoError(t, yaml.Unmarshal(data, &p))
	require.NoError(t, p.Validate(projectPath))
	require.NotContains(t, string(data), "get_positions", "broker positions come from get_account_summary.positions")
	require.True(t, p.Trading.EntryPolicy.Enabled)
	require.True(t, p.Trading.EntryPolicy.NoPositionAdditions)
	// 2026-09-10: portfolio gates and the analysis-evidence gate ship on.
	require.Positive(t, p.Trading.EntryPolicy.MaxGrossExposureUSD)
	require.Positive(t, p.Trading.EntryPolicy.MaxPositions)
	require.Positive(t, p.Trading.EntryPolicy.DailyLossPauseUSD)
	require.True(t, p.Trading.AnalysisEvidence.Enabled)
	require.Equal(t, 205, p.Trading.AnalysisEvidence.MinDailyBars)
	require.Contains(t, p.Trading.AnalysisEvidence.BenchmarkSymbols, "SPY")
	require.Contains(t, p.HallucinationJudge.Prompt, "holdings_review")
	require.Equal(t, "60m", p.Autonomy.PollInterval)
	for _, symbol := range p.Trading.EntryPolicy.AllowedSymbols {
		require.Contains(t, p.Trading.Watchlist, symbol)
	}
	data, err = os.ReadFile("../../configs/workflows/ibkr-trading-v2.md")
	require.NoError(t, err)
	require.NotContains(t, string(data), "get_positions")
	require.Contains(t, string(data), "holdings_review")
	require.NotContains(t, string(data), "Pass the actual full daily history to scorecard", "scorecard takes a symbol since 2026-09-10")
	w, err := ParseWorkflowMarkdown(data, "ibkr-trading-v2.md")
	require.NoError(t, err)
	require.Equal(t, w.ID, p.DefaultWorkflowID)
	data, err = os.ReadFile(swarmPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "get_positions")
	require.Contains(t, string(data), "holdings_review")
	s, err := ParseSwarmMarkdown(data, swarmPath)
	require.NoError(t, err)
	require.Equal(t, s.ID, p.SwarmID)
}
