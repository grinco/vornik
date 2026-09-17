package api

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/pricing/upstream"
)

// `pricing_coverage` answers "is there an entry?" and cannot answer "is the
// entry right". Both failures it was written for were ABSENCE; a stale rate is
// invisible to every check there is, and every rate drives cost metrics,
// budget enforcement and spend attribution.
//
// Design https://docs.vornik.io,
// amendment 2026-09-17 (D5-D8).

// driftFixture writes a pricing table with explicit rates and returns a doctor
// wired to it, plus a snapshot to compare against.
func driftFixture(t *testing.T, ours map[string][2]float64) *DoctorHandlers {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("models:\n")
	for id, r := range ours {
		b.WriteString("  \"" + id + "\": { input: " + ftoa(r[0]) + ", output: " + ftoa(r[1]) + " }\n")
	}
	b.WriteString("default: { input: 1.00, output: 3.00 }\n")
	p := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return &DoctorHandlers{configDir: dir, pricingPath: p}
}

func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// A price that disagrees with the pinned snapshot beyond the threshold is a
// WARNING naming BOTH numbers (C6) — upstream is not automatically right, and
// the largest real disagreement is one where OUR number is the verified one.
func TestPricingDrift_ReportsDisagreementWithBothNumbers(t *testing.T) {
	snap := upstream.Snapshot{Models: map[string]*upstream.Entry{
		"deepseek-v4-flash": {Input: 0.300, Output: 1.200},
	}}
	h := driftFixture(t, map[string][2]float64{"deepseek-v4-flash": {0.065, 0.180}})
	got := h.checkPricingDriftAgainst(snap)

	require.Equal(t, "WARNING", got.Status)
	joined := strings.Join(got.Items, "\n")
	require.Contains(t, joined, "deepseek-v4-flash")
	require.Contains(t, joined, "0.0650", "our rate must be in the finding")
	require.Contains(t, joined, "0.3000", "upstream rate must be in the finding")
}

// C1, and the specific way THIS check could repeat the failure the design was
// written for: "3 prices drifted" reads as 3 of the whole table. It means 3 of
// the ids that could be compared at all.
func TestPricingDrift_MessageCarriesTheDenominatorItExamined(t *testing.T) {
	snap := upstream.Snapshot{Models: map[string]*upstream.Entry{
		"matched":     {Input: 1.00, Output: 3.00},
		"no-upstream": nil,
	}}
	h := driftFixture(t, map[string][2]float64{
		"matched": {1.00, 3.00}, "no-upstream": {9.99, 9.99}, "not-in-snapshot": {5, 5},
	})
	got := h.checkPricingDriftAgainst(snap)

	require.Equal(t, "OK", got.Status, "prices that agree must not warn")
	require.Contains(t, got.Message, "1 of 3",
		"the message must state how many ids were actually compared, not the table size")
	require.Contains(t, got.Message, "2",
		"the message must say how many ids it could NOT verify")
}

// D8: an available field we lack is a different operator action from a
// disagreement, and must not be laundered into the drift count.
func TestPricingDrift_EnrichmentIsReportedButIsNotDrift(t *testing.T) {
	cr := 0.035
	snap := upstream.Snapshot{Models: map[string]*upstream.Entry{
		"m": {Input: 0.35, Output: 2.75, CacheRead: &cr, MaxInputTokens: 262144},
	}}
	h := driftFixture(t, map[string][2]float64{"m": {0.35, 2.75}})
	got := h.checkPricingDriftAgainst(snap)

	require.Equal(t, "OK", got.Status, "an available cache_read is not a wrong price")
	require.Contains(t, strings.Join(got.Items, "\n"), "cache_read")
}

// A check that cannot find its inputs must SKIP, not report OK (C1/C5).
func TestPricingDrift_NoPricingPathSkipsRatherThanPasses(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkPricingDriftAgainst(upstream.Snapshot{})
	require.NotEqual(t, "OK", got.Status,
		"with no pricing table configured the check has examined nothing and must not read as OK")
}
