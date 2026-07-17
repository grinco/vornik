package ui

// trading_perf_chart.go — server-rendered SVG geometry for the
// Trading Performance daily P&L bar chart + equity line overlay.
//
// Mirrors the approach used in insights_trends.go: compute bar/point
// coordinates in Go and pass them to the template as plain integers /
// floats so the template emits the SVG directly with no client-side JS.

import (
	"fmt"
	"math"
	"strings"

	"vornik.io/vornik/internal/tradingpnl"
)

// perfChart* constants define the SVG canvas geometry (px).
const (
	perfChartH       = 120 // drawable area height (bars + line live here)
	perfChartTop     = 10  // top padding above bars
	perfChartColW    = 40  // column width per day
	perfChartBarW    = 26  // bar width (centred in column)
	perfChartLeftPad = 6   // left margin (inside the plot area)
	// Axis gutters (px) reserved OUTSIDE the plot area for the Y-axis
	// value labels: realized P&L on the left, equity on the right
	// (2026-07-16). The plot area is shifted right by perfChartLeftGutter.
	perfChartLeftGutter  = 48
	perfChartRightGutter = 52
)

// perfAxisTick is one Y-axis value label. Anchor is the SVG
// text-anchor ("end" for the left/realized axis, "start" for the
// right/equity axis).
type perfAxisTick struct {
	X      int
	Y      int
	Label  string
	Anchor string
}

// perfBarPoint is one day's bar + optional equity-line vertex for the SVG.
type perfBarPoint struct {
	// Date is the day label (YYYY-MM-DD) used for tooltip / aria.
	Date string

	// Bar geometry (realized P&L bar, centred in its column).
	X                int  // left edge of bar rect
	BarY             int  // top-left of bar rect
	BarH             int  // height of bar rect (always ≥0)
	BarAboveBaseline bool // true when realized>0 (green), false when <0 (red)

	// Equity-line vertex.
	HasEquity bool
	EX        int    // x-coordinate of the equity dot/vertex
	EY        int    // y-coordinate
	ELabel    string // "$N" for tooltip

	// X-axis label fields (every-other-day only; pre-computed to avoid
	// template arithmetic).
	ShowLabel bool
	DateShort string // "MM-DD" from Date
	LabelY    int    // y coordinate of the label text
}

// perfChartData is the complete set of geometry passed to the template.
type perfChartData struct {
	Points    []perfBarPoint
	SVGWidth  int
	SVGHeight int
	// PolylinePoints is the SVG <polyline points="…"> attribute value
	// for the equity line.
	PolylinePoints string
	HasEquity      bool
	HasBars        bool
	// Y-axis value labels (2026-07-16). YAxisLeft is the realized-P&L
	// scale (+max / $0 / -max around the baseline); YAxisRight is the
	// equity scale (max / min), empty when there's no equity data.
	YAxisLeft  []perfAxisTick
	YAxisRight []perfAxisTick
	// PlotLeft/PlotRight bound the plotting area (x) between the axis
	// gutters — the zero baseline spans this range, not the full width.
	PlotLeft  int
	PlotRight int
}

// perfExtents holds the axis-scaling values derived from the daily series.
type perfExtents struct {
	maxAbsRealized float64
	minEquity      float64
	maxEquity      float64
	hasEquity      bool
	hasBars        bool
}

// computePerfExtents scans the daily series once to find P&L and equity range.
func computePerfExtents(daily []tradingpnl.DailyPoint) perfExtents {
	ext := perfExtents{minEquity: math.MaxFloat64, maxEquity: -math.MaxFloat64}
	for _, d := range daily {
		if abs := math.Abs(d.RealizedUSD); abs > ext.maxAbsRealized {
			ext.maxAbsRealized = abs
		}
		if d.RealizedUSD != 0 {
			ext.hasBars = true
		}
		if d.EquityUSD == 0 {
			continue
		}
		ext.hasEquity = true
		if d.EquityUSD < ext.minEquity {
			ext.minEquity = d.EquityUSD
		}
		if d.EquityUSD > ext.maxEquity {
			ext.maxEquity = d.EquityUSD
		}
	}
	if !ext.hasEquity {
		ext.minEquity = 0
		ext.maxEquity = 0
	}
	return ext
}

// buildPerfBarGeometry fills the bar-related fields of p for one daily point.
func buildPerfBarGeometry(p *perfBarPoint, realized float64, maxAbs float64, baseline int) {
	if maxAbs <= 0 {
		p.BarY = baseline
		return
	}
	barH := int(math.Abs(realized) * float64(perfChartH/2) / maxAbs)
	if barH < 1 && realized != 0 {
		barH = 1
	}
	if realized >= 0 {
		p.BarY = baseline - barH
		p.BarAboveBaseline = true
	} else {
		p.BarY = baseline
	}
	p.BarH = barH
}

// buildPerfEquityVertex fills the equity-line fields of p for one daily point.
// Returns the "cx,cy" polyline fragment (empty string when equityUSD==0).
func buildPerfEquityVertex(p *perfBarPoint, equityUSD float64, cx int, ext perfExtents) string {
	if equityUSD == 0 {
		return ""
	}
	equityRange := ext.maxEquity - ext.minEquity
	var ey int
	if equityRange > 0 {
		ey = perfChartTop + int((ext.maxEquity-equityUSD)*float64(perfChartH)/equityRange)
	} else {
		ey = perfChartTop + perfChartH/2
	}
	p.HasEquity = true
	p.EX = cx
	p.EY = ey
	p.ELabel = fmt.Sprintf("$%.0f", equityUSD)
	return fmt.Sprintf("%d,%d", cx, ey)
}

// layoutPerfChart builds the SVG geometry from the merged daily series
// produced by mergePerfDaily. Pure; no I/O.
func layoutPerfChart(daily []tradingpnl.DailyPoint) perfChartData {
	n := len(daily)
	if n == 0 {
		return perfChartData{}
	}

	ext := computePerfExtents(daily)
	baseline := perfChartTop + perfChartH/2
	labelY := perfChartTop + perfChartH + 16
	plotLeft := perfChartLeftGutter

	points := make([]perfBarPoint, n)
	polyParts := make([]string, 0, n)

	for i, d := range daily {
		x := plotLeft + perfChartLeftPad + i*perfChartColW
		cx := x + perfChartColW/2
		dateStr := d.Day.Format("2006-01-02")

		p := perfBarPoint{
			Date:      dateStr,
			X:         cx - perfChartBarW/2,
			ShowLabel: i%2 == 0,
			DateShort: dateStr[5:], // "MM-DD"
			LabelY:    labelY,
		}
		buildPerfBarGeometry(&p, d.RealizedUSD, ext.maxAbsRealized, baseline)
		if frag := buildPerfEquityVertex(&p, d.EquityUSD, cx, ext); frag != "" {
			polyParts = append(polyParts, frag)
		}
		points[i] = p
	}

	svgWidth := plotLeft + perfChartLeftPad + n*perfChartColW + perfChartRightGutter
	plotRight := svgWidth - perfChartRightGutter

	return perfChartData{
		Points:         points,
		SVGWidth:       svgWidth,
		SVGHeight:      perfChartTop + perfChartH + 22,
		PolylinePoints: strings.Join(polyParts, " "),
		HasEquity:      ext.hasEquity,
		HasBars:        ext.hasBars,
		YAxisLeft:      buildRealizedAxis(ext, baseline, plotLeft),
		YAxisRight:     buildEquityAxis(ext, svgWidth),
		PlotLeft:       plotLeft,
		PlotRight:      plotRight,
	}
}

// buildRealizedAxis emits the left Y-axis ticks for the realized-P&L bars:
// +max at the top, $0 at the baseline, -max at the bottom. Right-anchored
// just inside the left gutter.
func buildRealizedAxis(ext perfExtents, baseline, plotLeft int) []perfAxisTick {
	x := plotLeft - 6
	return []perfAxisTick{
		{X: x, Y: perfChartTop, Label: "+" + fmtAxisUSD(ext.maxAbsRealized), Anchor: "end"},
		{X: x, Y: baseline, Label: "$0", Anchor: "end"},
		{X: x, Y: perfChartTop + perfChartH, Label: "-" + fmtAxisUSD(ext.maxAbsRealized), Anchor: "end"},
	}
}

// buildEquityAxis emits the right Y-axis ticks for the equity line: max at
// the top, min at the bottom. Left-anchored just inside the right gutter.
// Empty when there's no equity data (broker-offline / no snapshots).
func buildEquityAxis(ext perfExtents, svgWidth int) []perfAxisTick {
	if !ext.hasEquity {
		return nil
	}
	x := svgWidth - perfChartRightGutter + 6
	return []perfAxisTick{
		{X: x, Y: perfChartTop, Label: fmtAxisUSD(ext.maxEquity), Anchor: "start"},
		{X: x, Y: perfChartTop + perfChartH, Label: fmtAxisUSD(ext.minEquity), Anchor: "start"},
	}
}

// fmtAxisUSD renders a compact USD axis label: "$1.2k", "$12.3k", "$1.2M",
// or "$182" for small magnitudes. Sign is applied by the caller.
func fmtAxisUSD(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1_000_000:
		return fmt.Sprintf("$%.1fM", a/1_000_000)
	case a >= 1_000:
		return fmt.Sprintf("$%.1fk", a/1_000)
	default:
		return fmt.Sprintf("$%.0f", a)
	}
}

// mergePerfDaily merges Aggregate's realized daily series with EquityCurve's
// equity series into a unified []tradingpnl.DailyPoint for chart rendering.
// Days from either source that don't appear in the other keep their zero value.
func mergePerfDaily(realized []tradingpnl.DailyPoint, equity []tradingpnl.DailyPoint) []tradingpnl.DailyPoint {
	if len(realized) == 0 && len(equity) == 0 {
		return nil
	}

	// Collect all unique days.
	daySet := make(map[string]tradingpnl.DailyPoint)
	for _, r := range realized {
		key := r.Day.Format("2006-01-02")
		e := daySet[key]
		e.Day = r.Day
		e.RealizedUSD = r.RealizedUSD
		daySet[key] = e
	}
	for _, eq := range equity {
		key := eq.Day.Format("2006-01-02")
		e := daySet[key]
		if e.Day.IsZero() {
			e.Day = eq.Day
		}
		e.EquityUSD = eq.EquityUSD
		daySet[key] = e
	}

	// Flatten and sort by day (insertion sort — small slices, ≤30 days).
	merged := make([]tradingpnl.DailyPoint, 0, len(daySet))
	for _, v := range daySet {
		merged = append(merged, v)
	}
	for i := 1; i < len(merged); i++ {
		for j := i; j > 0 && merged[j].Day.Before(merged[j-1].Day); j-- {
			merged[j], merged[j-1] = merged[j-1], merged[j]
		}
	}
	return merged
}
