package trading

import "testing"

func cfg() EntryConfig {
	return EntryConfig{MinEntryTotal: 3, BlockLongInRiskOff: true, StaleBehavior: "block_opens",
		MinComponentCount: map[string]int{"us": 6, "apac": 4}, Protected: map[string]bool{"LOCK": true}}
}

func TestEvaluateEntry(t *testing.T) {
	ok, r := EvaluateEntry(EntryProposal{Symbol: "AAPL", Side: "long", Total: 4, RegimeLabel: "RISK_ON",
		ComponentCount: 6, HasScores: true, Region: "us"}, cfg())
	if !ok {
		t.Fatalf("expected pass, got %q", r)
	}

	if ok, r := EvaluateEntry(EntryProposal{HasScores: false, Side: "long"}, cfg()); ok || r != "missing_scores" {
		t.Fatalf("missing scores must fail-closed, got ok=%v r=%q", ok, r)
	}
	if _, r := EvaluateEntry(EntryProposal{Side: "long", Total: 4, RegimeStale: true, HasScores: true, Region: "us", ComponentCount: 6}, cfg()); r != "stale" {
		t.Fatalf("stale must block, got %q", r)
	}
	if _, r := EvaluateEntry(EntryProposal{Side: "long", Total: 2, RegimeLabel: "RISK_ON", HasScores: true, Region: "us", ComponentCount: 6}, cfg()); r != "below_min" {
		t.Fatalf("below floor must block, got %q", r)
	}
	if _, r := EvaluateEntry(EntryProposal{Side: "long", Total: 4, RegimeLabel: "RISK_OFF", HasScores: true, Region: "us", ComponentCount: 6}, cfg()); r != "risk_off_long" {
		t.Fatalf("long into risk-off must block, got %q", r)
	}
	if _, r := EvaluateEntry(EntryProposal{Side: "long", Total: 4, RegimeLabel: "RISK_ON", HasScores: true, Region: "apac", ComponentCount: 3}, cfg()); r != "panel_incomplete" {
		t.Fatalf("incomplete panel must block, got %q", r)
	}
}

// TestEvaluateEntry_ExitNeverBlocked pins that non-open sides always pass,
// regardless of how bad the inputs look.
func TestEvaluateEntry_ExitNeverBlocked(t *testing.T) {
	ok, r := EvaluateEntry(EntryProposal{Side: "exit", Total: -10, RegimeStale: true, HasScores: false,
		RegimeLabel: "RISK_OFF", Region: "us", ComponentCount: 0}, cfg())
	if !ok || r != "" {
		t.Fatalf("exit must never be blocked, got ok=%v r=%q", ok, r)
	}
}

// TestEvaluateEntry_StaleFailClosed pins the deviation from the brief: a
// stale regime blocks the open unless StaleBehavior is EXPLICITLY an
// opt-out value ("neutral" or "last_known"). Empty string, the brief's own
// "block_opens", and any typo/unknown value must all block.
func TestEvaluateEntry_StaleFailClosed(t *testing.T) {
	base := EntryProposal{Side: "long", Total: 4, RegimeLabel: "RISK_ON", HasScores: true,
		Region: "us", ComponentCount: 6, RegimeStale: true}

	for _, staleBehavior := range []string{"", "block_open", "block_opens", "bogus", "NEUTRAL"} {
		c := cfg()
		c.StaleBehavior = staleBehavior
		if ok, r := EvaluateEntry(base, c); ok || r != "stale" {
			t.Fatalf("StaleBehavior=%q must fail-closed to stale, got ok=%v r=%q", staleBehavior, ok, r)
		}
	}

	for _, staleBehavior := range []string{"neutral", "last_known"} {
		c := cfg()
		c.StaleBehavior = staleBehavior
		if ok, r := EvaluateEntry(base, c); !ok || r != "" {
			t.Fatalf("StaleBehavior=%q must opt out of stale block, got ok=%v r=%q", staleBehavior, ok, r)
		}
	}
}

// TestEvaluateEntry_MissingScoresFailClosed pins that missing scores block
// even when other fields look otherwise valid, and take priority over
// staleness/panel/floor/regime checks (checked first).
func TestEvaluateEntry_MissingScoresFailClosed(t *testing.T) {
	p := EntryProposal{Side: "long", Total: 10, RegimeLabel: "RISK_ON", HasScores: false,
		Region: "us", ComponentCount: 6, RegimeStale: true}
	if ok, r := EvaluateEntry(p, cfg()); ok || r != "missing_scores" {
		t.Fatalf("missing scores must fail-closed first, got ok=%v r=%q", ok, r)
	}
}
