package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/tradingpnl"
)

// TestLayoutPerfChart_YAxisLabels covers the 2026-07-16 gap: the daily P&L
// chart rendered bars + equity line with NO Y-axis value labels, so the
// scale was unreadable. layoutPerfChart must now emit a realized (left)
// axis — +max / $0 / -max around the baseline — and an equity (right)
// axis (max / min), and shift the plot right to make room.
func TestLayoutPerfChart_YAxisLabels(t *testing.T) {
	d := func(n int) time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n) }
	daily := []tradingpnl.DailyPoint{
		{Day: d(0), RealizedUSD: 182, EquityUSD: 10000},
		{Day: d(1), RealizedUSD: -50, EquityUSD: 9800},
	}
	c := layoutPerfChart(daily)

	// Realized (left) axis: three ticks, top→bottom = +max, $0, -max.
	require.Len(t, c.YAxisLeft, 3, "realized axis has +max / 0 / -max")
	assert.Equal(t, perfChartTop, c.YAxisLeft[0].Y, "top tick at chart top")
	assert.Equal(t, perfChartTop+perfChartH, c.YAxisLeft[2].Y, "bottom tick at chart bottom")
	assert.Contains(t, c.YAxisLeft[1].Label, "0", "middle tick is the zero baseline")
	assert.Contains(t, c.YAxisLeft[0].Label, "182", "top tick shows +max realized")
	assert.Equal(t, "end", c.YAxisLeft[0].Anchor, "left-axis labels are right-anchored")

	// Equity (right) axis: max on top, min on bottom.
	require.Len(t, c.YAxisRight, 2, "equity axis has max / min")
	assert.Contains(t, c.YAxisRight[0].Label, "10", "top = max equity")
	assert.Contains(t, c.YAxisRight[1].Label, "9", "bottom = min equity")
	assert.Equal(t, "start", c.YAxisRight[0].Anchor, "right-axis labels are left-anchored")

	// Plot shifted right to clear the left gutter; baseline spans the plot.
	assert.Greater(t, c.Points[0].X, perfChartLeftPad, "bars shifted past the left gutter")
	assert.Greater(t, c.PlotLeft, 0)
	assert.Less(t, c.PlotRight, c.SVGWidth)
}

// No equity → no right axis, but the realized axis still renders.
func TestLayoutPerfChart_YAxisRealizedOnlyWhenNoEquity(t *testing.T) {
	d := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	c := layoutPerfChart([]tradingpnl.DailyPoint{{Day: d, RealizedUSD: 100}})
	assert.Len(t, c.YAxisLeft, 3)
	assert.Empty(t, c.YAxisRight, "no equity → no right axis")
}

// TestBuildEquityAxis_LabelsMustDiffer reproduces the operator report of
// 2026-08-07: "the graphs on top of the trading dashboard don't match up".
//
// Real data from the ibkr-trader paper account: equity ran $1,051,739 →
// $1,052,629 — an $890 move (0.08%). The equity curve is auto-scaled to its own
// min/max, so it climbed the full plot height, while fmtAxisUSD rendered BOTH
// axis ticks as "$1.1M" because it formats to one decimal at the millions
// scale. The chart therefore showed a dramatic rise against an axis claiming
// the range was $1.1M to $1.1M — unreadable, and the reason it "doesn't match".
func TestBuildEquityAxis_LabelsMustDiffer(t *testing.T) {
	cases := []struct {
		name     string
		min, max float64
	}{
		{"observed paper account", 1_051_739, 1_052_629},
		{"tight range at millions", 1_000_000, 1_000_400},
		{"tight range at thousands", 10_000, 10_040},
		{"wide range", 900_000, 1_200_000},
		{"small account", 9_980, 10_050},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext := perfExtents{hasEquity: true, minEquity: c.min, maxEquity: c.max}
			ticks := buildEquityAxis(ext, 800)
			if len(ticks) != 2 {
				t.Fatalf("got %d ticks, want 2", len(ticks))
			}
			if ticks[0].Label == ticks[1].Label {
				t.Errorf("axis top and bottom render the SAME label %q for range $%.0f–$%.0f — "+
					"the reader cannot tell what the curve's vertical span means",
					ticks[0].Label, c.min, c.max)
			}
		})
	}
}
