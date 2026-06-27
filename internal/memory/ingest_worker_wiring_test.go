package memory

import (
	"testing"

	"github.com/rs/zerolog"
)

// TestNewIngestWorker_DefaultsApplied — the production constructor
// (vs newIngestWorkerForTest) applies all zero-value defaults and
// hands back a worker with the trivial setters wired.
func TestNewIngestWorker_DefaultsApplied(t *testing.T) {
	queue := newFakeIngestQueue()
	arts := newFakeArtifactRepo()
	idx := &Indexer{logger: zerolog.Nop()}

	w := NewIngestWorker(queue, arts, idx, zerolog.Nop(), IngestWorkerConfig{})
	if w == nil {
		t.Fatal("constructor returned nil")
	}
	if w.cfg.PollInterval == 0 || w.cfg.MaxBatchPerProject == 0 ||
		w.cfg.MaxAttempts == 0 || w.cfg.MaxProjectsPerTick == 0 ||
		w.cfg.CircuitBreakerThreshold == 0 || w.cfg.CircuitBreakerPause == 0 {
		t.Fatalf("defaults not applied: %+v", w.cfg)
	}
	if w.repo == nil || w.artifact == nil || w.indexer != idx {
		t.Fatal("dependencies not wired")
	}

	// SetMetrics + SetPipeline + Stopped.
	w.SetMetrics(freshMetrics())
	if w.metrics == nil {
		t.Fatal("metrics not set")
	}
	w.SetPipeline(&Pipeline{})
	if w.pipeline == nil {
		t.Fatal("pipeline not set")
	}
	if w.Stopped() == nil {
		t.Fatal("stopped channel should be non-nil")
	}
}
