package graph

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// fakeChunkSource serves chunks from a queue and records
// MarkExtracted calls. Concurrency-safe so tests can exercise
// MaxParallel > 1.
type fakeChunkSource struct {
	mu          sync.Mutex
	queue       []persistence.ChunkForExtraction
	marked      []string
	fetchErr    error
	markErrFor  map[string]error
	fetchCalls  atomic.Int32
	attempts    map[string]int
	failReasons map[string]string
	quarantined []string
	failErr     error
}

func (f *fakeChunkSource) FetchUnextracted(_ context.Context, limit int) ([]persistence.ChunkForExtraction, error) {
	f.fetchCalls.Add(1)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.queue)
	if n > limit {
		n = limit
	}
	out := make([]persistence.ChunkForExtraction, n)
	copy(out, f.queue[:n])
	f.queue = f.queue[n:]
	return out, nil
}

func (f *fakeChunkSource) MarkExtracted(_ context.Context, chunkID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.markErrFor[chunkID]; ok {
		return err
	}
	f.marked = append(f.marked, chunkID)
	return nil
}

// RecordExtractionFailure mirrors the real repositories: count consecutive
// failures per chunk and report quarantine once the count reaches maxAttempts.
func (f *fakeChunkSource) RecordExtractionFailure(_ context.Context, chunkID, reason string, maxAttempts int) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return 0, false, f.failErr
	}
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	if f.failReasons == nil {
		f.failReasons = map[string]string{}
	}
	f.attempts[chunkID]++
	f.failReasons[chunkID] = reason
	n := f.attempts[chunkID]
	if n >= maxAttempts {
		f.quarantined = append(f.quarantined, chunkID)
		return n, true, nil
	}
	return n, false, nil
}

func (f *fakeChunkSource) PendingCount(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue), nil
}

func (f *fakeChunkSource) Stats(context.Context) (*persistence.KGStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &persistence.KGStats{ChunksPending: len(f.queue)}, nil
}

func (f *fakeChunkSource) ReflagChunksMissingEdges(context.Context, string, bool) (int, error) {
	// Worker tests don't exercise the operator-driven backfill
	// path; return zero so the interface is satisfied.
	return 0, nil
}

// happyPipeline is a minimal Pipeline whose stages all succeed
// against any input — useful when testing the worker control
// loop without exercising the full extractor stack. Each stage
// gets its own repeating provider so the worker can process many
// chunks per tick without exhausting scripted replies.
func happyPipeline() *Pipeline {
	entRepo := &fakeEntityRepoForPipeline{}
	edgeRepo := &fakeEdgeRepo{}
	mentRepo := &fakeMentionRepo{}
	return &Pipeline{
		Extractor: NewExtractor(newRepeatingProvider(`[]`), "ex"),
		Resolver:  NewResolver(newRepeatingProvider(``), "res", entRepo, fakeEmbedder),
		Relations: NewRelationshipExtractor(newRepeatingProvider(``), "rel"),
		Validator: NewValidator(newRepeatingProvider(``), "val"),
		Entities:  entRepo,
		Edges:     edgeRepo,
		Mentions:  mentRepo,
		Embedder:  fakeEmbedder,
	}
}

// failingPipeline returns a Pipeline whose extractor always
// errors (non-retryable) so RunChunk surfaces a failure for
// every chunk.
func failingPipeline() *Pipeline {
	entRepo := &fakeEntityRepoForPipeline{}
	bad := newRepeatingErrorProvider(errors.New("permanent: fake LLM down"))
	return &Pipeline{
		Extractor: NewExtractor(bad, "ex"),
		Resolver:  NewResolver(newRepeatingProvider(``), "res", entRepo, fakeEmbedder),
		Relations: NewRelationshipExtractor(newRepeatingProvider(``), "rel"),
		Validator: NewValidator(newRepeatingProvider(``), "val"),
		Entities:  entRepo,
		Edges:     &fakeEdgeRepo{},
		Mentions:  &fakeMentionRepo{},
		Embedder:  fakeEmbedder,
	}
}

func TestWorker_TickProcessesBatchAndMarksExtracted(t *testing.T) {
	src := &fakeChunkSource{
		queue: []persistence.ChunkForExtraction{
			{ID: "c1", ProjectID: "p1", Content: "x"},
			{ID: "c2", ProjectID: "p1", Content: "y"},
		},
	}
	w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 10})
	w.tick(context.Background())

	if len(src.marked) != 2 {
		t.Errorf("expected 2 marked, got %d (%+v)", len(src.marked), src.marked)
	}
	_, processed, failed := w.LastTickStats()
	if processed != 2 || failed != 0 {
		t.Errorf("expected processed=2 failed=0, got %d/%d", processed, failed)
	}
}

func TestWorker_FailedChunkStaysFlagged(t *testing.T) {
	src := &fakeChunkSource{
		queue: []persistence.ChunkForExtraction{{ID: "c1", ProjectID: "p1", Content: "x"}},
	}
	w := NewWorker(src, failingPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	w.tick(context.Background())

	if len(src.marked) != 0 {
		t.Errorf("failed chunk must NOT be marked extracted, got %+v", src.marked)
	}
	_, processed, failed := w.LastTickStats()
	if processed != 0 || failed != 1 {
		t.Errorf("expected processed=0 failed=1, got %d/%d", processed, failed)
	}
}

func TestWorker_EmptyQueueIsNoop(t *testing.T) {
	src := &fakeChunkSource{}
	w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	w.tick(context.Background())
	_, processed, failed := w.LastTickStats()
	if processed != 0 || failed != 0 {
		t.Errorf("empty queue: expected 0/0, got %d/%d", processed, failed)
	}
}

func TestWorker_FetchErrorRecordsFailure(t *testing.T) {
	src := &fakeChunkSource{fetchErr: errors.New("DB down")}
	w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	w.tick(context.Background())
	_, _, failed := w.LastTickStats()
	if failed != 1 {
		t.Errorf("fetch error must count as failed tick, got failed=%d", failed)
	}
}

func TestWorker_CircuitBreakerTripsAndPauses(t *testing.T) {
	src := &fakeChunkSource{
		queue: []persistence.ChunkForExtraction{
			{ID: "c1", ProjectID: "p1", Content: "x"},
		},
	}
	cfg := WorkerConfig{
		BatchSize:               5,
		CircuitBreakerThreshold: 2,
		CircuitBreakerPause:     time.Hour, // long enough that we know subsequent ticks are skipped
	}
	w := NewWorker(src, failingPipeline(), zerolog.New(io.Discard), cfg)

	// Re-queue a chunk before each failing tick.
	for i := 0; i < 2; i++ {
		src.mu.Lock()
		src.queue = append(src.queue, persistence.ChunkForExtraction{ID: "cN", ProjectID: "p1", Content: "x"})
		src.mu.Unlock()
		w.tick(context.Background())
	}

	// Breaker should now be open. A subsequent tick must NOT
	// call FetchUnextracted again.
	beforeFetch := src.fetchCalls.Load()
	w.tick(context.Background())
	if src.fetchCalls.Load() != beforeFetch {
		t.Errorf("expected breaker to skip FetchUnextracted, but it was called")
	}
}

func TestWorker_BreakerResetsAfterCleanTick(t *testing.T) {
	src := &fakeChunkSource{
		queue: []persistence.ChunkForExtraction{
			{ID: "c1", ProjectID: "p1", Content: "x"},
		},
	}
	cfg := WorkerConfig{
		BatchSize:               5,
		CircuitBreakerThreshold: 100, // never trip in this test
	}
	w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), cfg)

	// First failing tick (DB error), then a clean tick.
	src.fetchErr = errors.New("DB blip")
	w.tick(context.Background())
	if w.consecutiveBad != 1 {
		t.Errorf("expected consecutiveBad=1, got %d", w.consecutiveBad)
	}
	src.fetchErr = nil
	w.tick(context.Background())
	if w.consecutiveBad != 0 {
		t.Errorf("clean tick should reset bad counter, got %d", w.consecutiveBad)
	}
}

func TestWorker_StartStopLifecycle(t *testing.T) {
	src := &fakeChunkSource{
		queue: []persistence.ChunkForExtraction{
			{ID: "c1", ProjectID: "p1", Content: "x"},
		},
	}
	cfg := WorkerConfig{
		BatchSize:    5,
		PollInterval: 50 * time.Millisecond,
	}
	w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), cfg)
	w.Start(context.Background())
	time.Sleep(120 * time.Millisecond) // allow at least one tick
	w.Stop()
	select {
	case <-w.Stopped():
	case <-time.After(time.Second):
		t.Fatal("worker did not signal Stopped in time")
	}
	if len(src.marked) == 0 {
		t.Errorf("expected at least one chunk marked after Start/Stop, got %+v", src.marked)
	}
}

func TestWorker_ParallelDoesNotDuplicate(t *testing.T) {
	chunks := make([]persistence.ChunkForExtraction, 10)
	for i := range chunks {
		chunks[i] = persistence.ChunkForExtraction{ID: "c" + string(rune('A'+i)), ProjectID: "p1", Content: "x"}
	}
	src := &fakeChunkSource{queue: chunks}
	w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 20, MaxParallel: 4})
	w.tick(context.Background())
	if len(src.marked) != 10 {
		t.Errorf("expected 10 marks under parallel mode, got %d", len(src.marked))
	}
	// Each chunk id should appear exactly once.
	seen := make(map[string]struct{}, len(src.marked))
	for _, id := range src.marked {
		if _, dup := seen[id]; dup {
			t.Errorf("duplicate mark for %s", id)
		}
		seen[id] = struct{}{}
	}
}

// truncatingPipeline always truncates in the extractor stage — the systemic
// failure this bound exists for.
func truncatingPipeline() *Pipeline {
	entRepo := &fakeEntityRepoForPipeline{}
	return &Pipeline{
		Extractor: NewExtractor(newTruncatingProvider(), "ex"),
		Resolver:  NewResolver(newRepeatingProvider(``), "res", entRepo, fakeEmbedder),
		Relations: NewRelationshipExtractor(newRepeatingProvider(``), "rel"),
		Validator: NewValidator(newRepeatingProvider(``), "val"),
		Entities:  entRepo,
		Edges:     &fakeEdgeRepo{},
		Mentions:  &fakeMentionRepo{},
		Embedder:  fakeEmbedder,
	}
}

// TestWorker_QuarantinesAfterRepeatedTruncation pins the bound that makes
// treating truncation as an error safe.
//
// Before 2026-08-05 a truncated completion was laundered into "no entities
// found" and the chunk marked extracted — silent, permanent data loss.
// Reporting it honestly fixes that, but needs_graph_extraction is a boolean
// with no memory, so on its own the fix would re-attempt the same ~83% of
// chunks every tick at a full token budget: a silent data-loss bug traded for
// a loud spend bug. The counter is what makes the honest failure affordable.
func TestWorker_QuarantinesAfterRepeatedTruncation(t *testing.T) {
	src := &fakeChunkSource{}
	w := NewWorker(src, truncatingPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})

	const chunkID = "c-truncating"
	chunk := persistence.ChunkForExtraction{ID: chunkID, ProjectID: "p1", Content: "names Ada"}
	for attempt := 1; attempt <= MaxExtractionAttempts; attempt++ {
		w.runOne(context.Background(), chunk)
	}

	src.mu.Lock()
	defer src.mu.Unlock()
	if got := src.attempts[chunkID]; got != MaxExtractionAttempts {
		t.Errorf("attempts = %d, want %d", got, MaxExtractionAttempts)
	}
	if len(src.quarantined) != 1 || src.quarantined[0] != chunkID {
		t.Errorf("chunk must be quarantined once the bound is reached, got %v", src.quarantined)
	}
	// Never silently marked done — that is the laundering this replaces.
	for _, id := range src.marked {
		if id == chunkID {
			t.Fatal("a truncated chunk must never be marked extracted")
		}
	}
	if src.failReasons[chunkID] == "" {
		t.Error("the failure reason must be retained so the gap is diagnosable")
	}
}

// TestWorker_SingleFailureStaysFlagged — under the bound the chunk keeps its
// flag, so a genuinely transient failure still self-heals on the next tick.
func TestWorker_SingleFailureStaysFlagged(t *testing.T) {
	src := &fakeChunkSource{}
	w := NewWorker(src, truncatingPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})

	w.runOne(context.Background(), persistence.ChunkForExtraction{ID: "c1", ProjectID: "p1", Content: "x"})

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.quarantined) != 0 {
		t.Errorf("one failure must not quarantine, got %v", src.quarantined)
	}
	if src.attempts["c1"] != 1 {
		t.Errorf("attempts = %d, want 1", src.attempts["c1"])
	}
}
