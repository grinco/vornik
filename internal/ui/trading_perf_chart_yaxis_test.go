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
