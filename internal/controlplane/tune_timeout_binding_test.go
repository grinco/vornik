package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// scanTimeoutBinding — the raise-side counterpart to scanTimeoutReclaim
// (LLD 2026-08-10 §6).
//
// Today the only raise path lives inside scanLatency, which fires on a
// PROJECT-level execution-latency breach against an absolute threshold and
// only then inspects the slowest step. A step pinned against its own ceiling
// while the project's overall latency stays under that bar is invisible — which
// is exactly what happened to easeit-companion/ingest on 2026-08-10: the step
// timed out on every attempt, on_fail:recover absorbed it, and the executions
// completed in minutes.

// The load-bearing case. A truncated run's recorded duration is CAPPED at the
// timeout, so a step that times out repeatedly can still show a p95 BELOW the
// binding threshold. Outcome rate is what makes this detector correct rather
// than merely plausible — p95 alone would miss it.
func TestScanTimeoutBinding_FiresOnDegradedRateWithP95BelowThreshold(t *testing.T) {
	repo := newTuneTestRepo(t)
	// dev-pipeline/implement has a 10m (600s) timeout. p95 300s = 0.5x, well
	// under the 0.8 binding threshold, yet a third of runs are timing out.
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement", Role: "coder", Model: "m1",
		P95Seconds: 300, MaxSeconds: 300, Count: 30, DegradedCount: 10, TimeoutCount: 10,
	}})

	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}

	ps, err := repo.List(context.Background(), persistence.ProposalListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("want 1 raise proposal, got %d", len(ps))
	}
	if ps[0].ApplyTarget == "" {
		t.Fatalf("raise proposal must be applyable, got target %q", ps[0].ApplyTarget)
	}
	if !strings.Contains(strings.ToLower(ps[0].Title), "timeout") {
		t.Fatalf("title must name the timeout: %q", ps[0].Title)
	}
}

// A healthy step with headroom and no degradation must not be touched — this
// is the reclaim detector's territory, and firing here would oscillate.
func TestScanTimeoutBinding_QuietOnHealthyStep(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement", Role: "coder", Model: "m1",
		P95Seconds: 60, MaxSeconds: 90, Count: 30,
	}})

	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}

	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("filed %d proposal(s) for a healthy step; want 0", n)
	}
}

// A single bad scan must not propose — the shared streak applies here too.
func TestScanTimeoutBinding_RequiresSustainedBreach(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement", Role: "coder", Model: "m1",
		P95Seconds: 300, MaxSeconds: 300, Count: 30, DegradedCount: 10, TimeoutCount: 10,
	}})

	w.scanTimeoutBinding(context.Background())

	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("filed %d proposal(s) after one scan; want 0 (streak is %d)", n, w.breachesToPropose())
	}
}

// p95 at/above the binding threshold still fires even with no degraded
// outcomes yet — impending truncation, the existing binding semantics.
func TestScanTimeoutBinding_FiresOnBindingP95WithoutDegradation(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement", Role: "coder", Model: "m1",
		// 0.8 * 600s = 480s exactly.
		P95Seconds: 480, MaxSeconds: 500, Count: 30,
	}})

	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}

	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("filed %d proposal(s) for a step at the binding threshold; want 1", n)
	}
}

// The degraded-rate threshold is a RATE, not a count: one timeout in a large
// clean window is noise, not a signal to raise.
func TestScanTimeoutBinding_IgnoresIsolatedDegradation(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement", Role: "coder", Model: "m1",
		P95Seconds: 60, MaxSeconds: 90, Count: 200, DegradedCount: 1, TimeoutCount: 1, // 0.5%
	}})

	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}

	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("filed %d proposal(s) for 0.5%% degradation; want 0", n)
	}
}

// Censored data: once a step times out, every truncated run records exactly the
// timeout, so observed durations cannot tell you what the step actually needed.
// The suggestion must escalate relative to the CURRENT ceiling, not to the
// censored observations — otherwise it renders below current, the raise is
// refused as not-useful, and the step times out forever.
func TestBindingSuggestion_EscalatesFromCurrentWhenTruncated(t *testing.T) {
	current := 600 * time.Second
	st := StepLatencySample{P95Seconds: 300, MaxSeconds: 300, Count: 30, DegradedCount: 10, TimeoutCount: 10}

	got := bindingSuggestion(st, current)

	if want := 900 * time.Second; got != want {
		t.Fatalf("bindingSuggestion = %v, want %v (current*1.5, not the censored 450s)", got, want)
	}
}

// With no truncation the observations are trustworthy, so the usual
// max(p95, observedMax) * 1.5 basis governs.
func TestBindingSuggestion_UsesObservationsWhenNotTruncated(t *testing.T) {
	current := 600 * time.Second
	st := StepLatencySample{P95Seconds: 480, MaxSeconds: 500, Count: 30}

	got := bindingSuggestion(st, current)

	if want := 750 * time.Second; got != want {
		t.Fatalf("bindingSuggestion = %v, want %v (max 500 * 1.5)", got, want)
	}
}

// Review finding 6 (review-20260810-53f0): raising a timeout only ever fixes
// TRUNCATION. A step failing on schema_violation or degenerate_loop is not
// short of time, and responding to it with a raise widens the ceiling on every
// scan for a cause the raise cannot address. Only timeout-class outcomes may
// drive the raise; the reclaim guard stays conservative and still refuses on
// ANY degradation.
func TestScanTimeoutBinding_IgnoresNonTimeoutDegradation(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement", Role: "coder", Model: "m1",
		// A third of runs degraded, but none of them by timing out, and p95 is
		// nowhere near the binding band.
		P95Seconds: 60, MaxSeconds: 90, Count: 30, DegradedCount: 10, TimeoutCount: 0,
	}})

	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}

	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("filed %d raise proposal(s) for non-timeout degradation; want 0", n)
	}
}

// The reclaim side stays conservative: ANY degradation disqualifies a
// reduction, including the non-timeout kinds that must not drive a raise.
func TestReclaimEligible_RefusesOnNonTimeoutDegradation(t *testing.T) {
	st := StepLatencySample{P95Seconds: 60, MaxSeconds: 90, Count: 50, DegradedCount: 5, TimeoutCount: 0}

	if reclaimEligible(st, 600*time.Second, 0.5, 30) {
		t.Fatal("reclaimEligible = true with non-timeout degradation; want false")
	}
}
