package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory/ned"
)

// --- planSample (the §7 sampling frame) --------------------------------------

func TestPlanSample_FloorCapAndInsufficientBucket(t *testing.T) {
	// janka dominates (like prod ~95%); two other eligible strata; three tiny
	// projects below the floor; one empty-id bucket that must be dropped.
	counts := map[string]int{
		"janka":     26770,
		"assistant": 768,
		"_external": 342,
		"small-a":   78,
		"small-b":   41,
		"tiny":      2,
		"":          281, // empty project id — never sampleable
	}
	plan := planSample(counts, 800, 100, 0.50)

	byProj := map[string]stratumPlan{}
	for _, sp := range plan.Eligible {
		byProj[sp.Project] = sp
	}

	// Dominant cap: janka may not exceed 50% of 800 = 400.
	if plan.CapMax != 400 {
		t.Errorf("CapMax = %d, want 400", plan.CapMax)
	}
	if byProj["janka"].Draw > 400 {
		t.Errorf("janka draw %d exceeds the 50%% dominant cap (400)", byProj["janka"].Draw)
	}

	// Every eligible stratum gets at least the floor of 100 (all have >=100).
	for _, p := range []string{"janka", "assistant", "_external"} {
		if byProj[p].Draw < 100 {
			t.Errorf("stratum %s draw %d below the 100 floor", p, byProj[p].Draw)
		}
	}
	// The whole budget is used when headroom allows (400+768cap? no: assistant
	// avail 768 but cap 400, _external 342). janka=400, others fill remainder.
	if plan.TotalDraw != 800 {
		t.Errorf("TotalDraw = %d, want 800 (budget fully allocatable here)", plan.TotalDraw)
	}
	// _external cannot be drawn beyond its availability of 342.
	if byProj["_external"].Draw > 342 {
		t.Errorf("_external draw %d exceeds availability 342", byProj["_external"].Draw)
	}

	// Insufficient bucket: small-a, small-b, tiny — sorted, with counts, and NOT
	// in the eligible set.
	wantInsuff := map[string]int{"small-a": 78, "small-b": 41, "tiny": 2}
	if len(plan.Insufficient) != 3 {
		t.Fatalf("insufficient bucket size = %d, want 3", len(plan.Insufficient))
	}
	for _, sp := range plan.Insufficient {
		if wantInsuff[sp.Project] != sp.Available {
			t.Errorf("insufficient %s available=%d, want %d", sp.Project, sp.Available, wantInsuff[sp.Project])
		}
	}
	if _, ok := byProj["small-a"]; ok {
		t.Error("small-a must not be an eligible stratum")
	}
	if _, ok := byProj[""]; ok {
		t.Error("empty-project-id turns must never be sampled")
	}
}

func TestPlanSample_CapsAvailabilityBelowFloorTarget(t *testing.T) {
	// A stratum with availability below what an even split would give must be
	// capped at its availability, and the remainder flows to strata with headroom.
	counts := map[string]int{"a": 1000, "b": 120}
	plan := planSample(counts, 400, 100, 0.50)
	byProj := map[string]stratumPlan{}
	for _, sp := range plan.Eligible {
		byProj[sp.Project] = sp
	}
	// cap = 200. b avail 120 < 200 so b<=120; a capped at 200.
	if byProj["b"].Draw > 120 {
		t.Errorf("b draw %d exceeds availability 120", byProj["b"].Draw)
	}
	if byProj["a"].Draw > 200 {
		t.Errorf("a draw %d exceeds cap 200", byProj["a"].Draw)
	}
}

func TestPlanSample_DeterministicOrdering(t *testing.T) {
	counts := map[string]int{"zeta": 500, "alpha": 500, "mu": 500}
	p1 := planSample(counts, 300, 100, 0.50)
	p2 := planSample(counts, 300, 100, 0.50)
	if len(p1.Eligible) != 3 {
		t.Fatalf("eligible = %d, want 3", len(p1.Eligible))
	}
	// Sorted by project name, deterministically.
	if p1.Eligible[0].Project != "alpha" || p1.Eligible[1].Project != "mu" || p1.Eligible[2].Project != "zeta" {
		t.Errorf("eligible order = %v, want alpha,mu,zeta", []string{p1.Eligible[0].Project, p1.Eligible[1].Project, p1.Eligible[2].Project})
	}
	for i := range p1.Eligible {
		if p1.Eligible[i] != p2.Eligible[i] {
			t.Errorf("planSample not deterministic at %d: %+v vs %+v", i, p1.Eligible[i], p2.Eligible[i])
		}
	}
}

// --- decisionGuidance (the §7 thresholds) ------------------------------------

func TestDecisionGuidance_NewOver30Blocks(t *testing.T) {
	g := decisionGuidance(map[string]float64{"new": 0.31, "ambiguous": 0.10, "match": 0.59}, 100)
	joined := strings.Join(g, "\n")
	if !strings.Contains(joined, "DO NOT ship") {
		t.Errorf("new>30%% must produce a DO-NOT-ship verdict; got:\n%s", joined)
	}
	if !strings.Contains(joined, "PERSONAL scope only") {
		t.Errorf("the re-scope options must be listed; got:\n%s", joined)
	}
}

func TestDecisionGuidance_AmbiguousOver20Tunes(t *testing.T) {
	g := decisionGuidance(map[string]float64{"new": 0.05, "ambiguous": 0.21, "match": 0.74}, 100)
	joined := strings.Join(g, "\n")
	if !strings.Contains(joined, "TUNE the resolver") {
		t.Errorf("ambiguous>20%% must produce a TUNE verdict; got:\n%s", joined)
	}
	if strings.Contains(joined, "DO NOT ship") {
		t.Errorf("new is below threshold — must not say DO NOT ship; got:\n%s", joined)
	}
}

func TestDecisionGuidance_BelowThresholdsShips(t *testing.T) {
	g := decisionGuidance(map[string]float64{"new": 0.30, "ambiguous": 0.20, "match": 0.50}, 100)
	joined := strings.Join(g, "\n")
	// Exactly at the thresholds is NOT over them → ship.
	if !strings.Contains(joined, "SHIP shared scope end-to-end") {
		t.Errorf("at-threshold values must ship; got:\n%s", joined)
	}
}

func TestDecisionGuidance_NoPersonBearingIsInconclusive(t *testing.T) {
	g := decisionGuidance(map[string]float64{}, 0)
	if len(g) != 1 || !strings.Contains(g[0], "INCONCLUSIVE") {
		t.Errorf("zero person-bearing turns must be inconclusive; got %v", g)
	}
}

// --- fakes for runCalibration ------------------------------------------------

type fakeTurnSource struct {
	counts     map[string]int
	byProject  map[string][]string // project -> texts to return
	sampleCall int
}

func (f *fakeTurnSource) AvailableCounts(_ context.Context, _ time.Time) (map[string]int, error) {
	return f.counts, nil
}

func (f *fakeTurnSource) SampleStratum(_ context.Context, projectID string, _ time.Time, n int, _ string) ([]string, error) {
	f.sampleCall++
	texts := f.byProject[projectID]
	if len(texts) > n {
		texts = texts[:n]
	}
	return texts, nil
}

// fakeScreener maps a turn text to an outcome by a leading marker so tests are
// fully deterministic without any LLM. "err:" texts return an error.
type fakeScreener struct {
	calls int
}

func (f *fakeScreener) Classify(_ context.Context, _ string, text string) (ned.Outcome, error) {
	f.calls++
	switch {
	case strings.HasPrefix(text, "err:"):
		return ned.OutcomeNone, errors.New("simulated transport error")
	case strings.HasPrefix(text, "new:"):
		return ned.OutcomeNew, nil
	case strings.HasPrefix(text, "amb:"):
		return ned.OutcomeAmbiguous, nil
	case strings.HasPrefix(text, "match:"):
		return ned.OutcomeMatch, nil
	default:
		return ned.OutcomeNone, nil
	}
}

func baseParams() calibrateParams {
	return calibrateParams{
		SampleSize:     800,
		MinPerStratum:  100,
		DominantCap:    0.50,
		Window:         30 * 24 * time.Hour,
		WindowLabel:    "30d",
		AsOf:           time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Seed:           "seed",
		ExtractorModel: "openai.gpt-oss-120b-1:0",
		ResolverModel:  "openai.gpt-oss-20b-1:0",
	}
}

// --- dry-run: ZERO screener calls --------------------------------------------

func TestRunCalibration_DryRunMakesNoScreenerCalls(t *testing.T) {
	src := &fakeTurnSource{counts: map[string]int{"janka": 500, "assistant": 200}}
	scr := &fakeScreener{}
	var buf bytes.Buffer
	p := baseParams()
	p.DryRun = true
	if err := runCalibration(context.Background(), src, scr, p, &buf); err != nil {
		t.Fatalf("dry-run error: %v", err)
	}
	if scr.calls != 0 {
		t.Errorf("dry-run must make ZERO screener calls; got %d", scr.calls)
	}
	if src.sampleCall != 0 {
		t.Errorf("dry-run must not draw samples; SampleStratum calls=%d", src.sampleCall)
	}
	out := buf.String()
	if !strings.Contains(out, "SAMPLING PLAN") || !strings.Contains(out, "no LLM calls") {
		t.Errorf("dry-run output missing plan header:\n%s", out)
	}
	if !strings.Contains(out, "estimate") {
		t.Errorf("dry-run must print a call estimate:\n%s", out)
	}
}

func TestRunCalibration_DryRunJSONShape(t *testing.T) {
	src := &fakeTurnSource{counts: map[string]int{"janka": 500, "assistant": 200, "tiny": 5}}
	var buf bytes.Buffer
	p := baseParams()
	p.DryRun = true
	p.JSON = true
	if err := runCalibration(context.Background(), src, &fakeScreener{}, p, &buf); err != nil {
		t.Fatalf("dry-run json error: %v", err)
	}
	var pj planJSON
	if err := json.Unmarshal(buf.Bytes(), &pj); err != nil {
		t.Fatalf("dry-run JSON did not parse: %v\n%s", err, buf.String())
	}
	if pj.Mode != "dry-run" {
		t.Errorf("mode = %q, want dry-run", pj.Mode)
	}
	if pj.EstCallsWorst != pj.Plan.TotalDraw*2 {
		t.Errorf("worst estimate = %d, want 2*%d", pj.EstCallsWorst, pj.Plan.TotalDraw)
	}
	if len(pj.Plan.Insufficient) != 1 || pj.Plan.Insufficient[0].Project != "tiny" {
		t.Errorf("insufficient bucket wrong: %+v", pj.Plan.Insufficient)
	}
}

// --- real run: tally + aggregation + guidance --------------------------------

func TestRunCalibration_TalliesAggregatesAndClassifies(t *testing.T) {
	// Two eligible strata. Craft outcomes so the overall person-bearing new-rate
	// crosses 30%.
	src := &fakeTurnSource{
		counts: map[string]int{"janka": 100, "assistant": 100},
		byProject: map[string][]string{
			// janka: 2 new, 1 amb, 1 match, 1 none, 1 err
			"janka": {"new:a", "new:b", "amb:c", "match:d", "none:e", "err:f"},
			// assistant: 1 new, 3 match, 1 none
			"assistant": {"new:g", "match:h", "match:i", "match:j", "none:k"},
		},
	}
	scr := &fakeScreener{}
	var buf bytes.Buffer
	p := baseParams()
	// Force both strata to draw all their fixture rows.
	p.SampleSize = 400
	if err := runCalibration(context.Background(), src, scr, p, &buf); err != nil {
		t.Fatalf("run error: %v", err)
	}
	out := buf.String()

	// Overall: new=3, ambiguous=1, match=4, none=2, err=1. withPerson=8.
	// new-rate = 3/8 = 37.5% > 30% → DO NOT ship.
	if !strings.Contains(out, "DO NOT ship") {
		t.Errorf("expected DO-NOT-ship verdict (new=3/8=37.5%%); got:\n%s", out)
	}
	// Errors are reported but excluded from the rate denominators.
	if !strings.Contains(out, "1 errors") {
		t.Errorf("error count must be surfaced separately; got:\n%s", out)
	}
	// The screener must have been called once per drawn turn (6 + 5 = 11).
	if scr.calls != 11 {
		t.Errorf("screener calls = %d, want 11 (one per drawn turn)", scr.calls)
	}
	// No content leaks: none of the fixture turn texts appear in the report.
	for _, leak := range []string{"new:a", "amb:c", "match:d", "none:e", "err:f", "new:g"} {
		if strings.Contains(out, leak) {
			t.Errorf("report leaked turn content %q:\n%s", leak, out)
		}
	}
}

func TestRunCalibration_JSONReportShape(t *testing.T) {
	src := &fakeTurnSource{
		counts:    map[string]int{"janka": 100},
		byProject: map[string][]string{"janka": {"match:a", "match:b", "none:c"}},
	}
	var buf bytes.Buffer
	p := baseParams()
	p.SampleSize = 100
	p.JSON = true
	if err := runCalibration(context.Background(), src, &fakeScreener{}, p, &buf); err != nil {
		t.Fatalf("run error: %v", err)
	}
	var r calibrationReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("report JSON did not parse: %v\n%s", err, buf.String())
	}
	if r.ExtractorModel != "openai.gpt-oss-120b-1:0" {
		t.Errorf("extractor model = %q", r.ExtractorModel)
	}
	if r.Overall.Match != 2 || r.Overall.None != 1 {
		t.Errorf("overall tally wrong: %+v", r.Overall)
	}
	// All match on person-bearing turns → ship.
	if r.WithPersonRates["match"] != 1.0 {
		t.Errorf("with-person match rate = %v, want 1.0", r.WithPersonRates["match"])
	}
	if len(r.DecisionGuidance) == 0 || !strings.Contains(strings.Join(r.DecisionGuidance, " "), "SHIP") {
		t.Errorf("expected SHIP guidance; got %v", r.DecisionGuidance)
	}
}

func TestRunCalibration_NonDryRunRequiresScreener(t *testing.T) {
	src := &fakeTurnSource{counts: map[string]int{"janka": 100}}
	var buf bytes.Buffer
	if err := runCalibration(context.Background(), src, nil, baseParams(), &buf); err == nil {
		t.Fatal("a non-dry-run without a screener must error")
	}
}

// --- parseWindow -------------------------------------------------------------

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"30d", 30 * 24 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"720h", 720 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"", 0, false},
		{"0d", 0, false},
		{"-5d", 0, false},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		got, err := parseWindow(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseWindow(%q) = %v, %v; want %v, nil", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseWindow(%q) expected an error", c.in)
		}
	}
}
