package taintlineage

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"", ModeAdvisory, true},
		{"advisory", ModeAdvisory, true},
		{"ADVISORY", ModeAdvisory, true},
		{" off ", ModeOff, true},
		{"enforce", ModeEnforce, true},
		{"bogus", ModeAdvisory, false},
		{"true", ModeAdvisory, false},
	}
	for _, tc := range cases {
		m, ok := NormalizeMode(tc.in)
		if m != tc.want || ok != tc.ok {
			t.Fatalf("NormalizeMode(%q) = (%v,%v), want (%v,%v)", tc.in, m, ok, tc.want, tc.ok)
		}
	}
}

func TestEffectiveMode_OverrideWins(t *testing.T) {
	if got := EffectiveMode("enforce", "advisory"); got != ModeEnforce {
		t.Fatalf("project override should win: got %v", got)
	}
	if got := EffectiveMode("", "enforce"); got != ModeEnforce {
		t.Fatalf("daemon default should apply when no override: got %v", got)
	}
	if got := EffectiveMode("garbage", "off"); got != ModeAdvisory {
		t.Fatalf("invalid override coerces to advisory (fail-safe): got %v", got)
	}
}

func rollHigh(complete bool) TaskTaint {
	return Rollup([]StepTaint{{
		Used: true, MaxSeverity: SeverityHigh, RequiresReview: true,
		Sources: []Source{{Tool: "web_fetch", Ref: "https://a.example", Severity: SeverityHigh}},
	}}, nil, complete)
}

func TestDecide_Advisory_FlagNoPark(t *testing.T) {
	d := Decide(ModeAdvisory, rollHigh(true), nil)
	if d.Park {
		t.Fatalf("advisory must never park")
	}
	if !d.Tainted || !d.RequiresReview {
		t.Fatalf("advisory should still flag High")
	}
}

func TestDecide_Enforce_HighParks(t *testing.T) {
	d := Decide(ModeEnforce, rollHigh(true), nil)
	if !d.Park {
		t.Fatalf("enforce + High + no latch must park")
	}
}

func TestDecide_Enforce_UnknownParks(t *testing.T) {
	roll := Rollup([]StepTaint{{Used: true, MaxSeverity: SeverityUnknown, Sources: []Source{{Tool: "weird", Ref: "weird", Severity: SeverityUnknown}}}}, nil, true)
	d := Decide(ModeEnforce, roll, nil)
	if !d.Park {
		t.Fatalf("enforce + Unknown must park (D8)")
	}
}

func TestDecide_Enforce_LowOnly_NoPark(t *testing.T) {
	roll := Rollup([]StepTaint{{Used: true, MaxSeverity: SeverityLow, Sources: []Source{{Tool: "memory_search", Ref: "q", Severity: SeverityLow}}}}, nil, true)
	d := Decide(ModeEnforce, roll, nil)
	if d.Park {
		t.Fatalf("enforce + Low-only must NOT park")
	}
}

// D6: an incomplete walk parks in enforce even with no tainted content.
func TestDecide_Enforce_IncompleteWalk_FailsClosed(t *testing.T) {
	roll := Rollup(nil, nil, false) // no taint, walk incomplete
	d := Decide(ModeEnforce, roll, nil)
	if !d.Park {
		t.Fatalf("enforce + incomplete walk must park (D6 fail-closed)")
	}
}

// D7 latch: same-source complete-walk re-run with a matching latch does NOT
// re-park; a new source (different hash) DOES re-park.
func TestDecide_Enforce_Latch(t *testing.T) {
	roll := rollHigh(true)
	matching := []string{roll.SourceSetHash}
	if Decide(ModeEnforce, roll, matching).Park {
		t.Fatalf("matching latch (complete walk) must suppress the content-driven park (D7)")
	}
	// A re-run with a different source set → different hash → re-parks.
	roll2 := Rollup([]StepTaint{{Used: true, MaxSeverity: SeverityHigh, RequiresReview: true,
		Sources: []Source{{Tool: "web_fetch", Ref: "https://NEW.example", Severity: SeverityHigh}}}}, nil, true)
	if !Decide(ModeEnforce, roll2, matching).Park {
		t.Fatalf("a new/changed source (different hash) must re-park (D7/C1)")
	}
}

// F1: an incomplete-walk re-run WITH a matching latch STILL parks — the latch
// never suppresses walkReview.
func TestDecide_Enforce_IncompleteWalk_MatchingLatch_StillParks(t *testing.T) {
	complete := rollHigh(true)
	latch := []string{complete.SourceSetHash}
	incomplete := rollHigh(false) // same sources, walk now incomplete
	if incomplete.SourceSetHash != complete.SourceSetHash {
		t.Fatalf("precondition: same source set → same hash")
	}
	d := Decide(ModeEnforce, incomplete, latch)
	if !d.Park {
		t.Fatalf("incomplete walk must re-park despite a matching latch (F1)")
	}
}

func TestDecide_Off_NoPark(t *testing.T) {
	d := Decide(ModeOff, rollHigh(false), nil)
	if d.Park {
		t.Fatalf("off must never park")
	}
}
