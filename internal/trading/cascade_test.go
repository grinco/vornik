package trading

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   CascadeInput
		want CascadeAction
	}{
		// --- held: EXIT_TRIM takes priority over EXIT when RISK_OFF && Total<0 ---
		{"held/risk_off/negative -> exit_trim", CascadeInput{HoldingState: "held", Total: -2, RegimeLabel: "RISK_OFF"}, ActionExitTrim},
		{"held/risk_off/total=-1 -> exit_trim", CascadeInput{HoldingState: "held", Total: -1, RegimeLabel: "RISK_OFF"}, ActionExitTrim},

		// --- held: EXIT at exact boundary Total<=-3 (RegimeLabel must not be RISK_OFF, else exit_trim wins) ---
		{"held/neutral/total=-3 (exit boundary) -> exit", CascadeInput{HoldingState: "held", Total: -3, RegimeLabel: "NEUTRAL"}, ActionExit},
		{"held/neutral/total=-4 -> exit", CascadeInput{HoldingState: "held", Total: -4, RegimeLabel: "NEUTRAL"}, ActionExit},
		// one above the exit boundary must NOT exit (pins <=-3, catches <=-2 regressions)
		{"held/neutral/total=-2 (above exit boundary) -> hold_review", CascadeInput{HoldingState: "held", Total: -2, RegimeLabel: "NEUTRAL"}, ActionHoldReview},

		// --- held: HOLD_RIDE at exact boundary Total>=3 ---
		{"held/neutral/total=3 (hold_ride boundary) -> hold_ride", CascadeInput{HoldingState: "held", Total: 3, RegimeLabel: "NEUTRAL"}, ActionHoldRide},
		{"held/neutral/total=5 -> hold_ride", CascadeInput{HoldingState: "held", Total: 5, RegimeLabel: "NEUTRAL"}, ActionHoldRide},
		// one below the hold_ride boundary must NOT ride (pins >=3, catches >=2 regressions)
		{"held/neutral/total=2 (below hold_ride boundary) -> hold_review", CascadeInput{HoldingState: "held", Total: 2, RegimeLabel: "NEUTRAL"}, ActionHoldReview},

		// --- held: HOLD_REVIEW default (mid-range) ---
		{"held/neutral/total=1 (mid range) -> hold_review", CascadeInput{HoldingState: "held", Total: 1, RegimeLabel: "NEUTRAL"}, ActionHoldReview},
		// --- held + stale: HOLD_REVIEW (stale short-circuits before the held switch) ---
		{"held/stale -> hold_review", CascadeInput{HoldingState: "held", Total: 3, RegimeLabel: "RISK_ON", Stale: true}, ActionHoldReview},

		// --- flat: RE_ENTRY at exact boundary Total>=3, not RISK_OFF ---
		{"flat/risk_on/total=3 (re_entry boundary) -> re_entry", CascadeInput{HoldingState: "flat", Total: 3, RegimeLabel: "RISK_ON"}, ActionReEntry},
		{"flat/risk_on/total=4 -> re_entry", CascadeInput{HoldingState: "flat", Total: 4, RegimeLabel: "RISK_ON"}, ActionReEntry},
		// one below the re_entry boundary must NOT re-enter (pins >=3, catches >=2 regressions)
		{"flat/risk_on/total=2 (below re_entry boundary) -> wait", CascadeInput{HoldingState: "flat", Total: 2, RegimeLabel: "RISK_ON"}, ActionWait},

		// --- flat: TACTICAL_REBOUND vs WAIT at the same Total, differing only by regime ---
		{"flat/risk_off/total=1 -> tactical_rebound", CascadeInput{HoldingState: "flat", Total: 1, RegimeLabel: "RISK_OFF"}, ActionTacticalRebound},
		{"flat/neutral/total=1 -> wait", CascadeInput{HoldingState: "flat", Total: 1, RegimeLabel: "NEUTRAL"}, ActionWait},
		// RISK_OFF + Total>=3 still re-enters as tactical_rebound, never RE_ENTRY, since RE_ENTRY excludes RISK_OFF
		{"flat/risk_off/total=3 -> tactical_rebound (not re_entry)", CascadeInput{HoldingState: "flat", Total: 3, RegimeLabel: "RISK_OFF"}, ActionTacticalRebound},

		// --- flat: STAY_OUT at exact boundary Total<=-1 ---
		{"flat/neutral/total=-1 (stay_out boundary) -> stay_out", CascadeInput{HoldingState: "flat", Total: -1, RegimeLabel: "NEUTRAL"}, ActionStayOut},
		{"flat/neutral/total=-3 -> stay_out", CascadeInput{HoldingState: "flat", Total: -3, RegimeLabel: "NEUTRAL"}, ActionStayOut},
		// --- flat + stale: STAY_OUT (stale short-circuits before the flat switch) ---
		{"flat/stale -> stay_out", CascadeInput{HoldingState: "flat", Total: 3, RegimeLabel: "RISK_ON", Stale: true}, ActionStayOut},

		// --- flat: OBSERVE default, one above the stay_out boundary ---
		{"flat/neutral/total=0 (above stay_out boundary) -> observe", CascadeInput{HoldingState: "flat", Total: 0, RegimeLabel: "NEUTRAL"}, ActionObserve},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.in); got != c.want {
				t.Errorf("Classify(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
