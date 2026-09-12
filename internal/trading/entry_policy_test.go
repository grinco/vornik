package trading

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntryPolicyValidate(t *testing.T) {
	require.NoError(t, (EntryPolicy{}).Validate())
	require.NoError(t, (EntryPolicy{Enabled: true, MaxRiskUSD: 50, MaxEntriesPerTick: 1}).Validate())
	for _, p := range []EntryPolicy{
		{Enabled: true}, {MaxRiskUSD: -1}, {MaxRiskUSD: math.NaN()},
		{MinNotionalUSD: math.Inf(1)}, {MaxEntriesPerTick: -1},
	} {
		require.Error(t, p.Validate())
	}
}
