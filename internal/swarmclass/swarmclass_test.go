package swarmclass

import "testing"

func TestIsTrading(t *testing.T) {
	cases := []struct {
		swarmID string
		want    bool
		why     string
	}{
		{"ibkr-trader", true, "the reference deployment's live trading swarm"},
		{"trading-research", true, "reads trading data even if it places nothing"},
		{"broker-ops", true, "broker path"},
		{"IBKR-TRADER", true, "case-insensitive: an operator's capitalisation is not a safety boundary"},
		{"Trading", true, "case-insensitive"},
		{"bench", false, "the benchmark swarm must remain scannable"},
		{"companion", false, ""},
		{"deep-research", false, "research alone is not trading"},
		{"", false, "empty is unknown, not trading — see IsTrading's doc comment"},
	}
	for _, c := range cases {
		if got := IsTrading(c.swarmID); got != c.want {
			t.Errorf("IsTrading(%q) = %v, want %v (%s)", c.swarmID, got, c.want, c.why)
		}
	}
}

// The markers are a safety boundary, so shrinking the set is a decision that
// should require editing this test rather than happening as a side effect.
func TestTradingMarkers_AreNotSilentlyNarrowed(t *testing.T) {
	want := map[string]bool{"trader": true, "broker": true, "trading": true}
	if len(tradingMarkers) != len(want) {
		t.Fatalf("marker set changed: %v — trading exclusion is shared by the cost/quality "+
			"detector, the applier and the benchmark guard, so narrowing it widens what "+
			"three automated systems may touch", tradingMarkers)
	}
	for _, m := range tradingMarkers {
		if !want[m] {
			t.Errorf("unexpected marker %q", m)
		}
	}
}
