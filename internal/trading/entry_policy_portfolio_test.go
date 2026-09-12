package trading

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// 2026-09-10 amendment: the three portfolio gates travel in the entry policy.
// Zero is off; negative or non-finite values are invalid even when disabled.
func TestEntryPolicyPortfolioLimitsValidate(t *testing.T) {
	base := EntryPolicy{Enabled: true, MaxRiskUSD: 50, MaxEntriesPerTick: 1}
	require.NoError(t, base.Validate())
	ok := base
	ok.MaxGrossExposureUSD, ok.MaxPositions, ok.DailyLossPauseUSD = 15000, 6, 150
	require.NoError(t, ok.Validate())
	for name, mutate := range map[string]func(*EntryPolicy){
		"negative exposure":  func(p *EntryPolicy) { p.MaxGrossExposureUSD = -1 },
		"nan exposure":       func(p *EntryPolicy) { p.MaxGrossExposureUSD = math.NaN() },
		"inf pause":          func(p *EntryPolicy) { p.DailyLossPauseUSD = math.Inf(1) },
		"negative pause":     func(p *EntryPolicy) { p.DailyLossPauseUSD = -150 },
		"negative positions": func(p *EntryPolicy) { p.MaxPositions = -6 },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			p.Enabled = false
			mutate(&p)
			require.Error(t, p.Validate())
		})
	}
}
