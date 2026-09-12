package autonomy

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// taskWithPrompt builds a task whose payload extractPrompt can actually
// parse. extractPrompt (manager.go) reads `{"context":{"prompt":"..."}}` —
// the shape createTaskErr persists in production (see
// manager_creation_hv_test.go, manager_options_test.go) — not the flat
// `{"prompt":"..."}` the task brief's helper sketched. Using the flat shape
// here would make every task invisible to FeedObservations and turn the
// FastBreach/SlowBreach/FailedPreviousRun cases below into false passes
// (NeverRan=true, not a measured breach).
func taskWithPrompt(prompt string, created time.Time) *persistence.Task {
	return &persistence.Task{
		Payload:   []byte(`{"context":{"prompt":` + quote(prompt) + `}}`),
		CreatedAt: created,
	}
}

func quote(s string) string { return `"` + s + `"` }

// feedPage is the shape FeedObservations is handed in production: a page
// of the project's most recent tasks, newest first, bounded by pageSize.
// Tests that do not care about the page horizon pass 0, which means "this
// slice is the whole history" and keeps NeverRan meaning "genuinely never
// ran".
const unboundedPage = 0

// Incident 2026-09-10: cultural-events is declared at a 60-day (1440h)
// cadence and ran TWICE 101.6h apart -- 14x too often. The breach is the
// GAP between the two runs, not the age of the newest one: a single run
// carries no gap and cannot be a `fast` breach at all (design §4, "a new
// task for a slug whose PREVIOUS run is younger than half its cadence").
// The first cut of this test modelled the incident as one task aged
// 101.6h, which certified the age-based predicate the fix removes.
func TestFeedObservations_FastBreach_CulturalEvents20260910(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	second := now.Add(-1 * time.Hour)
	first := second.Add(-101*time.Hour - 36*time.Minute) // the two runs, 101.6h apart
	feeds := []registry.ResolvedFeed{{Slug: "cultural-events", Cadence: 1440 * time.Hour}}
	tasks := []*persistence.Task{
		taskWithPrompt("cultural-events: refresh", second),
		taskWithPrompt("cultural-events: refresh", first),
	}

	got := FeedObservations(feeds, tasks, unboundedPage, now)
	if len(got) != 1 {
		t.Fatalf("want 1 observation, got %d", len(got))
	}
	if got[0].Breach != "fast" {
		t.Errorf("Breach = %q, want fast (two runs 101.6h apart against a 1440h cadence)", got[0].Breach)
	}
}

// The regression the age-based predicate could not express: a feed that
// just ran, exactly on schedule, is NOT a breach. Under the shipped
// (pre-2026-09-11) predicate this feed reported `fast` for the whole
// first half of every cadence period -- one increment per tick, forever,
// and `BREACH=fast` in the CLI for a feed doing precisely what it was
// asked to do.
func TestFeedObservations_OnScheduleGapIsNotFast(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 12 * time.Hour}}
	tasks := []*persistence.Task{
		taskWithPrompt("czech-news: refresh", now.Add(-1*time.Hour)),  // just ran
		taskWithPrompt("czech-news: refresh", now.Add(-13*time.Hour)), // 12h earlier: on cadence
	}

	got := FeedObservations(feeds, tasks, unboundedPage, now)
	if got[0].Breach != "" {
		t.Errorf("Breach = %q, want none: a 12h gap on a 12h cadence is on schedule", got[0].Breach)
	}
}

// One observed run has no interval to measure, so it can never be a
// `fast` breach -- reporting one would be inventing a gap.
func TestFeedObservations_SingleRunIsNeverFast(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 12 * time.Hour}}
	tasks := []*persistence.Task{taskWithPrompt("czech-news: refresh", now.Add(-1*time.Hour))}

	got := FeedObservations(feeds, tasks, unboundedPage, now)
	if got[0].Breach != "" {
		t.Errorf("Breach = %q, want none: a single run carries no gap", got[0].Breach)
	}
	if got[0].Lag != time.Hour {
		t.Errorf("Lag = %v, want 1h", got[0].Lag)
	}
}

// The incident's slow half, with its real numbers: czech-news promised
// 4h, observed mean gap 19.7h -- 4.9x slow (design §1.1).
func TestFeedObservations_SlowBreach(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 4 * time.Hour}}
	lag := 19*time.Hour + 42*time.Minute // 19.7h
	tasks := []*persistence.Task{taskWithPrompt("czech-news: refresh", now.Add(-lag))}

	got := FeedObservations(feeds, tasks, unboundedPage, now)
	if got[0].Breach != "slow" {
		t.Errorf("Breach = %q, want slow (19.7h against a 4h cadence -- 4.9x)", got[0].Breach)
	}
	if got[0].Lag != lag {
		t.Errorf("Lag = %v, want %v", got[0].Lag, lag)
	}
}

func TestFeedObservations_WithinCadence_NoBreach(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 12 * time.Hour}}
	tasks := []*persistence.Task{taskWithPrompt("czech-news: refresh", now.Add(-8*time.Hour))}

	if got := FeedObservations(feeds, tasks, unboundedPage, now); got[0].Breach != "" {
		t.Errorf("Breach = %q, want none (8h into a 12h cadence)", got[0].Breach)
	}
}

// A failed previous run does not reset the timer and does not license an
// immediate retry: a feed retrying in a tight loop is a degradation
// whatever the reason (design §4). Expressed as a GAP -- the retry one
// hour after a FAILED run -- because that is the predicate `fast`
// actually implements; the FAILED run must count as the previous run
// exactly as a COMPLETED one would.
func TestFeedObservations_FailedPreviousRunStillCounts(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 12 * time.Hour}}
	failed := taskWithPrompt("czech-news: refresh", now.Add(-2*time.Hour))
	failed.Status = persistence.TaskStatusFailed
	retry := taskWithPrompt("czech-news: refresh", now.Add(-1*time.Hour))

	got := FeedObservations(feeds, tasks(retry, failed), unboundedPage, now)
	if got[0].Breach != "fast" {
		t.Errorf("Breach = %q, want fast -- a FAILED run must still count as the previous run", got[0].Breach)
	}
}

func tasks(ts ...*persistence.Task) []*persistence.Task { return ts }

func TestFeedObservations_NeverRan_NoBreachNoLag(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "never-ran", Cadence: 12 * time.Hour}}

	got := FeedObservations(feeds, nil, unboundedPage, now)
	if len(got) != 1 {
		t.Fatalf("a never-run feed must still produce an observation, got %d", len(got))
	}
	if got[0].Breach != "" || got[0].Lag != 0 {
		t.Errorf("never-run feed: Breach=%q Lag=%v, want empty/0 -- absent evidence is not a breach",
			got[0].Breach, got[0].Lag)
	}
	if !got[0].NeverRan {
		t.Error("NeverRan must be true so callers can render it distinctly from a 0s lag")
	}
	if got[0].Unmeasured() {
		t.Error("an unsaturated page examined the whole history: this is a real never-ran, not an unmeasured feed")
	}
}

// --------------------------------------------------------------------
// Horizon: "no run found in a FULL page" is not "never ran". Both call
// sites bound history at 50 tasks (manager.go, autonomy_handlers.go); at
// the shipped demand of ~5.16 tasks/day that is ~9.7 days, so a 60-day
// feed sits outside the window ~84% of its cycle. Before the horizon was
// carried, a feed 200 days overdue was byte-identical in output to one
// that had genuinely never run, and `slow` was unreachable for any feed
// that had fallen off the end of the page.
// --------------------------------------------------------------------

// fullPage builds `n` filler tasks (none carrying the slug under test)
// spanning `span` back from now, so the page is saturated at pageSize n.
func fullPage(n int, span time.Duration, now time.Time) []*persistence.Task {
	out := make([]*persistence.Task, 0, n)
	for i := 0; i < n; i++ {
		age := time.Duration(float64(span) * float64(i) / float64(n-1))
		out = append(out, taskWithPrompt("other-feed: refresh", now.Add(-age)))
	}
	return out
}

func TestFeedObservations_SaturatedPageIsUnmeasuredNotNeverRan(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	horizonSpan := 233 * time.Hour // ~9.7 days, the shipped 50-task window
	feeds := []registry.ResolvedFeed{{Slug: "cultural-events", Cadence: 1440 * time.Hour}}

	got := FeedObservations(feeds, fullPage(50, horizonSpan, now), 50, now)
	if !got[0].NeverRan {
		t.Fatal("no matching task in the page: NeverRan must be true")
	}
	if !got[0].Unmeasured() {
		t.Error("a SATURATED page means older history exists unexamined: Unmeasured must be true")
	}
	if got[0].Horizon.Tasks != 50 || !got[0].Horizon.Saturated {
		t.Errorf("Horizon = %+v, want Tasks=50 Saturated=true", got[0].Horizon)
	}
	if got[0].Horizon.Oldest != now.Add(-horizonSpan) {
		t.Errorf("Horizon.Oldest = %v, want %v", got[0].Horizon.Oldest, now.Add(-horizonSpan))
	}
	if got[0].LagAtLeast != horizonSpan {
		t.Errorf("LagAtLeast = %v, want %v (a proven lower bound, not a measurement)", got[0].LagAtLeast, horizonSpan)
	}
	// 233h of examined history cannot prove a 1440h feed is overdue.
	if got[0].Breach != "" {
		t.Errorf("Breach = %q, want none: the examined window is shorter than the cadence", got[0].Breach)
	}
}

// The `slow` class the whole surface exists for, made reachable again:
// czech-news promises 4h and appears nowhere in a full page spanning
// 233h. Nothing needs to be measured to know that is overdue -- the
// bound alone proves it. Under the shipped code this feed emitted no
// lag, no breach, and printed "never ran".
func TestFeedObservations_SaturatedPageProvesSlow(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	horizonSpan := 233 * time.Hour
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 4 * time.Hour}}

	got := FeedObservations(feeds, fullPage(50, horizonSpan, now), 50, now)
	if got[0].Breach != "slow" {
		t.Errorf("Breach = %q, want slow: no run in 233h of examined history against a 4h cadence", got[0].Breach)
	}
	if got[0].LagAtLeast != horizonSpan {
		t.Errorf("LagAtLeast = %v, want %v", got[0].LagAtLeast, horizonSpan)
	}
	if got[0].Lag != 0 {
		t.Errorf("Lag = %v, want 0: a bound is not a measurement and must not be reported as one", got[0].Lag)
	}
}

// An UNSATURATED page is the whole history, so the same absence is a real
// "never ran" -- no bound, no breach.
func TestFeedObservations_UnsaturatedPageIsRealNeverRan(t *testing.T) {
	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	feeds := []registry.ResolvedFeed{{Slug: "czech-news", Cadence: 4 * time.Hour}}

	got := FeedObservations(feeds, fullPage(10, 233*time.Hour, now), 50, now)
	if got[0].Unmeasured() {
		t.Error("10 tasks out of a 50 bound is the whole history: Unmeasured must be false")
	}
	if got[0].Breach != "" || got[0].LagAtLeast != 0 {
		t.Errorf("Breach=%q LagAtLeast=%v, want none: absent evidence is not evidence of a breach",
			got[0].Breach, got[0].LagAtLeast)
	}
}

func TestFeedObservations_UndeclaredFeedsProduceNothing(t *testing.T) {
	now := time.Now().UTC()
	tasksIn := []*persistence.Task{taskWithPrompt("czech-news: refresh", now)}
	if got := FeedObservations(nil, tasksIn, unboundedPage, now); len(got) != 0 {
		t.Errorf("undeclared feeds must produce no observations, got %+v", got)
	}
}

func TestFeedObservations_PrefixMatchIsExact(t *testing.T) {
	now := time.Now().UTC()
	feeds := []registry.ResolvedFeed{{Slug: "news", Cadence: time.Hour}}
	// "czech-news: ..." must NOT match the slug "news".
	tasksIn := []*persistence.Task{taskWithPrompt("czech-news: refresh", now.Add(-30*time.Minute))}
	got := FeedObservations(feeds, tasksIn, unboundedPage, now)
	if !got[0].NeverRan {
		t.Error(`slug "news" must not match a "czech-news: " prompt`)
	}
}

// --------------------------------------------------------------------
// Emission wiring — buildStateContext's call site (manager.go), not just
// the pure FeedObservations math. The 2026-09-10 incident was specifically
// that no series recorded either overrun: the wiring, not the arithmetic,
// was the failure mode, so it needs its own coverage that would catch a
// transposed WithLabelValues argument or an inverted NeverRan guard.
// --------------------------------------------------------------------

// findMetricFamily returns the named family from a Gather() result, or nil
// if the family was never populated. Note: an *unfound* family is the
// expected (and desired) shape for "nothing declared" / "never ran" —
// prometheus only emits a family once some label combination has actually
// been touched, so absence here is a real assertion, not a missing setup
// step.
func findMetricFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// metricWithLabels returns the metric in mf whose label set exactly
// matches want (same names AND values — a transposed WithLabelValues call
// produces a metric with the label *names* still correct but the wrong
// *values* assigned to them, which this catches), or nil if none matches.
func metricWithLabels(mf *dto.MetricFamily, want map[string]string) *dto.Metric {
	if mf == nil {
		return nil
	}
	for _, metric := range mf.GetMetric() {
		if len(metric.GetLabel()) != len(want) {
			continue
		}
		match := true
		for _, lbl := range metric.GetLabel() {
			if v, ok := want[lbl.GetName()]; !ok || v != lbl.GetValue() {
				match = false
				break
			}
		}
		if match {
			return metric
		}
	}
	return nil
}

// TestBuildStateContext_EmitsFeedLagAndCadenceBreach is the emission-level
// regression the incident actually calls for (design §10 test matrix):
// a declared, previously-run feed must produce both series with the
// correct label values, and a declared-but-NEVER-run feed sharing the same
// tick must produce neither. The two feeds in one call is deliberate: it
// is what would catch the NeverRan skip being inverted (the never-run feed
// would then wrongly emit, and the ran feed would wrongly go silent) as
// well as a transposed project_id/slug/direction argument (the label
// values would land on the wrong label names).
func TestBuildStateContext_EmitsFeedLagAndCadenceBreach(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	// Incident-shaped: cultural-events at a 1440h (60-day) cadence, run
	// TWICE 101.6h apart -- 14x too often, a "fast" breach. Two tasks,
	// not one: `fast` is the interval between consecutive runs, so a
	// single task cannot express the incident (and a single task must
	// not be reported as a breach at all).
	now := time.Now()
	secondRun := now.Add(-1 * time.Hour)
	firstRun := secondRun.Add(-101*time.Hour - 36*time.Minute)
	tasks := []*persistence.Task{
		{ID: "t2", ProjectID: "p1", Status: persistence.TaskStatusQueued, CreatedAt: secondRun,
			Payload: []byte(`{"context":{"prompt":"cultural-events: refresh"}}`)},
		{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusQueued, CreatedAt: firstRun,
			Payload: []byte(`{"context":{"prompt":"cultural-events: refresh"}}`)},
	}
	repo := &mockTaskRepo{tasks: tasks}
	m := New(nil, &registry.Registry{}, repo, nil, WithMetrics(metrics))

	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Feeds: []registry.AutonomyFeed{
				{Slug: "cultural-events", Cadence: "1440h"},
				// Declared but no matching task anywhere in the tick's
				// task list -- must stay silent (absent evidence is not
				// a lag of zero).
				{Slug: "never-run-feed", Cadence: "12h"},
			},
		},
	}

	if _, _, err := m.buildStateContext(context.Background(), project); err != nil {
		t.Fatalf("buildStateContext error: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	lagFamily := findMetricFamily(mfs, "vornik_autonomy_feed_lag_seconds")
	ranLag := metricWithLabels(lagFamily, map[string]string{"project_id": "p1", "slug": "cultural-events"})
	if ranLag == nil {
		t.Fatalf("no feed_lag_seconds series for cultural-events; families: %+v", mfs)
	}
	wantLagSeconds := time.Hour.Seconds() // age of the MOST RECENT run
	if got := ranLag.GetGauge().GetValue(); got < wantLagSeconds-5 || got > wantLagSeconds+5 {
		t.Errorf("feed_lag_seconds{cultural-events} = %v, want ~%v (1h since the newest run)", got, wantLagSeconds)
	}

	breachFamily := findMetricFamily(mfs, "vornik_autonomy_feed_cadence_breach_total")
	fastBreach := metricWithLabels(breachFamily,
		map[string]string{"project_id": "p1", "slug": "cultural-events", "direction": "fast"})
	if fastBreach == nil {
		t.Fatalf("no feed_cadence_breach_total{direction=fast} series for cultural-events; families: %+v", mfs)
	}
	if got := fastBreach.GetCounter().GetValue(); got != 1 {
		t.Errorf("feed_cadence_breach_total{cultural-events,fast} = %v, want 1", got)
	}

	if m := metricWithLabels(lagFamily, map[string]string{"project_id": "p1", "slug": "never-run-feed"}); m != nil {
		t.Errorf("never-run-feed must not emit feed_lag_seconds, got %v", m.GetGauge().GetValue())
	}
	for _, dir := range []string{"slow", "fast"} {
		if m := metricWithLabels(breachFamily,
			map[string]string{"project_id": "p1", "slug": "never-run-feed", "direction": dir}); m != nil {
			t.Errorf("never-run-feed must not emit feed_cadence_breach_total{%s}, got %v", dir, m.GetCounter().GetValue())
		}
	}
}

// TestBuildStateContext_NoDeclaredFeeds_EmitsNoFeedSeries covers the other
// half of the spec line: a project that declares no autonomy.feeds at all
// must add no series -- not even an empty one -- so it can never be
// mistaken for a feed that happens to read as healthy.
func TestBuildStateContext_NoDeclaredFeeds_EmitsNoFeedSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	tasks := []*persistence.Task{
		taskWithPrompt("czech-news: refresh", time.Now().Add(-1*time.Hour)),
	}
	repo := &mockTaskRepo{tasks: tasks}
	m := New(nil, &registry.Registry{}, repo, nil, WithMetrics(metrics))

	project := &registry.Project{ID: "p1"} // Autonomy.Feeds left empty

	if _, _, err := m.buildStateContext(context.Background(), project); err != nil {
		t.Fatalf("buildStateContext error: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	if mf := findMetricFamily(mfs, "vornik_autonomy_feed_lag_seconds"); mf != nil {
		t.Errorf("no declared feeds must emit no feed_lag_seconds series, got %+v", mf)
	}
	if mf := findMetricFamily(mfs, "vornik_autonomy_feed_cadence_breach_total"); mf != nil {
		t.Errorf("no declared feeds must emit no feed_cadence_breach_total series, got %+v", mf)
	}
}

// TestBuildStateContext_SaturatedPageProvesSlowWithoutAGauge is the
// emission half of the horizon fix. A declared 4h feed that appears
// nowhere in a FULL page of history must (a) increment the slow breach
// counter -- the class this whole surface exists for, previously
// unreachable for any feed that had fallen off the end of the page --
// and (b) emit NO feed_lag_seconds, because a lower bound is not a
// measurement and publishing it as one would understate the real lag.
func TestBuildStateContext_SaturatedPageProvesSlowWithoutAGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	now := time.Now()
	// Exactly autonomyStateTaskPageSize tasks, none of them czech-news,
	// spanning 233h -- the shipped 50-task window at ~5.16 tasks/day.
	filler := make([]*persistence.Task, 0, autonomyStateTaskPageSize)
	for i := 0; i < autonomyStateTaskPageSize; i++ {
		age := time.Duration(float64(233*time.Hour) * float64(i) / float64(autonomyStateTaskPageSize-1))
		filler = append(filler, &persistence.Task{
			ID: "f", ProjectID: "p1", Status: persistence.TaskStatusQueued, CreatedAt: now.Add(-age),
			Payload: []byte(`{"context":{"prompt":"other-feed: refresh"}}`),
		})
	}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{tasks: filler}, nil, WithMetrics(metrics))

	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Feeds: []registry.AutonomyFeed{{Slug: "czech-news", Cadence: "4h"}},
		},
	}

	if _, _, err := m.buildStateContext(context.Background(), project); err != nil {
		t.Fatalf("buildStateContext error: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	breach := metricWithLabels(findMetricFamily(mfs, "vornik_autonomy_feed_cadence_breach_total"),
		map[string]string{"project_id": "p1", "slug": "czech-news", "direction": "slow"})
	if breach == nil {
		t.Fatalf("no slow breach for a 4h feed absent from 233h of examined history; families: %+v", mfs)
	}
	if got := breach.GetCounter().GetValue(); got != 1 {
		t.Errorf("feed_cadence_breach_total{czech-news,slow} = %v, want 1", got)
	}
	if mf := findMetricFamily(mfs, "vornik_autonomy_feed_lag_seconds"); mf != nil {
		t.Errorf("an unmeasured feed must emit no lag gauge (a bound is not a measurement), got %+v", mf)
	}
}

// TestBuildStateContext_MeasuredFeedFallingOffThePageDeletesItsGauge is the
// TRANSITION regression, and it is deliberately not a steady-state one.
//
// FeedLagSeconds is a GaugeVec, and a gauge child that is never written
// again is not absent -- it is frozen at its last value and exported
// forever as though it were current. So a feed that WAS measured and then
// falls off the 50-task page (precisely the degradation the horizon fix
// exists to detect) would keep reporting the lag it had on its last good
// tick while the breach counter climbed beside it. That makes the
// guarantee this change wrote into design §4 and LLD 11 §8 -- "absence of
// the series means not measured; presence means measured" -- false.
//
// TestBuildStateContext_SaturatedPageProvesSlowWithoutAGauge cannot catch
// this: it starts from a fresh registry with no prior Set, so "no child
// series" is true there for the wrong reason. The transition needs two
// ticks against the same registry.
//
// Two feeds, so the assertion also proves the delete is TARGETED: a
// Reset() on the whole vector would take the healthy feed's series out
// with it.
func TestBuildStateContext_MeasuredFeedFallingOffThePageDeletesItsGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	now := time.Now()

	feedTask := func(prompt string, age time.Duration) *persistence.Task {
		return &persistence.Task{
			ID: "t", ProjectID: "p1", Status: persistence.TaskStatusQueued, CreatedAt: now.Add(-age),
			Payload: []byte(`{"context":{"prompt":"` + prompt + `"}}`),
		}
	}

	repo := &mockTaskRepo{tasks: []*persistence.Task{
		feedTask("czech-news: refresh", 2*time.Hour),
		feedTask("world-news: refresh", 1*time.Hour),
	}}
	m := New(nil, &registry.Registry{}, repo, nil, WithMetrics(metrics))
	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Feeds: []registry.AutonomyFeed{
				{Slug: "czech-news", Cadence: "4h"},
				{Slug: "world-news", Cadence: "48h"},
			},
		},
	}

	// Tick 1: both feeds measured, both gauges written.
	if _, _, err := m.buildStateContext(context.Background(), project); err != nil {
		t.Fatalf("tick 1: buildStateContext error: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("tick 1: Gather() error: %v", err)
	}
	lagFamily := findMetricFamily(mfs, "vornik_autonomy_feed_lag_seconds")
	first := metricWithLabels(lagFamily, map[string]string{"project_id": "p1", "slug": "czech-news"})
	if first == nil {
		t.Fatalf("tick 1: czech-news must have a measured lag gauge; families: %+v", mfs)
	}
	if got := first.GetGauge().GetValue(); got < (2*time.Hour).Seconds()-5 {
		t.Fatalf("tick 1: feed_lag_seconds{czech-news} = %v, want ~7200", got)
	}

	// Tick 2: czech-news has fallen off the end of a now-SATURATED page.
	// world-news is still in it.
	filler := make([]*persistence.Task, 0, autonomyStateTaskPageSize)
	for i := 0; i < autonomyStateTaskPageSize; i++ {
		age := time.Duration(float64(233*time.Hour) * float64(i) / float64(autonomyStateTaskPageSize-1))
		filler = append(filler, feedTask("world-news: refresh", age))
	}
	repo.mu.Lock()
	repo.tasks = filler
	repo.mu.Unlock()

	if _, _, err := m.buildStateContext(context.Background(), project); err != nil {
		t.Fatalf("tick 2: buildStateContext error: %v", err)
	}
	mfs, err = reg.Gather()
	if err != nil {
		t.Fatalf("tick 2: Gather() error: %v", err)
	}
	lagFamily = findMetricFamily(mfs, "vornik_autonomy_feed_lag_seconds")

	if stale := metricWithLabels(lagFamily, map[string]string{"project_id": "p1", "slug": "czech-news"}); stale != nil {
		t.Errorf("feed_lag_seconds{czech-news} still exported as %v after the feed fell off the page; "+
			"a frozen gauge reads as a current measurement, which is the opposite of what "+
			"\"presence means measured\" promises", stale.GetGauge().GetValue())
	}
	// Targeted, not a Reset: the healthy feed keeps its series.
	if kept := metricWithLabels(lagFamily, map[string]string{"project_id": "p1", "slug": "world-news"}); kept == nil {
		t.Errorf("world-news is still measured and must keep its gauge; families: %+v", mfs)
	}
	// And the breach the operator is meant to act on is still counted.
	breach := metricWithLabels(findMetricFamily(mfs, "vornik_autonomy_feed_cadence_breach_total"),
		map[string]string{"project_id": "p1", "slug": "czech-news", "direction": "slow"})
	if breach == nil {
		t.Error("czech-news fell off a 233h page against a 4h cadence: the slow breach must still be counted")
	}
}
