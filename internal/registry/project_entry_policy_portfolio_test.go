package registry

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// 2026-09-10: the portfolio gates and the analysis-evidence gate parse from
// project YAML and are validated with the project.
func TestProjectTradingPortfolioAndEvidenceConfig(t *testing.T) {
	var p Project
	require.NoError(t, yaml.Unmarshal([]byte(`
projectId: t
displayName: t
swarmId: s
defaultWorkflowId: w
trading:
  entry_policy:
    enabled: true
    allowed_symbols: [MSFT]
    max_risk_usd: 50
    max_entries_per_tick: 1
    max_gross_exposure_usd: 15000
    max_positions: 6
    daily_loss_pause_usd: 150
  analysis_evidence:
    enabled: true
    min_daily_bars: 205
    benchmark_symbols: [SPY]
`), &p))
	require.NoError(t, p.Validate("t.yaml"))
	require.Equal(t, 15000.0, p.Trading.EntryPolicy.MaxGrossExposureUSD)
	require.Equal(t, 6, p.Trading.EntryPolicy.MaxPositions)
	require.Equal(t, 150.0, p.Trading.EntryPolicy.DailyLossPauseUSD)
	require.Equal(t, []string{"SPY"}, p.Trading.AnalysisEvidence.BenchmarkSymbols)

	p.Trading.AnalysisEvidence.MinDailyBars = 0
	err := p.Validate("t.yaml")
	require.Error(t, err)
	require.Contains(t, err.Error(), "trading.analysis_evidence")
}
