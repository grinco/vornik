package registry

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestProjectEntryPolicyYAML(t *testing.T) {
	var p Project
	require.NoError(t, yaml.Unmarshal([]byte(`trading:
  entry_policy:
    enabled: true
    allowed_symbols: [MSFT, JPM]
    long_only: true
    max_risk_usd: 50
    min_notional_usd: 500
    max_entries_per_tick: 1
`), &p))
	require.True(t, p.Trading.EntryPolicy.Enabled)
	require.True(t, p.Trading.EntryPolicy.LongOnly)
	require.Equal(t, []string{"MSFT", "JPM"}, p.Trading.EntryPolicy.AllowedSymbols)
	require.Equal(t, 50.0, p.Trading.EntryPolicy.MaxRiskUSD)
	require.NoError(t, p.Trading.EntryPolicy.Validate())
	p.ID, p.SwarmID, p.DefaultWorkflowID = "p", "s", "w"
	require.NoError(t, p.Validate("p.yaml"))
	p.Trading.EntryPolicy.MaxRiskUSD = 0
	require.ErrorContains(t, p.Validate("p.yaml"), "trading.entry_policy")
}
