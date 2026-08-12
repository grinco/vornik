package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// These tests pin the reclaim-path corrections from LLD
// 2026-08-10-canary-class-registry-step-outcome-design.md §6.1.
//
// Incident they exist for: cpp_20260804153515_03203f48c3ed91c5 cut the
// easeit-companion `ingest` step from 15m to 259s, computed as
// ceil(p95 173s * 1.5) over exactly 5 samples, while the step's observed max
// over the preceding ten days was already 237s. On 2026-08-10 the step began
// timing out on every attempt and the task parked in AWAITING_INPUT.

func TestReclaimSuggestion_FloorsAtObservedMax(t *testing.T) {
	// The real incident's numbers. p95*1.5 = 259s would truncate a run that
	// had already been observed (237s); max*1.5 = 356s does not.
	st := StepLatencySample{P95Seconds: 172.6, MaxSeconds: 237}

	got := reclaimSuggestion(st)

	if want := 356 * time.Second; got != want {
		t.Fatalf("reclaimSuggestion = %v, want %v (must floor at observed max, not p95)", got, want)
	}
}

func TestReclaimSuggestion_UsesP95WhenMaxIsClose(t *testing.T) {
	// A well-behaved step whose max sits near its p95: the p95 term governs
	// and the floor changes nothing.
	st := StepLatencySample{P95Seconds: 100, MaxSeconds: 100}

	got := reclaimSuggestion(st)

	if want := 150 * time.Second; got != want {
		t.Fatalf("reclaimSuggestion = %v, want %v", got, want)
	}
}

func TestReclaimEligible_RequiresHigherSampleFloorThanRaises(t *testing.T) {
	// 5 samples cleared the shared minSamples() floor and is what actually
	// fired in the incident. A reduction can only ever cause truncation, so it
	// must demand materially more evidence.
	current := 15 * time.Minute
	st := StepLatencySample{P95Seconds: 172.6, MaxSeconds: 237, Count: 5}

	if reclaimEligible(st, current, 0.5, 30) {
		t.Fatal("reclaimEligible = true with 5 samples; want false (below the reduce floor of 30)")
	}
}

func TestReclaimEligible_AllowsAtTheReduceFloor(t *testing.T) {
	current := 15 * time.Minute
	st := StepLatencySample{P95Seconds: 172.6, MaxSeconds: 237, Count: 30}

	if !reclaimEligible(st, current, 0.5, 30) {
		t.Fatal("reclaimEligible = false at exactly the reduce floor; want true")
	}
}

func TestReclaimEligible_RefusesWhenStepHasDegradedOutcomes(t *testing.T) {
	// A step that timed out at all is not over-provisioned, whatever its p95
	// says — and a truncated run's recorded duration is capped at the timeout,
	// which drags p95 DOWN and makes the step look even more reclaimable.
	current := 15 * time.Minute
	st := StepLatencySample{P95Seconds: 172.6, MaxSeconds: 237, Count: 50, DegradedCount: 1}

	if reclaimEligible(st, current, 0.5, 30) {
		t.Fatal("reclaimEligible = true with a degraded outcome in the window; want false")
	}
}

func TestReclaimEligible_RefusesWhenP95HasNoHeadroom(t *testing.T) {
	// Existing behaviour, pinned so the rewrite cannot drop it: p95 must sit
	// at/under ratio * current.
	current := 300 * time.Second
	st := StepLatencySample{P95Seconds: 280, MaxSeconds: 290, Count: 50}

	if reclaimEligible(st, current, 0.5, 30) {
		t.Fatal("reclaimEligible = true with p95 above the reclaim ratio; want false")
	}
}

// --- wiring: scanTimeoutReclaim must honour the §6.1 guards ----------------

func newReclaimWorker(t *testing.T, repo persistence.ProposalRepository, steps []StepLatencySample) *TuneWorker {
	t.Helper()
	w := newTuneWorker(repo, &fakeMetrics{steps: steps})
	w.Actionize = testActionizer(stdFiles())
	w.reclaimStreaks = map[WorkflowStepKey]int{}
	return w
}

// The incident's exact shape: dev-pipeline/implement has a 10m timeout, and
// 5 samples with a max already close to the suggestion. Three scans (the
// streak) must still file nothing, because 5 < MinSamplesReduce.
func TestScanTimeoutReclaim_RefusesBelowReduceSampleFloor(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement",
		P95Seconds: 172.6, MaxSeconds: 237, Count: 5,
	}})
	w.MinSamplesReduce = 30

	for i := 0; i < 3; i++ {
		w.scanTimeoutReclaim(context.Background())
	}

	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("filed %d proposal(s) from 5 samples; want 0", n)
	}
}

func TestScanTimeoutReclaim_RefusesWhenStepHasDegradedOutcomes(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement",
		P95Seconds: 100, MaxSeconds: 120, Count: 50, DegradedCount: 1,
	}})
	w.MinSamplesReduce = 30

	for i := 0; i < 3; i++ {
		w.scanTimeoutReclaim(context.Background())
	}

	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("filed %d proposal(s) for a step with a degraded outcome; want 0", n)
	}
}

// With enough clean samples the reclaim still fires — but the suggested value
// is floored at observed max, so it never truncates a run that happened.
func TestScanTimeoutReclaim_FilesMaxFlooredSuggestion(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "implement",
		P95Seconds: 172.6, MaxSeconds: 237, Count: 50,
	}})
	w.MinSamplesReduce = 30

	for i := 0; i < 3; i++ {
		w.scanTimeoutReclaim(context.Background())
	}

	ps, err := repo.List(context.Background(), persistence.ProposalListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("filed %d proposal(s); want exactly 1", len(ps))
	}
	// ceil(237 * 1.5) = 356s, not ceil(172.6 * 1.5) = 259s.
	if !strings.Contains(ps[0].ApplyContent, `timeout: "356s"`) {
		t.Fatalf("suggestion must be floored at observed max (356s), got:\n%s", ps[0].ApplyContent)
	}
	if strings.Contains(ps[0].ApplyContent, `timeout: "259s"`) {
		t.Fatal("suggestion is the p95-derived 259s — the incident value")
	}
}
