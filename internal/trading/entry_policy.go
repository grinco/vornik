package trading

import (
	"fmt"
	"math"
)

// EntryPolicy limits proposed entries independently of the scorecard switches.
// AllowedSymbols is an entry universe, not a restriction on closing holdings.
// Dollar risk is planned loss at the submitted stop, not a guaranteed loss cap:
// gaps, slippage and commissions can increase the actual loss.
type EntryPolicy struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	AllowedSymbols      []string `yaml:"allowed_symbols" json:"allowed_symbols"`
	LongOnly            bool     `yaml:"long_only" json:"long_only"`
	NoPositionAdditions bool     `yaml:"no_position_additions" json:"no_position_additions"`
	MaxRiskUSD          float64  `yaml:"max_risk_usd" json:"max_risk_usd"`
	MinNotionalUSD      float64  `yaml:"min_notional_usd" json:"min_notional_usd"`
	MaxEntriesPerTick   int      `yaml:"max_entries_per_tick" json:"max_entries_per_tick"`
	// Portfolio gates (2026-09-10), broker-only because the workflow filter
	// never sees holdings. Zero is off. Gross exposure counts held market
	// value plus working non-child order notional plus the order at hand;
	// positions count distinct held plus pending symbols; the daily-loss
	// pause refuses opens once the session's realised P&L is at or below
	// the negated value.
	MaxGrossExposureUSD float64 `yaml:"max_gross_exposure_usd" json:"max_gross_exposure_usd"`
	MaxPositions        int     `yaml:"max_positions" json:"max_positions"`
	DailyLossPauseUSD   float64 `yaml:"daily_loss_pause_usd" json:"daily_loss_pause_usd"`
}

// Validate rejects settings that could silently disable a numeric gate.
func (p EntryPolicy) Validate() error {
	for _, n := range []float64{p.MaxRiskUSD, p.MinNotionalUSD, p.MaxGrossExposureUSD, p.DailyLossPauseUSD} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return fmt.Errorf("entry risk, notional, exposure and loss-pause limits must be finite and nonnegative")
		}
	}
	if p.MaxEntriesPerTick < 0 || p.MaxPositions < 0 {
		return fmt.Errorf("max_entries_per_tick and max_positions cannot be negative")
	}
	if p.Enabled && (p.MaxRiskUSD == 0 || p.MaxEntriesPerTick == 0) {
		return fmt.Errorf("enabled entry policy requires positive max_risk_usd and max_entries_per_tick")
	}
	return nil
}
