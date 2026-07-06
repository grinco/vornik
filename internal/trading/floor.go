package trading

// EntryProposal describes a proposed action for a symbol, as emitted by the
// strategist, ahead of the code-enforced floor check.
type EntryProposal struct {
	Symbol, Side, Region, RegimeLabel string
	Total, ComponentCount             int
	RegimeStale, HasScores            bool
}

// EntryConfig is the operator-controlled configuration for the floor.
type EntryConfig struct {
	MinEntryTotal      int
	BlockLongInRiskOff bool
	StaleBehavior      string
	MinComponentCount  map[string]int
	Protected          map[string]bool
}

func isOpen(side string) bool { return side == "long" || side == "short" }

// EvaluateEntry is the code-enforced floor. ok=false with a reason for a
// blocked OPEN; exits/risk-reducing (non-open) actions always pass. reason
// is one of: missing_scores, stale, panel_incomplete, below_min, risk_off_long.
//
// Staleness is fail-closed: a stale regime blocks the open unless
// StaleBehavior is EXPLICITLY one of the "don't block" opt-out values
// ("neutral" or "last_known"). Empty, unknown, or mistyped values (including
// the superficially-plausible "block_opens") all block, so a missing or
// misconfigured StaleBehavior can never silently let a stale open through.
func EvaluateEntry(p EntryProposal, cfg EntryConfig) (bool, string) {
	if !isOpen(p.Side) {
		return true, "" // only opens are gated
	}
	if !p.HasScores {
		return false, "missing_scores"
	}
	if p.RegimeStale {
		switch cfg.StaleBehavior {
		case "neutral", "last_known":
			// fall through — operator explicitly opted out of blocking on staleness
		default:
			return false, "stale" // "block_opens", "", or any unknown value → fail-closed
		}
	}
	if minCount, ok := cfg.MinComponentCount[p.Region]; ok && p.ComponentCount < minCount {
		return false, "panel_incomplete"
	}
	if p.Total < cfg.MinEntryTotal {
		return false, "below_min"
	}
	if cfg.BlockLongInRiskOff && p.Side == "long" && p.RegimeLabel == "RISK_OFF" {
		return false, "risk_off_long"
	}
	return true, ""
}
