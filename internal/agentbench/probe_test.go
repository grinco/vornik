package agentbench

import (
	"context"
	"math"
	"testing"
)

func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

// The round-1 blind spot, pinned (§8). Every component except request precision
// reads perfect while the lead over-requested by 300%. Before request precision
// existed, this execution was indistinguishable from an ideal one.
func TestGrantProbe_OverRequestIsCaughtByRequestPrecisionAlone(t *testing.T) {
	gold := Gold{TaskID: "t1", Paths: [][]string{{"a", "b", "c", "d", "e"}}}
	trace := Trace{
		ExecutionID: "exec1",
		Requested:   []string{"a", "b", "c", "d", "e", "x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9", "x10", "x11", "x12", "x13", "x14", "x15"},
		Accepted:    []string{"a", "b", "c", "d", "e"},
		Refused:     []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9", "x10", "x11", "x12", "x13", "x14", "x15"},
		Invoked:     []string{"a", "b", "c", "d", "e"},
	}

	v, err := GrantProbe{}.Score(context.Background(), TaskRef{ID: "t1"}, gold, trace)
	if err != nil {
		t.Fatalf("score: %v", err)
	}

	approx(t, "path coverage", v.PathCoverage, 1.0)
	approx(t, "grant precision", v.GrantPrecision, 1.0)
	approx(t, "request precision", v.RequestPrecision, 0.25)
	if v.CoreMiss {
		t.Error("core miss reported when every core tool was granted")
	}
	if v.Escalations != 0 || v.Stalled {
		t.Error("escalation/stall reported on a clean execution")
	}
}

// The round-2 finding, pinned (§8). Under the INTERSECTION rule this design
// briefly used, `needed` here was empty and a policy granting nothing scored
// perfectly. Path coverage scores each demonstrated path on its own terms.
func TestGrantProbe_DisjointPathsDoNotEraseGroundTruth(t *testing.T) {
	gold := Gold{TaskID: "t2", Paths: [][]string{{"t1", "t2"}, {"t3", "t4"}}}

	covers := Trace{ExecutionID: "e1", Accepted: []string{"t1", "t2"}, Invoked: []string{"t1", "t2"}}
	v, err := GrantProbe{}.Score(context.Background(), TaskRef{ID: "t2"}, gold, covers)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	approx(t, "path coverage (covers path 1)", v.PathCoverage, 1.0)
	if v.CoreMiss {
		t.Error("core miss on disjoint paths: the core is empty, so nothing can be missed")
	}

	// The regression the intersection rule allowed: a policy that ASKS and ends
	// up with nothing useful. It must score 0, not 1.0. (An execution that never
	// asked at all is skipped instead — a different case, covered below.)
	nothing := Trace{ExecutionID: "e2", Requested: []string{"unrelated"}, Refused: []string{"unrelated"}}
	v, err = GrantProbe{}.Score(context.Background(), TaskRef{ID: "t2"}, gold, nothing)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if v.PathCoverage != 0 {
		t.Errorf("granting nothing scored %v — under the intersection rule this scored "+
			"1.0, which is the bug path coverage exists to fix", v.PathCoverage)
	}
}

// Round 3 verified this shape explicitly: max selects the CHEAPER path, which is
// correct semantics for grant adequacy. The agent demonstrated two routes and the
// grant covers one; min-coverage would answer "does the grant cover EVERY
// demonstrated route", a different and wrong question to gate on.
func TestGrantProbe_StrictSubsetPathScoresOnTheCheaperRoute(t *testing.T) {
	gold := Gold{TaskID: "t3", Paths: [][]string{{"a", "b"}, {"a", "b", "extra"}}}
	trace := Trace{ExecutionID: "e1", Accepted: []string{"a", "b"}, Invoked: []string{"a", "b"}}

	v, err := GrantProbe{}.Score(context.Background(), TaskRef{ID: "t3"}, gold, trace)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	approx(t, "path coverage", v.PathCoverage, 1.0)
	if v.CoreMiss {
		t.Error("core is {a,b} and both were granted")
	}
}

// A tool every observed path needed is a hard failure, not a fractional loss.
func TestGrantProbe_MissingACoreToolIsAHardFailure(t *testing.T) {
	gold := Gold{TaskID: "t4", Paths: [][]string{{"core", "a"}, {"core", "b"}}}
	trace := Trace{ExecutionID: "e1", Accepted: []string{"a", "b"}, Invoked: []string{"a"}}

	v, err := GrantProbe{}.Score(context.Background(), TaskRef{ID: "t4"}, gold, trace)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !v.CoreMiss {
		t.Error("granting neither path's shared tool must report a core miss")
	}
}

func TestGrantProbe_DegenerateInputs(t *testing.T) {
	ctx := context.Background()
	gold := Gold{TaskID: "t5", Paths: [][]string{{"a"}}}

	t.Run("no grant activity at all is not scored", func(t *testing.T) {
		// A step that never called grant_step_tools is not under-granting — it is
		// not using the feature. The first validated run scored six such steps at
		// 0.0 with a core miss each, dragging path coverage to zero.
		_, err := GrantProbe{}.Score(ctx, TaskRef{ID: "t5"}, gold, Trace{ExecutionID: "e"})
		if err == nil {
			t.Fatal("scored an execution that made no grant request")
		}
	})

	t.Run("granted without requesting leaves request precision undefined", func(t *testing.T) {
		v, err := GrantProbe{}.Score(ctx, TaskRef{ID: "t5"}, gold,
			Trace{ExecutionID: "e", Accepted: []string{"a"}})
		if err != nil {
			t.Fatalf("score: %v", err)
		}
		// Reporting 0.0 would read as "asked for nothing useful" and drag any
		// average down; the lead simply made no request to judge.
		if v.RequestPrecisionDefined {
			t.Errorf("request precision reported as defined with zero requests (%v)", v.RequestPrecision)
		}
	})

	t.Run("zero grants leaves grant precision undefined", func(t *testing.T) {
		v, err := GrantProbe{}.Score(ctx, TaskRef{ID: "t5"}, gold, Trace{ExecutionID: "e", Requested: []string{"a"}})
		if err != nil {
			t.Fatalf("score: %v", err)
		}
		if v.GrantPrecisionDefined {
			t.Error("grant precision reported as defined with zero grants")
		}
	})

	t.Run("escalation without a prior refusal still counts", func(t *testing.T) {
		v, err := GrantProbe{}.Score(ctx, TaskRef{ID: "t5"}, gold,
			Trace{ExecutionID: "e", Accepted: []string{"a"}, Escalations: 2})
		if err != nil {
			t.Fatalf("score: %v", err)
		}
		if v.Escalations != 2 {
			t.Errorf("escalations = %d, want 2 — an escalation is a decision signal "+
				"whether or not a refusal preceded it", v.Escalations)
		}
	})

	t.Run("an excluded task is refused, not silently scored", func(t *testing.T) {
		excluded := Gold{TaskID: "t6", Excluded: true, ExcludedReason: "unrestricted arm never passed it"}
		_, err := GrantProbe{}.Score(ctx, TaskRef{ID: "t6"}, excluded, Trace{ExecutionID: "e"})
		if err == nil {
			t.Fatal("scored a task with no ground truth — that measures the model's ceiling " +
				"and reports it as a policy result")
		}
	})

	t.Run("gold with no paths is an error, not a free pass", func(t *testing.T) {
		_, err := GrantProbe{}.Score(ctx, TaskRef{ID: "t7"}, Gold{TaskID: "t7"}, Trace{ExecutionID: "e"})
		if err == nil {
			t.Fatal("empty gold scored as if satisfied")
		}
	})
}

func TestGold_CoreAndUnion(t *testing.T) {
	g := Gold{Paths: [][]string{{"a", "b", "c"}, {"a", "c", "d"}, {"a", "c"}}}

	core := g.Core()
	if len(core) != 2 || core[0] != "a" || core[1] != "c" {
		t.Errorf("core = %v, want [a c]", core)
	}
	union := g.Union()
	if len(union) != 4 {
		t.Errorf("union = %v, want 4 entries", union)
	}
}

// Sorted output, so a verdict and a golden diff are stable across runs.
func TestGold_CoreAndUnionAreSorted(t *testing.T) {
	g := Gold{Paths: [][]string{{"z", "m", "a"}, {"z", "m", "a"}}}
	core := g.Core()
	for i := 1; i < len(core); i++ {
		if core[i-1] > core[i] {
			t.Fatalf("core is unsorted: %v — an unstable order makes every journal diff noise", core)
		}
	}
}

// Gold records what was INVOKED (bare, from tool_audit_log); grants record what
// the model ASKED FOR (whatever spelling it used). Comparing literally
// under-reports: the first validated execution had 4 of 5 gold tools granted and
// scored 2 of 5, because two carried the function-namespace prefix.
func TestGrantProbe_ComparesToolNamesInABareForm(t *testing.T) {
	gold := Gold{TaskID: "t1", Paths: [][]string{
		{"git_log", "git_status", "mcp__vornik__grant_step_tools", "read_many_files", "run_shell"},
	}}
	trace := Trace{
		ExecutionID: "e1",
		Requested:   []string{"functions.git_status", "functions.read_many_files"},
		Accepted: []string{
			"mcp__vornik__grant_step_tools", "run_shell", // bare / mcp-qualified
			"functions.git_status", "functions.read_many_files", // dotted
		},
		Invoked: []string{"git_status", "run_shell"},
	}

	v, err := GrantProbe{}.Score(context.Background(), TaskRef{ID: "t1"}, gold, trace)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	// 4 of 5: everything but git_log, regardless of how each was spelled.
	approx(t, "path coverage", v.PathCoverage, 0.8)
	if !v.CoreMiss {
		t.Error("git_log is in every path and was not granted — that is a core miss")
	}
}
