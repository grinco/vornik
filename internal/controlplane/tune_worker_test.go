package controlplane

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// fakeMetrics returns a scripted rate per tick.
type fakeMetrics struct{ ret map[string]RateSample }

func (f *fakeMetrics) FailedTaskRates(_ context.Context) (map[string]RateSample, error) {
	return f.ret, nil
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
		Threshold: 0.5, MinSamples: 5, BreachesToPropose: 3,
		Logger:   zerolog.Nop(),
		breaches: map[string]int{},
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
