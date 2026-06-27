// Coverage for driftInsight — the pure function that derives the
// operator-facing recommendation string for the effective-cost
// drift panel. Pure data-in / data-out; no DB, no HTTP. The
// upstream caller pairs it with each drift row when rendering the
// per-project page.

package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/budget"
)

// TestDriftInsight_NoBaselineReturnsEmpty pins the early-return:
// without a baseline the function has no anchor to compare against,
// so it returns "" and the panel renders no recommendation row.
func TestDriftInsight_NoBaselineReturnsEmpty(t *testing.T) {
	got := driftInsight(budget.DriftRow{HasBaseline: false, Ratio: 99.0})
	if got != "" {
		t.Errorf("no baseline: got %q, want empty", got)
	}
}

// TestDriftInsight_BoringBandReturnsEmpty — when the ratio is in
// the 0.85..1.5× band the project is operating within normal
// variation; no row should render.
func TestDriftInsight_BoringBandReturnsEmpty(t *testing.T) {
	for _, ratio := range []float64{0.85, 1.0, 1.2, 1.49} {
		got := driftInsight(budget.DriftRow{HasBaseline: true, Ratio: ratio})
		if got != "" {
			t.Errorf("ratio %.2f: got %q, want empty (boring band)", ratio, got)
		}
	}
}

// TestDriftInsight_RegressionSuccessesDropped — primary cause is
// success-rate drop (under 60% of baseline). Insight should
// mention "successes dropped" so an operator knows to look at
// failures, not spend.
func TestDriftInsight_RegressionSuccessesDropped(t *testing.T) {
	got := driftInsight(budget.DriftRow{
		HasBaseline:      true,
		Ratio:            2.5,
		CurrentSpendUSD:  10,
		BaselineSpendUSD: 70, // 10/day baseline
		CurrentOks:       2,
		BaselineOks:      70, // 10/day baseline; 2 vs 10 = 20%
	})
	if !strings.Contains(got, "successes dropped") {
		t.Errorf("got %q, want mention of successes dropped", got)
	}
	if !strings.Contains(got, "investigate failures") {
		t.Errorf("got %q, want guidance toward failure investigation", got)
	}
}

// TestDriftInsight_RegressionSpendUp — primary cause is spend
// up >1.5× baseline. Insight should mention spend/prompt-bloat.
func TestDriftInsight_RegressionSpendUp(t *testing.T) {
	got := driftInsight(budget.DriftRow{
		HasBaseline:      true,
		Ratio:            2.5,
		CurrentSpendUSD:  20,
		BaselineSpendUSD: 70, // 10/day baseline; 20 is 2x
		CurrentOks:       10,
		BaselineOks:      70, // 10/day baseline; on par
	})
	if !strings.Contains(got, "spend at") {
		t.Errorf("got %q, want mention of spend at <ratio>× baseline", got)
	}
}

// TestDriftInsight_RegressionGenericMessage — when neither
// success-rate-drop nor spend-up condition fires but the ratio
// is still >2×, the function returns the generic "up X×" line.
func TestDriftInsight_RegressionGenericMessage(t *testing.T) {
	got := driftInsight(budget.DriftRow{
		HasBaseline:      true,
		Ratio:            2.2,
		CurrentSpendUSD:  5,
		BaselineSpendUSD: 7, // 1/day; current spend lower
		CurrentOks:       7,
		BaselineOks:      7, // 1/day; on par
	})
	if !strings.Contains(got, "Cost/success up") {
		t.Errorf("got %q, want generic regression message", got)
	}
}

// TestDriftInsight_MidRangeWarning — 1.5× < ratio ≤ 2.0× returns
// a "watch this combo" warning instead of an investigation
// recommendation. The threshold matters because the panel uses
// the insight text to colour the row.
func TestDriftInsight_MidRangeWarning(t *testing.T) {
	got := driftInsight(budget.DriftRow{HasBaseline: true, Ratio: 1.7})
	if !strings.Contains(got, "watch this combo") {
		t.Errorf("got %q, want 'watch this combo' warning", got)
	}
}

// TestDriftInsight_Improvement — ratio < 0.7 reports the combo
// is becoming MORE efficient. Operators reading the panel see a
// celebratory line instead of a missing entry.
func TestDriftInsight_Improvement(t *testing.T) {
	got := driftInsight(budget.DriftRow{HasBaseline: true, Ratio: 0.5})
	if !strings.Contains(got, "becoming more efficient") {
		t.Errorf("got %q, want improvement message", got)
	}
}
