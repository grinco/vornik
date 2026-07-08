package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// fakeMetrics returns scripted signals per tick.
type fakeMetrics struct {
	ret   map[string]RateSample
	lats  map[string]LatencySample
	tools []ToolLatencySample
}

func (f *fakeMetrics) FailedTaskRates(_ context.Context) (map[string]RateSample, error) {
	return f.ret, nil
}

func (f *fakeMetrics) LatencyP95s(_ context.Context) (map[string]LatencySample, error) {
	return f.lats, nil
}

func (f *fakeMetrics) ToolLatencies(_ context.Context) ([]ToolLatencySample, error) {
	return f.tools, nil
}

func newTuneTestRepo(t *testing.T) persistence.ProposalRepository {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewProposalRepository(db.DB)
}

func draftCount(t *testing.T, repo persistence.ProposalRepository) int {
	t.Helper()
	ps, err := repo.List(context.Background(), persistence.ProposalListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(ps)
}

func newTuneWorker(repo persistence.ProposalRepository, m MetricsSource) *TuneWorker {
	return &TuneWorker{
		Proposals: repo, Metrics: m, Interval: 1, // Interval only matters for Run
		Threshold: 0.5, LatencyThresholdSeconds: 300, MinSamples: 5, BreachesToPropose: 3,
		ToolLatencyThresholdSeconds: 60, MaxSuggestedTimeoutSeconds: 300,
		Logger:          zerolog.Nop(),
		failedBreaches:  map[string]int{},
		latencyBreaches: map[string]int{},
		toolBreaches:    map[ProjectToolKey]int{},
	}
}

func TestTune_ProposesOnSustainedBreach(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{ret: map[string]RateSample{"janka": {Failed: 8, Total: 10, Rate: 0.8}}}
	w := newTuneWorker(repo, m)
	ctx := context.Background()

	// 2 breaching ticks: not yet (need 3).
	w.tick(ctx)
	w.tick(ctx)
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("must not propose before %d consecutive breaches, got %d proposals", w.breachesToPropose(), n)
	}
	// 3rd consecutive breach → one proposal.
	w.tick(ctx)
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("expected exactly 1 proposal on the 3rd breach, got %d", n)
	}
	// Continued breaching must NOT duplicate (open DRAFT dedup).
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("open DRAFT must dedup; expected still 1, got %d", n)
	}
}

func TestTune_ProposesOnSustainedLatencyBreach(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{lats: map[string]LatencySample{"janka": {P95Seconds: 420, Count: 8}}}
	w := newTuneWorker(repo, m)
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx)
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("must not propose before 3 consecutive latency breaches, got %d", n)
	}
	w.tick(ctx)
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("expected 1 latency proposal on the 3rd breach, got %d", n)
	}
	// The proposal must be the latency one (distinct from failed-rate).
	ps, _ := repo.List(ctx, persistence.ProposalListFilter{ProjectID: "janka"})
	if len(ps) != 1 || ps[0].Title != tuneLatencyTitle("janka") {
		t.Fatalf("expected the latency-titled proposal, got %+v", ps)
	}
}

func TestTune_NoProposeBelowThreshold(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{ret: map[string]RateSample{"janka": {Failed: 2, Total: 10, Rate: 0.2}}}
	w := newTuneWorker(repo, m)
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("below threshold must never propose, got %d", n)
	}
}

func TestTune_NoProposeBelowMinSamples(t *testing.T) {
	repo := newTuneTestRepo(t)
	// 100% failed but only 2 samples — too few to act on.
	m := &fakeMetrics{ret: map[string]RateSample{"janka": {Failed: 2, Total: 2, Rate: 1.0}}}
	w := newTuneWorker(repo, m)
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("below min-samples must never propose, got %d", n)
	}
}

func TestTune_BreachStreakResetsOnRecovery(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{ret: map[string]RateSample{"janka": {Failed: 8, Total: 10, Rate: 0.8}}}
	w := newTuneWorker(repo, m)
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx) // 2 breaches
	// Recovery tick resets the streak.
	m.ret = map[string]RateSample{"janka": {Failed: 1, Total: 10, Rate: 0.1}}
	w.tick(ctx)
	// Back to breaching — needs 3 fresh consecutive again.
	m.ret = map[string]RateSample{"janka": {Failed: 8, Total: 10, Rate: 0.8}}
	w.tick(ctx)
	w.tick(ctx)
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("streak must reset on recovery; expected 0 proposals, got %d", n)
	}
	w.tick(ctx) // 3rd fresh breach
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("expected 1 proposal after fresh streak, got %d", n)
	}
}

func toolProposal(t *testing.T, repo persistence.ProposalRepository) *persistence.ControlPlaneProposal {
	t.Helper()
	ps, _ := repo.List(context.Background(), persistence.ProposalListFilter{})
	for _, p := range ps {
		if p.ProposedBy == "instinct" {
			return p
		}
	}
	return nil
}

func TestInstinct_ToolTimeoutProposesWithClampedSuggestion(t *testing.T) {
	repo := newTuneTestRepo(t)
	// p95 = 400s → ceil(400*1.5)=600 > 300 cap → clamped to 300.
	m := &fakeMetrics{tools: []ToolLatencySample{
		{Key: ProjectToolKey{Project: "janka", Tool: "web_fetch"}, P95Seconds: 400, Count: 10},
	}}
	w := newTuneWorker(repo, m)
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx)
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("must not propose before 3 breaches, got %d", n)
	}
	w.tick(ctx)
	p := toolProposal(t, repo)
	if p == nil {
		t.Fatal("expected an instinct proposal after 3 sustained breaches")
	}
	if p.ProposedBy != "instinct" {
		t.Errorf("ProposedBy = %q, want instinct", p.ProposedBy)
	}
	if !contains(p.Rationale, "web_fetch") || !contains(p.Rationale, "clamped") {
		t.Errorf("rationale should name the tool + note the clamp: %q", p.Rationale)
	}
	if !contains(p.Evidence, `"suggested_timeout_s":300`) {
		t.Errorf("evidence should carry the clamped suggested timeout: %q", p.Evidence)
	}
}

func TestInstinct_ToolBelowThresholdNeverFires(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{tools: []ToolLatencySample{
		{Key: ProjectToolKey{Project: "janka", Tool: "shell"}, P95Seconds: 10, Count: 100},
	}}
	w := newTuneWorker(repo, m)
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("sub-threshold tool latency must never propose, got %d", n)
	}
}

func TestInstinct_ToolBelowMinSamplesResetsStreak(t *testing.T) {
	repo := newTuneTestRepo(t)
	key := ProjectToolKey{Project: "janka", Tool: "web_fetch"}
	m := &fakeMetrics{tools: []ToolLatencySample{{Key: key, P95Seconds: 400, Count: 10}}}
	w := newTuneWorker(repo, m)
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx) // 2 breaches
	// Intermittent load: count dips below MinSamples → treated as recovery.
	m.tools = []ToolLatencySample{{Key: key, P95Seconds: 400, Count: 2}}
	w.tick(ctx)
	// Back to breaching — must need 3 fresh consecutive.
	m.tools = []ToolLatencySample{{Key: key, P95Seconds: 400, Count: 10}}
	w.tick(ctx)
	w.tick(ctx)
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("streak must reset when count<MinSamples; got %d", n)
	}
	w.tick(ctx)
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("expected 1 proposal after fresh streak, got %d", n)
	}
}

func TestInstinct_ToolSignalDisabledByNegativeThreshold(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{tools: []ToolLatencySample{
		{Key: ProjectToolKey{Project: "janka", Tool: "web_fetch"}, P95Seconds: 999, Count: 100},
	}}
	w := newTuneWorker(repo, m)
	w.ToolLatencyThresholdSeconds = -1 // disabled
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("disabled tool signal must never propose, got %d", n)
	}
}

func TestInstinct_CompositeKeysDoNotCollide(t *testing.T) {
	// Keys that would collide under naive "|"-joining maintain independent
	// streaks (typed struct keys): project "a|b"+tool "c" vs "a"+tool "b|c".
	repo := newTuneTestRepo(t)
	k1 := ProjectToolKey{Project: "a|b", Tool: "c"}
	k2 := ProjectToolKey{Project: "a", Tool: "b|c"}
	m := &fakeMetrics{tools: []ToolLatencySample{
		{Key: k1, P95Seconds: 400, Count: 10},
		{Key: k2, P95Seconds: 400, Count: 10},
	}}
	w := newTuneWorker(repo, m)
	w.toolBreaches[k1] = 2 // k1 already at 2 breaches; k2 fresh
	w.tick(context.Background())
	// k1 hits 3 (fires); k2 only at 1 (no fire) — proves independent streaks.
	if got := w.toolBreaches[k2]; got != 1 {
		t.Fatalf("k2 streak must be independent (1), got %d", got)
	}
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("only k1 should have fired, got %d proposals", n)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
