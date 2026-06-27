package ui

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// snap is a tiny helper for building one TradingPositionsSnapshot.
// The math layer ignores everything except RecordedAt + EquityUSD;
// other fields are zeroed.
func snap(at time.Time, equity float64) *persistence.TradingPositionsSnapshot {
	return &persistence.TradingPositionsSnapshot{
		RecordedAt: at,
		EquityUSD:  equity,
	}
}

// TestComputeSoakMetrics_EmptyInput — no snapshots produces a
// zero-valued struct with SoakReady=false. The panel template
// hides the headline numbers in that state.
func TestComputeSoakMetrics_EmptyInput(t *testing.T) {
	out := computeSoakMetrics(nil)
	assert.False(t, out.SoakReady)
	assert.Equal(t, 0, out.SampleCount)
	assert.Equal(t, 0, out.WindowDays)
	assert.Zero(t, out.SharpeAnnualized)
	assert.Zero(t, out.MaxDrawdownPct)
}

// TestComputeSoakMetrics_BelowSoakReadyThreshold — fewer than
// 5 distinct UTC dates means SoakReady=false even when there
// are many intra-day samples. Drawdown still computes (single-
// sample math is well-defined); Sharpe stays zero so the
// template doesn't surface a misleading number.
func TestComputeSoakMetrics_BelowSoakReadyThreshold(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(t0, 10000),
		snap(t0.Add(1*time.Hour), 9800), // intra-day dip
		snap(t0.Add(2*time.Hour), 10100),
	}
	out := computeSoakMetrics(snaps)
	assert.False(t, out.SoakReady, "single-day series cannot be soak-ready")
	assert.Equal(t, 3, out.SampleCount)
	assert.Equal(t, 1, out.WindowDays)
	assert.Zero(t, out.SharpeAnnualized)
	// Drawdown: HWM=10000, low=9800 → 2%
	assert.InDelta(t, 2.0, out.MaxDrawdownPct, 0.01)
}

// TestComputeSoakMetrics_ConstantReturnsHandlesZeroStddev — a
// path with a constant per-day return mathematically has
// stddev=0; float64 noise leaves a residual ~1e-16 which would
// blow the Sharpe up to ~1e16 without the epsilon guard. The
// production code returns 0 in this case so the panel doesn't
// surface a wildly misleading number on a degenerate input.
// The realistic-jittered case is exercised by
// TestComputeSoakMetrics_RisingEquityWithJitter below.
func TestComputeSoakMetrics_ConstantReturnsHandlesZeroStddev(t *testing.T) {
	base := time.Date(2026, 5, 1, 16, 0, 0, 0, time.UTC)
	var snaps []*persistence.TradingPositionsSnapshot
	equity := 10000.0
	for d := 0; d < 10; d++ {
		snaps = append(snaps, snap(base.AddDate(0, 0, d), equity))
		equity *= 1.005
	}
	out := computeSoakMetrics(snaps)
	assert.True(t, out.SoakReady, "10 daily samples should be soak-ready")
	assert.Zero(t, out.SharpeAnnualized,
		"constant per-day return → stddev≈0 → defensive Sharpe=0 (epsilon guard)")
	assert.Zero(t, out.MaxDrawdownPct, "monotonic-up series has no drawdown")
}

// TestComputeSoakMetrics_RisingEquityWithJitter — the more
// realistic case: returns are positive on average but with
// some noise so stddev > 0. Sharpe must be a reasonable
// positive number (~1-3 range for well-behaved inputs).
func TestComputeSoakMetrics_RisingEquityWithJitter(t *testing.T) {
	base := time.Date(2026, 5, 1, 16, 0, 0, 0, time.UTC)
	// 6 daily returns: alternating +1% / -0.4% — mean is positive,
	// stddev nonzero. Net upward trend → Sharpe > 0.
	deltas := []float64{1.01, 0.996, 1.01, 0.996, 1.01, 0.996, 1.01}
	var snaps []*persistence.TradingPositionsSnapshot
	equity := 10000.0
	for d, mul := range deltas {
		snaps = append(snaps, snap(base.AddDate(0, 0, d), equity))
		equity *= mul
	}
	out := computeSoakMetrics(snaps)
	require.True(t, out.SoakReady, "7 daily samples should be soak-ready (need 5+)")
	assert.Greater(t, out.SharpeAnnualized, 0.0, "net positive returns must produce positive Sharpe")
	assert.False(t, math.IsNaN(out.SharpeAnnualized))
	assert.False(t, math.IsInf(out.SharpeAnnualized, 0))
}

// TestComputeSoakMetrics_FallingEquity — losing strategy
// produces negative Sharpe AND non-zero drawdown. The drawdown
// must reflect the deepest peak-to-trough.
func TestComputeSoakMetrics_FallingEquity(t *testing.T) {
	base := time.Date(2026, 5, 1, 16, 0, 0, 0, time.UTC)
	// Equity path: 10000 → 11000 → 9000 → 9500 → 8500 → 9000 → 8000
	// HWM hits 11000 at index 1; trough at index 6 → 8000.
	// Max drawdown = (11000 - 8000) / 11000 = 27.27%
	path := []float64{10000, 11000, 9000, 9500, 8500, 9000, 8000}
	var snaps []*persistence.TradingPositionsSnapshot
	for i, eq := range path {
		snaps = append(snaps, snap(base.AddDate(0, 0, i), eq))
	}
	out := computeSoakMetrics(snaps)
	require.True(t, out.SoakReady)
	assert.Less(t, out.SharpeAnnualized, 0.0, "net negative returns must produce negative Sharpe")
	assert.InDelta(t, 27.272, out.MaxDrawdownPct, 0.01)
}

// TestMaxDrawdownPct_IntraDayRecovery — a deep intra-day dip
// that fully recovers must still surface in MaxDrawdown. The
// broker's own drawdown_pct resets daily; the snapshot-derived
// figure walks the full window so it captures these.
func TestMaxDrawdownPct_IntraDayRecovery(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(t0, 10000),
		snap(t0.Add(2*time.Hour), 9500),  // -5% intra-day
		snap(t0.Add(4*time.Hour), 10000), // recovered
	}
	dd := maxDrawdownPct(snaps)
	assert.InDelta(t, 5.0, dd, 0.01, "intra-day 5% dip must register even though equity recovered")
}

// TestEquity24hChange_HappyPath — equity moved +3% in 24h.
func TestEquity24hChange_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(now.Add(-30*time.Hour), 9500),  // outside window
		snap(now.Add(-23*time.Hour), 10000), // first inside 24h
		snap(now.Add(-12*time.Hour), 10100),
		snap(now, 10300),
	}
	pct, usd := equity24hChange(snaps)
	assert.InDelta(t, 3.0, pct, 0.01, "(10300 - 10000) / 10000 * 100 = 3.0%")
	assert.InDelta(t, 300.0, usd, 0.01, "absolute delta 10300 - 10000 = $300")
}

// TestEquity24hChange_SubPercentVisibleInUSD — the regression
// driver: a $30 move on a $1M account rounds to "0.00%" with
// the panel's 2-dp percent format but the USD field still
// surfaces it. The point of carrying both is that an operator
// shouldn't mistake "tiny move" for "no move".
func TestEquity24hChange_SubPercentVisibleInUSD(t *testing.T) {
	now := time.Date(2026, 5, 13, 14, 0, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(now.Add(-23*time.Hour), 1_045_724.43),
		snap(now, 1_045_754.79),
	}
	pct, usd := equity24hChange(snaps)
	assert.InDelta(t, 0.0029, pct, 0.001, "30.36 / 1045724.43 * 100 ≈ 0.0029%")
	assert.InDelta(t, 30.36, usd, 0.01, "absolute delta carries the dollars the percent rounds away")
}

// TestEquity24hChange_NoSamplesIn24h — single sample outside
// the window plus the latest just produces 0; we don't lie
// about a synthetic "since-launch" return.
func TestEquity24hChange_OnlyLatest(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(now, 10000),
	}
	pct, usd := equity24hChange(snaps)
	assert.Zero(t, pct, "single sample → cannot compute 24h change → return 0")
	assert.Zero(t, usd, "single sample → cannot compute 24h change → return 0 USD")
}

// TestComputeSoakMetrics_IgnoresZeroEquitySamples — a sample
// with equity ≤ 0 is corrupt (broker hiccup, log replay) and
// must not poison the Sharpe / drawdown math.
func TestComputeSoakMetrics_IgnoresZeroEquitySamples(t *testing.T) {
	base := time.Date(2026, 5, 1, 16, 0, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(base, 10000),
		snap(base.AddDate(0, 0, 1), 0), // corrupt — must skip
		snap(base.AddDate(0, 0, 2), 10100),
		snap(base.AddDate(0, 0, 3), 10200),
		snap(base.AddDate(0, 0, 4), 10300),
		snap(base.AddDate(0, 0, 5), 10400),
	}
	out := computeSoakMetrics(snaps)
	// Window days = 5 valid (one zero skipped); SampleCount
	// counts ALL rows for transparency.
	assert.Equal(t, 6, out.SampleCount, "raw sample count includes corrupt rows for transparency")
	assert.Equal(t, 5, out.WindowDays, "WindowDays excludes the zero-equity row")
}

// TestComputeSoakMetrics_DailyClosePicksLatestPerDay — the
// daily-close picker must pick the LAST sample of each UTC
// date, not the first. Confirmed by counting WindowDays
// (3 unique dates from 6 intra-day rows) plus SampleCount
// (full row count).
func TestComputeSoakMetrics_DailyClosePicksLatestPerDay(t *testing.T) {
	d1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	snaps := []*persistence.TradingPositionsSnapshot{
		snap(d1.Add(9*time.Hour), 10000),
		snap(d1.Add(15*time.Hour), 10500), // d1 close
		snap(d2.Add(9*time.Hour), 10300),
		snap(d2.Add(15*time.Hour), 10800), // d2 close
		snap(d3.Add(9*time.Hour), 10600),
		snap(d3.Add(15*time.Hour), 11000), // d3 close
	}
	out := computeSoakMetrics(snaps)
	assert.Equal(t, 3, out.WindowDays, "three unique UTC dates")
	assert.Equal(t, 6, out.SampleCount, "all six rows counted")
	// Drawdown walks the FULL series (not just daily closes)
	// so the morning dip on d2 (10800 HWM → 10300 = ~4.6%)
	// IS captured even though d2's close was higher than d1.
	// Computed exactly: HWM rises to 10500 then to 10800;
	// largest dip from HWM is (10800-10600)/10800 = 1.85% on
	// d3 morning, NOT (10500-10300)/10500=1.9% because by
	// the time we see 10300 HWM is still 10500.
	// Walking: 10000→HWM=10000; 10500→HWM=10500; 10300→DD=
	// (10500-10300)/10500*100 = 1.9047... ; 10800→HWM=10800;
	// 10600→DD=(10800-10600)/10800*100 = 1.851...; 11000→HWM
	// final. Max DD = 1.9047%.
	assert.InDelta(t, 1.9047, out.MaxDrawdownPct, 0.01)
}
