package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// fakeIngestQueue is a minimal in-memory IngestQueueRepository for the
// worker tests. We don't reach for a real Postgres: the worker's
// behaviour is interesting at the goroutine + state-machine level,
// not at the SQL level (the SQL is exercised in integration tests).
type fakeIngestQueue struct {
	mu      sync.Mutex
	items   map[string]*persistence.IngestQueueItem
	failNow atomic.Bool // when true, ClaimBatch returns an error (simulates DB outage)
}

func newFakeIngestQueue() *fakeIngestQueue {
	return &fakeIngestQueue{items: make(map[string]*persistence.IngestQueueItem)}
}

func (f *fakeIngestQueue) Enqueue(ctx context.Context, item *persistence.IngestQueueItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item.ID == "" {
		item.ID = fmt.Sprintf("ingq_%d", len(f.items)+1)
	}
	if item.State == "" {
		item.State = "queued"
	}
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now()
	}
	clone := *item
	f.items[item.ID] = &clone
	return nil
}

func (f *fakeIngestQueue) ClaimBatch(ctx context.Context, projectID string, limit int) ([]*persistence.IngestQueueItem, error) {
	if f.failNow.Load() {
		return nil, errors.New("simulated DB outage")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*persistence.IngestQueueItem
	for _, item := range f.items {
		if item.ProjectID != projectID || item.State != "queued" {
			continue
		}
		item.State = "processing"
		now := time.Now()
		item.StartedAt = &now
		item.Attempts++
		clone := *item
		out = append(out, &clone)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeIngestQueue) MarkDone(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		it.State = "done"
		now := time.Now()
		it.FinishedAt = &now
	}
	return nil
}

func (f *fakeIngestQueue) MarkFailed(ctx context.Context, id string, maxAttempts int, errMsg string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return false, nil
	}
	terminal := int(it.Attempts) >= maxAttempts
	if terminal {
		it.State = "failed"
		now := time.Now()
		it.FinishedAt = &now
	} else {
		it.State = "queued"
	}
	it.LastError = &errMsg
	return terminal, nil
}

func (f *fakeIngestQueue) ProjectsWithQueued(ctx context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, it := range f.items {
		if it.State != "queued" || seen[it.ProjectID] {
			continue
		}
		seen[it.ProjectID] = true
		out = append(out, it.ProjectID)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeIngestQueue) QueueDepth(ctx context.Context, projectID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, it := range f.items {
		if it.ProjectID == projectID && (it.State == "queued" || it.State == "processing") {
			n++
		}
	}
	return n, nil
}

func (f *fakeIngestQueue) ResetStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	threshold := time.Now().Add(-olderThan)
	reset := 0
	for _, it := range f.items {
		if it.State != "processing" || it.StartedAt == nil {
			continue
		}
		if it.StartedAt.Before(threshold) {
			it.State = "queued"
			it.StartedAt = nil
			reset++
		}
	}
	return reset, nil
}

func (f *fakeIngestQueue) CountStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	threshold := time.Now().Add(-olderThan)
	n := 0
	for _, it := range f.items {
		if it.State == "processing" && it.StartedAt != nil && it.StartedAt.Before(threshold) {
			n++
		}
	}
	return n, nil
}

func (f *fakeIngestQueue) get(id string) *persistence.IngestQueueItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		clone := *it
		return &clone
	}
	return nil
}

// fakeArtifactRepo implements memory.ArtifactReader (Get only) — the
// worker doesn't need the full ArtifactRepository surface.
type fakeArtifactRepo struct {
	artifacts map[string]*persistence.Artifact
}

func newFakeArtifactRepo() *fakeArtifactRepo {
	return &fakeArtifactRepo{artifacts: make(map[string]*persistence.Artifact)}
}

func (f *fakeArtifactRepo) Get(ctx context.Context, id string) (*persistence.Artifact, error) {
	if a, ok := f.artifacts[id]; ok {
		return a, nil
	}
	return nil, nil
}

// fakeIndexer captures the IngestText calls so tests can assert
// on what the worker forwarded.
type fakeIndexerForWorker struct {
	mu    sync.Mutex
	calls []ingestCall
	err   error
}

type ingestCall struct {
	projectID, taskID, artifactID, sourceName, content string
}

func (f *fakeIndexerForWorker) IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, ingestCall{projectID, taskID, artifactID, sourceName, content})
	return nil
}

// workerHarness runs an IngestWorker against a stubbed indexer
// adapter. The real Indexer pulls in chunking + DB writes; for the
// worker's contract we only care that IngestText was called with the
// right args, so we implement the same surface in a tiny wrapper.
type workerHarness struct {
	queue  *fakeIngestQueue
	arts   *fakeArtifactRepo
	stub   *fakeIndexerForWorker
	worker *IngestWorker
	tmpDir string
}

func newWorkerHarness(t *testing.T) *workerHarness {
	t.Helper()
	queue := newFakeIngestQueue()
	arts := newFakeArtifactRepo()
	stub := &fakeIndexerForWorker{}
	tmpDir := t.TempDir()
	w := newIngestWorkerForTest(queue, arts, stub, IngestWorkerConfig{
		PollInterval:            20 * time.Millisecond,
		MaxBatchPerProject:      8,
		MaxAttempts:             2,
		MaxProjectsPerTick:      8,
		CircuitBreakerThreshold: 100, // disable for normal tests
		CircuitBreakerPause:     time.Second,
	})
	return &workerHarness{queue: queue, arts: arts, stub: stub, worker: w, tmpDir: tmpDir}
}

// addArtifact creates a fake artifact + writes its content under
// the harness's tmp dir. Returns the artifact ID.
func (h *workerHarness) addArtifact(t *testing.T, projectID, name, content string) string {
	t.Helper()
	id := fmt.Sprintf("art_%d", len(h.arts.artifacts)+1)
	path := filepath.Join(h.tmpDir, id+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	taskID := "task_x"
	h.arts.artifacts[id] = &persistence.Artifact{
		ID:            id,
		ProjectID:     projectID,
		Name:          name,
		StoragePath:   path,
		ArtifactClass: persistence.ArtifactClassOutput,
		TaskID:        &taskID,
	}
	return id
}

// enqueue creates a fresh queue row pointing at an artifact.
func (h *workerHarness) enqueue(t *testing.T, projectID, artifactID, role string) string {
	t.Helper()
	item := &persistence.IngestQueueItem{
		ProjectID:        projectID,
		SourceArtifactID: artifactID,
		ProducerRole:     role,
		Priority:         50,
	}
	if err := h.queue.Enqueue(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return item.ID
}

// TestIngestWorker_DrainsHappyPath verifies the basic contract: a
// single enqueued item is picked up within one tick, the indexer is
// called with the artifact's content, and the queue row goes 'done'.
func TestIngestWorker_DrainsHappyPath(t *testing.T) {
	h := newWorkerHarness(t)
	artID := h.addArtifact(t, "proj-A", "research.md", "# heading\nsome body\n")
	itemID := h.enqueue(t, "proj-A", artID, "researcher")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.worker.Start(ctx)
	defer h.worker.Stop()

	// Poll up to 500ms for completion.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.queue.get(itemID).State == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.queue.get(itemID).State; got != "done" {
		t.Fatalf("queue item state = %q, want done", got)
	}

	h.stub.mu.Lock()
	defer h.stub.mu.Unlock()
	if len(h.stub.calls) != 1 {
		t.Fatalf("expected 1 IngestText call, got %d", len(h.stub.calls))
	}
	c := h.stub.calls[0]
	if c.projectID != "proj-A" || c.artifactID != artID || c.sourceName != "research.md" {
		t.Errorf("unexpected IngestText args: %+v", c)
	}
	if c.content != "# heading\nsome body\n" {
		t.Errorf("content mismatch: %q", c.content)
	}
}

// TestIngestWorker_RetryThenSucceed simulates a transient indexer
// failure on the first attempt. The worker re-queues, the next tick
// claims again (attempts=2), and this time it succeeds.
func TestIngestWorker_RetryThenSucceed(t *testing.T) {
	h := newWorkerHarness(t)
	artID := h.addArtifact(t, "proj-B", "x.md", "body")
	itemID := h.enqueue(t, "proj-B", artID, "writer")

	// Inject failure for the first IngestText call only.
	var firstCall sync.Once
	failOnce := errors.New("transient")
	h.stub.err = failOnce
	go func() {
		// Clear the error after the first observed attempt so the
		// next tick succeeds.
		for {
			h.stub.mu.Lock()
			if len(h.stub.calls) > 0 {
				firstCall.Do(func() { h.stub.err = nil })
			}
			h.stub.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			if h.queue.get(itemID).State == "done" {
				return
			}
		}
	}()

	// Wait — but this fake setup calls IngestText only when err is
	// nil. We need a different shape: pre-set the failure to apply
	// once via a counter.
	// Reset: install a counter-based failure.
	h.stub.mu.Lock()
	h.stub.calls = nil
	h.stub.err = nil
	h.stub.mu.Unlock()

	calls := atomic.Int32{}
	prevImpl := func(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error {
		n := calls.Add(1)
		if n == 1 {
			return errors.New("transient")
		}
		h.stub.mu.Lock()
		h.stub.calls = append(h.stub.calls, ingestCall{projectID, taskID, artifactID, sourceName, content})
		h.stub.mu.Unlock()
		return nil
	}
	// Swap in a counter-based stub and rebuild the worker.
	h.worker.Stop()
	stub2 := &counterIndexer{ingest: prevImpl}
	h.worker = newIngestWorkerForTest(h.queue, h.arts, stub2, IngestWorkerConfig{
		PollInterval:       20 * time.Millisecond,
		MaxBatchPerProject: 8,
		MaxAttempts:        3,
	})
	// Re-queue (it went to "processing" then maybe "queued" via MarkFailed
	// when the first failing attempt occurred above; re-enqueue cleanly).
	h.queue.mu.Lock()
	h.queue.items[itemID].State = "queued"
	h.queue.items[itemID].Attempts = 0
	h.queue.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.worker.Start(ctx)
	defer h.worker.Stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.queue.get(itemID).State == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.queue.get(itemID).State; got != "done" {
		t.Fatalf("queue item state = %q, want done", got)
	}
	if calls.Load() < 2 {
		t.Errorf("expected ≥2 attempts (one failure + one success), got %d", calls.Load())
	}
}

// TestIngestWorker_TerminalAfterMaxAttempts confirms the worker
// stops retrying once attempts >= MaxAttempts and the item lands
// in 'failed'.
func TestIngestWorker_TerminalAfterMaxAttempts(t *testing.T) {
	h := newWorkerHarness(t)
	artID := h.addArtifact(t, "proj-C", "y.md", "body")
	itemID := h.enqueue(t, "proj-C", artID, "writer")

	// Always fail.
	stub := &counterIndexer{ingest: func(_ context.Context, _, _, _, _, _ string) error {
		return errors.New("permanent")
	}}
	h.worker.Stop()
	h.worker = newIngestWorkerForTest(h.queue, h.arts, stub, IngestWorkerConfig{
		PollInterval:       20 * time.Millisecond,
		MaxBatchPerProject: 8,
		MaxAttempts:        2,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.worker.Start(ctx)
	defer h.worker.Stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.queue.get(itemID).State == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.queue.get(itemID).State; got != "failed" {
		t.Fatalf("queue item state = %q, want failed", got)
	}
	if got := h.queue.get(itemID).LastError; got == nil || *got == "" {
		t.Errorf("last_error not recorded on terminal failure")
	}
}

// counterIndexer adapts a function to the workerIndexer surface.
type counterIndexer struct {
	ingest func(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error
}

func (c *counterIndexer) IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error {
	return c.ingest(ctx, projectID, taskID, artifactID, sourceName, content)
}

// newIngestWorkerForTest constructs a worker against a workerIndexer
// (interface-typed) instead of *Indexer. The production constructor
// takes *Indexer because that's the only producer in the live
// system; tests need to inject stubs without standing up a real
// chunker + repo.
func newIngestWorkerForTest(
	queue persistence.IngestQueueRepository,
	arts ArtifactReader,
	idx workerIndexer,
	cfg IngestWorkerConfig,
) *IngestWorker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.MaxBatchPerProject <= 0 {
		cfg.MaxBatchPerProject = 16
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.MaxProjectsPerTick <= 0 {
		cfg.MaxProjectsPerTick = 64
	}
	if cfg.CircuitBreakerThreshold <= 0 {
		cfg.CircuitBreakerThreshold = 5
	}
	if cfg.CircuitBreakerPause <= 0 {
		cfg.CircuitBreakerPause = 60 * time.Second
	}
	w := &IngestWorker{
		repo:     queue,
		artifact: arts,
		indexer:  nil, // set via testIndexer below
		logger:   zerolog.Nop(),
		cfg:      cfg,
		stopped:  make(chan struct{}),
	}
	w.testIndexer = idx
	return w
}

// workerIndexer is the surface of *Indexer the worker actually
// uses. Tests inject this via testIndexer to bypass the real
// chunker + repo.
type workerIndexer interface {
	IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error
}
