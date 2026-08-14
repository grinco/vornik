package agentbench

import (
	"context"
	"testing"
)

func TestToolUseProbe_SeparatesInventedNamesFromBadArguments(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Calls: []ToolCall{
			{Name: "memory_search"},
			{Name: "mcp__vornik__teleport", Failed: true, ErrorText: "unknown tool: mcp__vornik__teleport"},
			{Name: "memory_search", Failed: true, ErrorText: "invalid arguments: missing required property 'query'"},
			{Name: "read_file"},
		},
	}
	v := ToolUseProbe{}.ScoreToolUse(trace, TaskRef{ID: "t1"})

	if v.UnknownTool != 1 {
		t.Errorf("unknown tool = %d, want 1", v.UnknownTool)
	}
	if v.ArgumentError != 1 {
		t.Errorf("argument error = %d, want 1 — a hallucinated name and a schema-rejected "+
			"argument set need different fixes and must not blend", v.ArgumentError)
	}
	if want := 0.5; v.CallValidity != want {
		t.Errorf("call validity = %v, want %v", v.CallValidity, want)
	}
}

// Scoring an agent on a third party's outage measures the wrong thing.
func TestToolUseProbe_DependencyOutagesLeaveTheDenominator(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Calls: []ToolCall{
			{Name: "fetch_url"},
			{Name: "fetch_url", Failed: true, ErrorText: "dial tcp: connection refused"},
		},
	}
	v := ToolUseProbe{}.ScoreToolUse(trace, TaskRef{ID: "t1"})

	if v.OtherFailure != 1 {
		t.Fatalf("other failure = %d, want 1", v.OtherFailure)
	}
	// One attributable call, and it succeeded.
	if v.CallValidity != 1.0 {
		t.Errorf("call validity = %v, want 1.0 — the agent's choice was not what failed",
			v.CallValidity)
	}
}

func TestToolUseProbe_BudgetUtilisationAndExhaustion(t *testing.T) {
	t.Run("under-provisioned", func(t *testing.T) {
		trace := Trace{
			ExecutionID:   "e1",
			ToolBudget:    10,
			ToolCallsUsed: 10,
			Outcomes:      []StepOutcome{{StepID: "s1", Outcome: OutcomeIterationExhausted, Attempt: 1}},
		}
		v := ToolUseProbe{}.ScoreToolUse(trace, TaskRef{ID: "t1"})
		if v.BudgetUtilisation != 1.0 || !v.BudgetExhausted {
			t.Errorf("utilisation=%v exhausted=%v, want 1.0 and true",
				v.BudgetUtilisation, v.BudgetExhausted)
		}
	})

	t.Run("over-provisioned", func(t *testing.T) {
		trace := Trace{ExecutionID: "e1", ToolBudget: 40, ToolCallsUsed: 3}
		v := ToolUseProbe{}.ScoreToolUse(trace, TaskRef{ID: "t1"})
		if want := 0.075; v.BudgetUtilisation != want {
			t.Errorf("utilisation = %v, want %v — budget nobody needed is also a finding",
				v.BudgetUtilisation, want)
		}
		if v.BudgetExhausted {
			t.Error("reported exhausted at 7.5% utilisation")
		}
	})

	t.Run("no budget recorded leaves it undefined", func(t *testing.T) {
		v := ToolUseProbe{}.ScoreToolUse(Trace{ExecutionID: "e1", ToolCallsUsed: 5}, TaskRef{ID: "t1"})
		if v.BudgetUtilisationDefined {
			t.Error("utilisation defined with no budget — that is a division by an " +
				"unrecorded number, not a measurement")
		}
	})
}

func TestToolUseProbe_CountsRepeatedCalls(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Calls: []ToolCall{
			{Name: "list_files"}, {Name: "list_files"}, {Name: "list_files"},
			{Name: "read_file"},
		},
	}
	v := ToolUseProbe{}.ScoreToolUse(trace, TaskRef{ID: "t1"})
	if v.RepeatedCalls != 2 {
		t.Errorf("repeated calls = %d, want 2 — the cheapest signal for a degenerate "+
			"loop, and a direct token cost", v.RepeatedCalls)
	}
}

func TestToolUseProbe_NoCallsLeavesValidityUndefined(t *testing.T) {
	v := ToolUseProbe{}.ScoreToolUse(Trace{ExecutionID: "e1"}, TaskRef{ID: "t1"})
	if v.CallValidityDefined {
		t.Error("validity defined with no calls — an agent that called nothing has " +
			"not called anything badly")
	}
}

func TestToolUseProbe_ToolsByFailureCountRanksWorstFirst(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Calls: []ToolCall{
			{Name: "a", Failed: true, ErrorText: "invalid arguments"},
			{Name: "b", Failed: true, ErrorText: "unknown tool"},
			{Name: "b", Failed: true, ErrorText: "unknown tool"},
			{Name: "c", Failed: true, ErrorText: "connection refused"},
		},
	}
	v := ToolUseProbe{}.ScoreToolUse(trace, TaskRef{ID: "t1"})
	got := v.ToolsByFailureCount(trace)
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("ranking = %v, want [b a] — c failed on a dependency, not on the "+
			"agent's choice", got)
	}
}

// Both new probes satisfy the shared interface, which is what validates it —
// the design said the seam would be proven by the probes that land, not by
// argument. Three now do.
func TestAllProbes_SatisfyTheInterface(t *testing.T) {
	probes := []Probe{GrantProbe{}, SchemaProbe{}, ToolUseProbe{}}
	names := map[string]bool{}
	for _, p := range probes {
		if p.Name() == "" {
			t.Error("a probe reported no name")
		}
		if names[p.Name()] {
			t.Errorf("duplicate probe name %q — the journal keys on it", p.Name())
		}
		names[p.Name()] = true
	}

	// The two gold-free probes must not demand gold, or the cheap gate becomes
	// as expensive as the expensive one.
	ctx := context.Background()
	for _, p := range []Probe{SchemaProbe{}, ToolUseProbe{}} {
		if _, err := p.Score(ctx, TaskRef{ID: "t"}, Gold{}, Trace{ExecutionID: "e"}); err != nil {
			t.Errorf("%s demanded gold it does not need: %v", p.Name(), err)
		}
	}
}

// Call-count weighting answers "what fraction of CALLS conformed". It cannot
// answer "which role is broken": a chatty role at 95% and a role making one
// always-wrong call aggregate to ~95%, and the second role's total failure is
// invisible. Per-role validity is the metric that catches it.
func TestToolUseProbe_PerRoleValidityCatchesASingleBrokenRole(t *testing.T) {
	var calls []ToolCall
	for i := 0; i < 19; i++ {
		calls = append(calls, ToolCall{Name: "search", Role: "researcher"})
	}
	calls = append(calls,
		ToolCall{Name: "search", Role: "researcher", Failed: true, ErrorText: "invalid arguments"},
		ToolCall{Name: "made_up", Role: "reviewer", Failed: true, ErrorText: "unknown tool: made_up"},
	)

	v := ToolUseProbe{}.ScoreToolUse(Trace{ExecutionID: "e1", Calls: calls}, TaskRef{ID: "t1"})

	// The aggregate looks healthy.
	if v.CallValidity < 0.9 {
		t.Fatalf("aggregate validity = %v, expected the chatty role to dominate it", v.CallValidity)
	}
	// The broken role does not.
	reviewer := v.ByRole["reviewer"]
	if !reviewer.ValidityValid || reviewer.Validity != 0 {
		t.Errorf("reviewer validity = %v (defined %v), want 0 — one role calling a "+
			"nonexistent tool every time must not hide behind a chatty healthy one",
			reviewer.Validity, reviewer.ValidityValid)
	}
	if researcher := v.ByRole["researcher"]; researcher.Validity != 19.0/20.0 {
		t.Errorf("researcher validity = %v, want 0.95", researcher.Validity)
	}
}

// A role whose only failures were dependency outages has nothing attributable,
// so its validity is unmeasured rather than zero.
func TestToolUseProbe_PerRoleValidityUndefinedWhenOnlyOutages(t *testing.T) {
	v := ToolUseProbe{}.ScoreToolUse(Trace{
		ExecutionID: "e1",
		Calls: []ToolCall{
			{Name: "fetch", Role: "scraper", Failed: true, ErrorText: "connection refused"},
		},
	}, TaskRef{ID: "t1"})

	if rc := v.ByRole["scraper"]; rc.ValidityValid {
		t.Errorf("scraper validity reported as %v — the agent's choice was never tested",
			rc.Validity)
	}
}
