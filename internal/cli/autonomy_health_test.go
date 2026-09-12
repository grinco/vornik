package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRenderAutonomyHealth_UndeclaredFeedsPrintsNotDeclared pins the
// honesty rule from https://docs.vornik.io
// detection-design.md §6: a project that declares no feeds must never
// render a blank or "OK" that a caller could mistake for a measured,
// healthy cadence.
func TestRenderAutonomyHealth_UndeclaredFeedsPrintsNotDeclared(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{FeedsDeclared: false})
	if !strings.Contains(out, "not declared") {
		t.Errorf("undeclared feeds must print 'not declared', got:\n%s", out)
	}
	if strings.Contains(out, "OK") {
		t.Errorf("undeclared feeds must never print OK, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_NoVerdictsPrintsNoVerdicts pins the judge half
// of the same honesty rule: an empty verdict window must never render as
// a fabricated 0% failure rate.
func TestRenderAutonomyHealth_NoVerdictsPrintsNoVerdicts(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{Judge: judgeBlock{Declared: false}})
	if !strings.Contains(out, "no verdicts") {
		t.Errorf("empty verdict window must print 'no verdicts', got:\n%s", out)
	}
	if strings.Contains(out, "0%") || strings.Contains(out, "0.0%") {
		t.Errorf("empty verdict window must never print a fabricated failure rate, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_RouteChurnMissingKeysRenderNotMeasured pins the
// non-uniform routeChurn shape Task 5 shipped deliberately: the nil-repo
// path renders an empty map, and a missing key must read as "not
// measured", never as a silent zero.
func TestRenderAutonomyHealth_RouteChurnMissingKeysRenderNotMeasured(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{RouteChurn: map[string]any{}})
	for _, label := range []string{
		"tasks observed", "tasks with execution", "executions per task",
		"max executions for one task", "orphaned step outcomes",
	} {
		idx := strings.Index(out, label)
		if idx == -1 {
			t.Fatalf("expected route churn line for %q, got:\n%s", label, out)
		}
		line := out[idx : idx+strings.Index(out[idx:], "\n")]
		if !strings.Contains(line, "not measured") {
			t.Errorf("missing routeChurn key %q must render 'not measured', got line:\n%s", label, line)
		}
	}
}

// TestRenderAutonomyHealth_RouteChurnPopulatedRendersValues is the other
// half of the non-uniform shape: when the five named keys are present,
// their values render, not "not measured".
func TestRenderAutonomyHealth_RouteChurnPopulatedRendersValues(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{RouteChurn: map[string]any{
		"tasksObserved":        float64(40),
		"tasksWithExecution":   float64(40),
		"executionsPerTask":    float64(4),
		"maxExecutionsForTask": float64(4),
		"orphanedStepOutcomes": float64(2),
	}})
	if strings.Contains(out, "not measured") {
		t.Errorf("populated routeChurn must not render 'not measured', got:\n%s", out)
	}
	if !strings.Contains(out, "40") || !strings.Contains(out, "orphaned step outcomes") {
		t.Errorf("populated routeChurn must render its values, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_MonotonyAbsentDoesNotPrintFalse pins that
// "monotony" is present only when it fires; absence is the normal state
// and must never render as "monotony: false".
func TestRenderAutonomyHealth_MonotonyAbsentDoesNotPrintFalse(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{Outcomes: healthOutcomes{
		Counts: map[string]int64{"CREATED": 3}, Total: 3,
	}})
	if strings.Contains(strings.ToLower(out), "monotony: false") || strings.Contains(strings.ToLower(out), "monotony false") {
		t.Errorf("absent monotony must never render as false, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_MonotonyPresentIsFlagged is the positive case:
// when the endpoint set monotony:true, the render must surface it.
func TestRenderAutonomyHealth_MonotonyPresentIsFlagged(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{Outcomes: healthOutcomes{
		Counts: map[string]int64{"CREATED": 30}, Total: 30, Monotony: true,
	}})
	if !strings.Contains(strings.ToLower(out), "monotony") {
		t.Errorf("monotony:true must be surfaced, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_NeverRanDistinctFromZeroLag pins that a feed
// with no evidence at all (neverRan) must never collapse into the same
// text as a feed that ran and has a 0s lag -- they are not the same fact.
func TestRenderAutonomyHealth_NeverRanDistinctFromZeroLag(t *testing.T) {
	never := renderAutonomyHealth(healthPayload{
		FeedsDeclared: true,
		Feeds:         []healthFeedRow{{Slug: "czech-news", NeverRan: true, LagSeconds: 0}},
	})
	if !strings.Contains(never, "never ran") {
		t.Errorf("neverRan feed must print 'never ran', got:\n%s", never)
	}

	ran := renderAutonomyHealth(healthPayload{
		FeedsDeclared: true,
		Feeds:         []healthFeedRow{{Slug: "czech-news", NeverRan: false, LagSeconds: 0}},
	})
	if strings.Contains(ran, "never ran") {
		t.Errorf("a feed that ran with 0s lag must not print 'never ran', got:\n%s", ran)
	}
}

// TestRenderAutonomyHealth_UnresolvedSaysLookupsNotTicks pins the
// unresolved-count caveat: it counts failed LOOKUPS, not ticks. One tick
// can fail two lookups (a verdict lookup and an execution lookup, on the
// same already-resolved task -- see tallyDelivery in
// internal/api/autonomy_handlers.go:320-346) and count twice here, so the
// number cannot support a claim like "N ticks were unresolvable".
//
// This asserts the PROPERTY -- no tick-flavoured word anywhere in the
// rendered unresolved line -- rather than enumerating specific forbidden
// phrasings. Enumerating phrasings is exactly the failure class this
// whole change is about: it lets a rephrase that keeps the forbidden
// MEANING slip past a check that only matches the old WORDING. A prior
// version of this test only checked for the literal strings "3 ticks" and
// "unresolved ticks", which passed against
// "(examinable ticks the endpoint could not resolve)" -- a phrasing that
// still reads as a tick count to an operator.
func TestRenderAutonomyHealth_UnresolvedSaysLookupsNotTicks(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{
		Delivery: healthDelivery{Unresolved: 3},
	})
	if !strings.Contains(out, "unresolved lookups") {
		t.Errorf("unresolved must be labelled as lookups, got:\n%s", out)
	}

	line := unresolvedLine(t, out)
	if strings.Contains(strings.ToLower(line), "tick") {
		t.Errorf("the unresolved line must not contain any tick-flavoured word (label or parenthetical), got line:\n%s", line)
	}
}

// unresolvedLine extracts the single rendered "unresolved lookups: ..."
// line from renderAutonomyHealth's output, so callers can assert
// properties of that line specifically rather than the whole table.
func unresolvedLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "unresolved lookups") {
			return l
		}
	}
	t.Fatalf("no 'unresolved lookups' line found in output:\n%s", out)
	return ""
}

// TestRenderAutonomyHealth_NoDelegationDistinctFromDelegated pins that a
// CREATED tick whose task concluded without ever creating a delegated
// child renders distinctly -- otherwise it hides inside the same
// CREATED->COMPLETED shape as a healthy delegating tick (design §6).
func TestRenderAutonomyHealth_NoDelegationDistinctFromDelegated(t *testing.T) {
	child := "task_20260910_child1"
	out := renderAutonomyHealth(healthPayload{
		Delivery: healthDelivery{Rows: []healthDeliveryRow{
			{TaskID: "task_20260910_parent1", Status: "COMPLETED", NoDelegation: true},
			{TaskID: "task_20260910_parent2", ChildTaskID: &child, Status: "COMPLETED", NoDelegation: false},
		}},
	})
	if !strings.Contains(out, "no delegation") {
		t.Errorf("a non-delegating tick must be marked 'no delegation', got:\n%s", out)
	}
	if !strings.Contains(out, "child1") {
		t.Errorf("a delegating tick must show the child task, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_JudgeDeclaredRendersFailRate sanity-checks the
// populated judge path renders the numbers, not the honesty fallback.
func TestRenderAutonomyHealth_JudgeDeclaredRendersFailRate(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{Judge: judgeBlock{
		Declared: true,
		Counts:   map[string]int64{"fail": 17, "abstain": 21, "pass": 2},
		Total:    40,
		FailRate: 0.425,
	}})
	if strings.Contains(out, "no verdicts") {
		t.Errorf("a declared judge block must not print 'no verdicts', got:\n%s", out)
	}
	if !strings.Contains(out, "42.5%") {
		t.Errorf("expected the fail rate rendered, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_HeaderStatesWindowAndScope pins the required
// header content: the window, the tick count, and that cadence adherence
// covers declared feeds only.
func TestRenderAutonomyHealth_HeaderStatesWindowAndScope(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{
		ProjectID: "assistant",
		WindowHrs: 120,
		Since:     "2026-09-05T00:00:00Z",
		Outcomes:  healthOutcomes{Counts: map[string]int64{"CREATED": 40}, Total: 40},
	})
	if !strings.Contains(out, "120") {
		t.Errorf("header must state the window, got:\n%s", out)
	}
	if !strings.Contains(out, "40") {
		t.Errorf("header must state the tick count, got:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "declared feeds only") {
		t.Errorf("header must state cadence adherence covers declared feeds only, got:\n%s", out)
	}
}

// TestRenderAutonomyHealth_FullContractRoundTrip unmarshals the actual
// nil-repo JSON shape GetAutonomyHealth's respondAutonomyHealthNotEvaluated
// emits (internal/api/autonomy_handlers.go) and asserts every honesty
// string fires together -- the shape a brand-new project with no autonomy
// eval repo wired would show an operator.
func TestRenderAutonomyHealth_FullContractRoundTrip(t *testing.T) {
	raw := []byte(`{
		"projectId": "assistant",
		"windowHrs": 24,
		"outcomes": {"counts": {}, "total": 0},
		"feedsDeclared": false,
		"feeds": null,
		"delivery": {"rows": [], "truncated": false, "unresolved": 0},
		"judge": {"declared": false},
		"routeChurn": {}
	}`)
	var p healthPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	out := renderAutonomyHealth(p)
	for _, want := range []string{"not declared", "no verdicts", "not measured"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in rendered output, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OK") {
		t.Errorf("nil-repo project must never print OK anywhere, got:\n%s", out)
	}
}

// Finding 2's CLI half. "never ran" and "no run inside the examined
// history" are different facts and must not render alike: a feed 200
// days overdue printed byte-identically to one that had genuinely never
// run, because FeedObservation carried no horizon for the renderer to
// read.
func TestRenderAutonomyHealth_UnmeasuredDistinctFromNeverRan(t *testing.T) {
	neverRan := renderAutonomyHealth(healthPayload{
		FeedsDeclared: true,
		Feeds: []healthFeedRow{{
			Slug: "cultural-events", CadenceSeconds: 3600, NeverRan: true,
			HorizonTasks: 3, HorizonOldest: "2026-09-01T00:00:00Z",
		}},
	})
	if !strings.Contains(neverRan, "never ran") {
		t.Errorf("an unsaturated page examined the whole history; want \"never ran\":\n%s", neverRan)
	}
	if strings.Contains(cadenceSection(neverRan), "not measured") {
		t.Errorf("a genuine never-ran must not claim it was unmeasured:\n%s", neverRan)
	}

	unmeasured := renderAutonomyHealth(healthPayload{
		FeedsDeclared: true,
		Feeds: []healthFeedRow{{
			Slug: "cultural-events", CadenceSeconds: 3600,
			NeverRan: true, Unmeasured: true, LagAtLeastSeconds: 838800, // 233h
			HorizonTasks: 50, HorizonOldest: "2026-09-01T00:00:00Z",
		}},
	})
	if strings.Contains(unmeasured, "never ran") {
		t.Errorf("an unmeasured feed must NOT print \"never ran\" -- that is the conflation:\n%s", unmeasured)
	}
	if !strings.Contains(unmeasured, ">233h0m0s") {
		t.Errorf("want the lower bound rendered with a leading \">\":\n%s", unmeasured)
	}
	for _, want := range []string{"not measured", "last 50 tasks", "2026-09-01T00:00:00Z", "lower bound"} {
		if !strings.Contains(cadenceSection(unmeasured), want) {
			t.Errorf("want the scope of the claim published (%q):\n%s", want, unmeasured)
		}
	}
}

// A provable slow breach on an unmeasured feed still renders in the
// BREACH column: the bound is enough to know it is overdue even though
// the lag itself was never measured.
func TestRenderAutonomyHealth_UnmeasuredCanStillCarryABreach(t *testing.T) {
	out := renderAutonomyHealth(healthPayload{
		FeedsDeclared: true,
		Feeds: []healthFeedRow{{
			Slug: "czech-news", CadenceSeconds: 14400,
			NeverRan: true, Unmeasured: true, LagAtLeastSeconds: 838800,
			HorizonTasks: 50, HorizonOldest: "2026-09-01T00:00:00Z", Breach: "slow",
		}},
	})
	if !strings.Contains(out, "slow") {
		t.Errorf("a proven breach must render even without a measured lag:\n%s", out)
	}
}

// cadenceSection slices the CADENCE block out of a rendered health page
// so a cadence assertion cannot be satisfied (or defeated) by identical
// wording in the ROUTE CHURN block, which has its own "not measured".
func cadenceSection(out string) string {
	start := strings.Index(out, "CADENCE (declared feeds only)")
	if start < 0 {
		return ""
	}
	rest := out[start:]
	if end := strings.Index(rest, "\nDELIVERY"); end >= 0 {
		return rest[:end]
	}
	return rest
}
