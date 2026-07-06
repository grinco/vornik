// Package trading holds deterministic trading-strategy domain logic:
// the decision cascade and the entry floor. Pure, no I/O, no LLM.
//
// ROADMAP STATUS: the decision cascade (Classify/CascadeAction below)
// is built for roadmap use and is NOT wired into v1 enforcement. The
// live gate is the verifier/scorecard_floor verifier, which keys off
// the strategist proposal's authoritative intent/action fields
// (open|close, BUY|SELL) — never off CascadeAction. Classify can
// return a non-exit action for a close proposal, so using it as the
// protected-symbol EXIT guard would be a bug; see evaluateFloorProposal
// in internal/verifier/scorecard_floor.go. Classify remains a tested,
// ready-to-wire primitive for when the roadmap calls for holding-
// state-aware decisions in enforcement.
package trading

// CascadeAction is one of the fixed position-lifecycle states the strategist's
// scores map to, authoritative decision output from Classify.
type CascadeAction string

// CascadeAction values enumerate position-lifecycle decisions.
const (
	ActionExit            CascadeAction = "EXIT"
	ActionExitTrim        CascadeAction = "EXIT_TRIM"
	ActionHoldRide        CascadeAction = "HOLD_RIDE"
	ActionHoldReview      CascadeAction = "HOLD_REVIEW"
	ActionReEntry         CascadeAction = "RE_ENTRY"
	ActionTacticalRebound CascadeAction = "TACTICAL_REBOUND"
	ActionWait            CascadeAction = "WAIT"
	ActionStayOut         CascadeAction = "STAY_OUT"
	ActionObserve         CascadeAction = "OBSERVE"
)

// CascadeInput holds the strategist's score input and regime state for the
// decision cascade.
type CascadeInput struct {
	HoldingState string // "held" | "flat"
	Total        int
	Trend        int
	Momentum     int
	RegimeLabel  string // RISK_ON | NEUTRAL | RISK_OFF
	Stale        bool
}

// Classify maps scores + holding state to the authoritative action. The
// strategist emits the inputs + a narrative; this function decides.
func Classify(in CascadeInput) CascadeAction {
	held := in.HoldingState == "held"
	if in.Stale {
		if held {
			return ActionHoldReview // don't add/exit on stale data; watch it
		}
		return ActionStayOut
	}
	if held {
		switch {
		case in.RegimeLabel == "RISK_OFF" && in.Total < 0:
			return ActionExitTrim
		case in.Total <= -3:
			return ActionExit
		case in.Total >= 3:
			return ActionHoldRide
		default:
			return ActionHoldReview
		}
	}
	// flat
	switch {
	case in.Total >= 3 && in.RegimeLabel != "RISK_OFF":
		return ActionReEntry
	case in.Total >= 1 && in.RegimeLabel == "RISK_OFF":
		return ActionTacticalRebound
	case in.Total <= -1:
		return ActionStayOut
	case in.Total >= 1:
		return ActionWait
	default:
		return ActionObserve
	}
}
