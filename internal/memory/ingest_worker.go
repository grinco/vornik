package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// IngestWorker drains project_ingest_queue, fetches the source
// artifact's content for each claimed item, and ingests it into
// project memory via the existing Indexer.IngestText path.
//
// Phase 1 — synchronous IngestText, bypassing the rag-ingest swarm.
// Phase 2 swaps the IngestText call for a rag-ingest task dispatch
// (see https://docs.vornik.io §11).
//
// Failure handling:
//   - read-artifact failures (file gone, no path) mark the row
//     terminal-failed on the first attempt; a missing artifact won't
//     resolve on retry,
//   - chunk-insert failures (DB transient, embedder downstream
//     hiccup) re-queue with attempts++; terminal after attempts ≥
//     max_attempts (default 3),
//   - circuit-break: 5 consecutive batches with a non-zero failure
//     count pause the worker for 60s and emit a warn-level log
//     (operator can see "stuck ingest" without polling).
//
// One worker handles all projects; rounds robin across projects
// with queued work each tick. Per-project parallelism would land
// later if a project's queue depth justifies it.
// IngestTextWriter is the surface of *Indexer the worker actually
// uses. Carved as an interface so tests can inject stubs without
// standing up a real chunker + DB-backed repo, and so future Phase-2
// dispatch (rag-ingest task) can swap in another implementation
// transparently.
type IngestTextWriter interface {
	IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error
}

// ArtifactReader is the narrow read-side surface of the artifact
// repository the worker needs. We don't depend on the full
// persistence.ArtifactRepository (Create/Delete/UpdateTaskID/etc)
// because the worker only ever loads existing artifacts to pipe
// their content into the indexer.
type ArtifactReader interface {
	Get(ctx context.Context, id string) (*persistence.Artifact, error)
}

// ArtifactBlobReader is the narrow interface IngestWorker uses to
// read artifact BYTES via the backend-aware Store. Separated from
// ArtifactReader so test fakes that already implement the metadata
// Get aren't forced to grow a Retrieve they don't exercise. Wire
// *artifacts.Store via WithArtifactBlobReader to make S3-backed
// memory ingestion work; absent reader keeps the legacy direct-disk
// path alive for filesystem-only deployments + tests.
type ArtifactBlobReader interface {
	Retrieve(ctx context.Context, artifactID string) ([]byte, error)
}

type IngestWorker struct {
	repo     persistence.IngestQueueRepository
	artifact ArtifactReader
	// blob (optional) reads artifact bytes via the backend-aware
	// Store. Nil falls back to os.ReadFile on artifact.StoragePath
	// — correct only on the filesystem backend.
	blob    ArtifactBlobReader
	indexer IngestTextWriter
	// testIndexer overrides indexer when set. Production code never
	// touches this field; ingest_worker_test.go uses it to bypass
	// the real Indexer's chunker + repo.
	testIndexer IngestTextWriter
	// pipeline (Phase 2) — when non-nil, processItem routes through
	// the gate stack + quarantine instead of calling IngestText
	// directly. Nil-safe; absent pipeline = legacy direct path.
	pipeline *Pipeline
	logger   zerolog.Logger
	metrics  *Metrics

	cfg     IngestWorkerConfig
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped chan struct{}

	mu             sync.Mutex
	consecutiveBad int
	pausedUntil    time.Time
}

// IngestWorkerConfig tunes the drain loop. Zero values pick safe
// defaults.
type IngestWorkerConfig struct {
	// Interval between drain ticks. Default 5s — matches the
	// design's Phase 1 acceptance ("queue drains within 5s").
	PollInterval time.Duration
	// MaxBatchPerProject caps how many items one tick processes
	// per project. Bounded so a 10k-row queue doesn't starve other
	// projects on a single tick. Default 16.
	MaxBatchPerProject int
	// MaxAttempts is the per-item retry budget before terminal
	// failure. Matches scheduler convention. Default 3.
	MaxAttempts int
	// MaxProjectsPerTick caps how many projects one tick visits.
	// Default 64 (matches ProjectsWithQueued's default page).
	MaxProjectsPerTick int
	// CircuitBreakerThreshold — pause after this many consecutive
	// bad ticks (each bad tick = at least one item failed). Default 5.
	CircuitBreakerThreshold int
	// CircuitBreakerPause — how long to stay paused once tripped.
	// Default 60s. The next tick after the pause unwedges (and the
	// counter resets when a tick produces zero failures).
	CircuitBreakerPause time.Duration
}

// NewIngestWorker constructs a worker but does not start it. Call
// Start to begin draining.
func NewIngestWorker(
	repo persistence.IngestQueueRepository,
	artifact ArtifactReader,
	indexer *Indexer,
	logger zerolog.Logger,
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
	return &IngestWorker{
		repo:     repo,
		artifact: artifact,
		indexer:  indexer,
		logger:   logger,
		cfg:      cfg,
		stopped:  make(chan struct{}),
	}
}

// SetMetrics wires the metrics sink. Optional — nil is a no-op.
func (w *IngestWorker) SetMetrics(m *Metrics) { w.metrics = m }

// SetPipeline routes processing through the Phase 2 gate-and-quarantine
// pipeline instead of calling IngestText directly. Nil disables.
func (w *IngestWorker) SetPipeline(p *Pipeline) { w.pipeline = p }

// SetArtifactBlobReader wires the backend-aware reader so ingest
// reads route through artifacts.Store.Retrieve. When unset (or
// nil), the worker falls back to os.ReadFile on StoragePath — the
// pre-phase-4 behaviour, correct only on the filesystem backend.
func (w *IngestWorker) SetArtifactBlobReader(r ArtifactBlobReader) { w.blob = r }

// readArtifactBytes reads an artifact's body via the backend-aware
// Store when wired, falling back to the legacy os.ReadFile path on
// the recorded StoragePath. Centralised so both processItemWithStats
// and processItem share the routing logic.
func (w *IngestWorker) readArtifactBytes(ctx context.Context, artifactID, storagePath string) ([]byte, error) {
	if w.blob != nil {
		return w.blob.Retrieve(ctx, artifactID)
	}
	return os.ReadFile(storagePath)
}

// StaleProcessingThreshold is the age threshold for "this row was
// claimed but never finished, almost certainly by a now-dead worker".
// 5 minutes is a comfortable margin over the longest plausible
// single-item processing time (chunking + embedding + DB write is
// well under a minute end-to-end) while still acting fast after a
// crash. Used both by the startup sweep and the per-tick gauge.
const StaleProcessingThreshold = 5 * time.Minute

// Start launches the drain loop. Returns immediately. Call Stop to
// terminate.
//
// Before the goroutine launches we run a best-effort sweep over
// project_ingest_queue resetting any row that's been 'processing'
// longer than StaleProcessingThreshold back to 'queued'. That's how
// we recover from the previous-incarnation-crashed-mid-batch case —
// without this, rows the dead worker claimed stay 'processing'
// forever (ClaimBatch only picks 'queued') and the queue's depth
// gauge reads non-zero with no progress being made.
func (w *IngestWorker) Start(parent context.Context) {
	if w.repo != nil {
		sweepCtx, cancel := context.WithTimeout(parent, 10*time.Second)
		reset, err := w.repo.ResetStaleProcessing(sweepCtx, StaleProcessingThreshold)
		cancel()
		switch {
		case err != nil:
			w.logger.Warn().Err(err).
				Dur("threshold", StaleProcessingThreshold).
				Msg("ingest worker: startup sweep failed — leaving any stuck rows for the next restart")
		case reset > 0:
			w.logger.Warn().
				Int("rows_reset", reset).
				Dur("threshold", StaleProcessingThreshold).
				Msg("ingest worker: startup sweep recovered stale processing rows from a previous crash")
		default:
			w.logger.Debug().
				Dur("threshold", StaleProcessingThreshold).
				Msg("ingest worker: startup sweep found no stale rows")
		}
	}

	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.wg.Add(1)
	go w.run(ctx)
	w.logger.Info().
		Dur("poll_interval", w.cfg.PollInterval).
		Int("max_batch_per_project", w.cfg.MaxBatchPerProject).
		Int("max_attempts", w.cfg.MaxAttempts).
		Msg("memory ingest worker started")
}

// Stop signals the drain loop to exit and waits for it.
func (w *IngestWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	close(w.stopped)
	w.logger.Info().Msg("memory ingest worker stopped")
}

// Stopped returns a channel closed when the worker has fully
// stopped. Tests use it to synchronise without polling.
func (w *IngestWorker) Stopped() <-chan struct{} { return w.stopped }

// run is the drain loop. One goroutine handles every project.
// Adding per-project goroutines would speed up high-queue-depth
// projects but the simple loop is enough at our scale; revisit
// if depth metrics show contention.
func (w *IngestWorker) run(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	// Tick once immediately so a stale queue at boot drains without
	// waiting a full PollInterval.
	w.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick is one full pass: drain every project with queued work, up
// to per-project + per-tick caps. Respects the circuit breaker.
func (w *IngestWorker) tick(ctx context.Context) {
	if w.circuitOpen() {
		return
	}

	// Refresh the stale-processing gauge. Healthy operation has this
	// at 0; non-zero values fire the alert. We do this every tick
	// because the startup sweep only catches what's stuck at boot —
	// a worker wedged on a single item after startup still needs to
	// surface here.
	w.updateStaleProcessingGauge(ctx)

	projects, err := w.repo.ProjectsWithQueued(ctx, w.cfg.MaxProjectsPerTick)
	if err != nil {
		w.logger.Warn().Err(err).Msg("ingest worker: list projects with queued work failed")
		return
	}
	if len(projects) == 0 {
		w.recordTickClean()
		return
	}

	totalProcessed, totalFailed := 0, 0
	for _, projectID := range projects {
		if ctx.Err() != nil {
			return
		}
		processed, failed := w.drainProject(ctx, projectID)
		totalProcessed += processed
		totalFailed += failed
	}

	if totalFailed > 0 {
		w.recordTickWithFailures(totalFailed)
		w.logger.Warn().
			Int("processed", totalProcessed).
			Int("failed", totalFailed).
			Int("projects", len(projects)).
			Msg("ingest worker: tick finished with failures")
	} else {
		w.recordTickClean()
		if totalProcessed > 0 {
			w.logger.Debug().
				Int("processed", totalProcessed).
				Int("projects", len(projects)).
				Msg("ingest worker: tick complete")
		}
	}
}

// drainProject claims and processes one batch for one project.
// Returns (processed, failed) item counts.
//
// Phase 3: when the pipeline + epoch repo are wired, the whole
// batch lands in one named epoch — opens at claim time, closes +
// activates at drain end with rollup counts. Empty epochs (every
// item rejected/quarantined) are closed but not activated so the
// active set stays clean.
func (w *IngestWorker) drainProject(ctx context.Context, projectID string) (int, int) {
	items, err := w.repo.ClaimBatch(ctx, projectID, w.cfg.MaxBatchPerProject)
	if err != nil {
		w.logger.Warn().Err(err).Str("project_id", projectID).Msg("ingest worker: claim batch failed")
		return 0, 1
	}
	if len(items) == 0 {
		return 0, 0
	}

	// Open one epoch per drain. The pipeline tags every admitted
	// chunk in this batch with the epoch_id; the close+activate
	// at the end makes the snapshot atomically visible to search.
	var epochID string
	if w.pipeline != nil {
		var bErr error
		epochID, bErr = w.pipeline.BeginEpoch(ctx, projectID, "", "ingest worker drain")
		if bErr != nil {
			w.logger.Warn().Err(bErr).Str("project_id", projectID).Msg("ingest worker: BeginEpoch failed; proceeding without epoch tag")
		}
	}

	processed, failed := 0, 0
	totals := persistence.CorpusEpochCounts{}
	for _, item := range items {
		if ctx.Err() != nil {
			break
		}
		stats, perr := w.processItemWithStats(ctx, item, epochID)
		totals.Admitted += stats.Admitted
		totals.Quarantined += stats.Quarantined
		if perr != nil {
			failed++
			terminal, mfErr := w.repo.MarkFailed(ctx, item.ID, w.cfg.MaxAttempts, perr.Error())
			if mfErr != nil {
				w.logger.Warn().Err(mfErr).Str("ingest_id", item.ID).Msg("ingest worker: MarkFailed errored")
			}
			level := w.logger.Warn()
			if terminal {
				level = w.logger.Error()
				if w.metrics != nil {
					w.metrics.IngestQueueTerminalFailuresTotal.WithLabelValues(item.ProjectID).Inc()
				}
			}
			level.Err(perr).
				Str("project_id", item.ProjectID).
				Str("ingest_id", item.ID).
				Str("source_artifact_id", item.SourceArtifactID).
				Str("producer_role", item.ProducerRole).
				Int16("attempts", item.Attempts).
				Bool("terminal", terminal).
				Msg("ingest worker: item failed")
			continue
		}
		processed++
		if mdErr := w.repo.MarkDone(ctx, item.ID); mdErr != nil {
			w.logger.Warn().Err(mdErr).Str("ingest_id", item.ID).Msg("ingest worker: MarkDone errored")
		}
		if w.metrics != nil {
			w.metrics.IngestQueueProcessedTotal.WithLabelValues(item.ProjectID).Inc()
		}
	}

	// Close + activate the epoch. Empty-admitted epochs close
	// without activation so the active set stays clean.
	if w.pipeline != nil && epochID != "" {
		if cErr := w.pipeline.CloseAndActivateEpoch(ctx, projectID, epochID, totals); cErr != nil {
			w.logger.Warn().Err(cErr).
				Str("project_id", projectID).
				Str("epoch_id", epochID).
				Msg("ingest worker: CloseAndActivateEpoch failed (non-fatal)")
		}
	}

	if w.metrics != nil {
		// Update the per-project depth gauge once per drain.
		if depth, qerr := w.repo.QueueDepth(ctx, projectID); qerr == nil {
			w.metrics.IngestQueueDepth.WithLabelValues(projectID).Set(float64(depth))
		}
	}

	return processed, failed
}

// processItemWithStats wraps processItem to also return the
// pipeline's per-item stats. The legacy direct-IngestText path
// returns zeroed stats (1 admitted on success, 0 otherwise) so
// epoch counts are still meaningful in the no-pipeline case.
func (w *IngestWorker) processItemWithStats(ctx context.Context, item *persistence.IngestQueueItem, epochID string) (IngestStats, error) {
	if item == nil {
		return IngestStats{}, errors.New("nil item")
	}
	if w.pipeline == nil || w.testIndexer != nil {
		err := w.processItem(ctx, item)
		if err != nil {
			return IngestStats{}, err
		}
		return IngestStats{Admitted: 1}, nil
	}
	art, err := w.artifact.Get(ctx, item.SourceArtifactID)
	if err != nil {
		return IngestStats{}, fmt.Errorf("artifact get: %w", err)
	}
	if art == nil {
		return IngestStats{}, errors.New("artifact not found")
	}
	if art.StoragePath == "" {
		return IngestStats{}, errors.New("artifact has no storage_path")
	}
	content, err := w.readArtifactBytes(ctx, item.SourceArtifactID, art.StoragePath)
	if err != nil {
		return IngestStats{}, fmt.Errorf("read artifact: %w", err)
	}
	if len(content) == 0 {
		return IngestStats{}, nil
	}
	taskID := ""
	if art.TaskID != nil {
		taskID = *art.TaskID
	}
	execID := ""
	if item.IngestExecutionID != nil {
		execID = *item.IngestExecutionID
	}
	sourceSize := int64(0)
	if art.SizeBytes != nil {
		sourceSize = *art.SizeBytes
	}
	// Pass repo_scope through to the pipeline so chunks land tagged.
	// Migration 75/76: item.RepoScope (set by the executor at enqueue
	// from task.payload.repo_scope) becomes opts.RepoScope which
	// IngestArtifactWithOptions stamps on the candidate.
	opts := IngestArtifactOptions{}
	if item.RepoScope != nil {
		opts.RepoScope = *item.RepoScope
	}
	return w.pipeline.IngestArtifactWithOptions(
		ctx, item.ProjectID, taskID, item.SourceArtifactID, art.Name,
		item.ProducerRole, execID, string(content), sourceSize, epochID,
		opts,
	)
}

// processItem reads the artifact and pipes the content through the
// existing Indexer.IngestText. Phase 1 keeps producer-side classification
// simple — proposed_class is passed through to the indexer's source_name
// (no use yet) so Phase 2's classifier sees what the producer suggested.
func (w *IngestWorker) processItem(ctx context.Context, item *persistence.IngestQueueItem) error {
	if item == nil {
		return errors.New("nil item")
	}
	art, err := w.artifact.Get(ctx, item.SourceArtifactID)
	if err != nil {
		return fmt.Errorf("artifact get: %w", err)
	}
	if art == nil {
		return errors.New("artifact not found")
	}
	if art.StoragePath == "" {
		return errors.New("artifact has no storage_path")
	}
	content, err := w.readArtifactBytes(ctx, item.SourceArtifactID, art.StoragePath)
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if len(content) == 0 {
		// Empty content isn't a hard failure — the indexer would
		// silently drop it. Mark done so we don't retry.
		return nil
	}

	taskID := ""
	if art.TaskID != nil {
		taskID = *art.TaskID
	}
	execID := ""
	if item.IngestExecutionID != nil {
		execID = *item.IngestExecutionID
	}
	sourceSize := int64(0)
	if art.SizeBytes != nil {
		sourceSize = *art.SizeBytes
	}

	// processItem is the legacy / test path (pipeline=nil OR
	// testIndexer set). Pipeline path runs through
	// processItemWithStats above; this is reached only when no
	// pipeline is wired or under tests.
	_ = execID
	_ = sourceSize
	idx := w.indexer
	if w.testIndexer != nil {
		idx = w.testIndexer
	}
	if err := idx.IngestText(ctx, item.ProjectID, taskID, item.SourceArtifactID, art.Name, string(content)); err != nil {
		return fmt.Errorf("indexer.IngestText: %w", err)
	}
	return nil
}

// updateStaleProcessingGauge refreshes the
// memory_ingest_queue_stale_processing gauge from the DB. Best-effort:
// a query failure leaves the previous reading in place rather than
// resetting to zero (resetting on error would mask the very situation
// the gauge is meant to surface).
func (w *IngestWorker) updateStaleProcessingGauge(ctx context.Context) {
	if w.metrics == nil || w.repo == nil {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := w.repo.CountStaleProcessing(queryCtx, StaleProcessingThreshold)
	if err != nil {
		w.logger.Debug().Err(err).Msg("ingest worker: stale-processing count failed; gauge not updated")
		return
	}
	w.metrics.IngestQueueStaleProcessing.Set(float64(n))
}

// circuitOpen reports whether the breaker is currently tripped.
func (w *IngestWorker) circuitOpen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pausedUntil.IsZero() {
		return false
	}
	if time.Now().Before(w.pausedUntil) {
		return true
	}
	// Pause expired — reset.
	w.pausedUntil = time.Time{}
	w.consecutiveBad = 0
	w.logger.Info().Msg("ingest worker: circuit breaker reset, resuming")
	return false
}

// recordTickClean clears the bad-tick counter.
func (w *IngestWorker) recordTickClean() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.consecutiveBad = 0
}

// recordTickWithFailures increments the bad-tick counter and trips
// the breaker on threshold.
func (w *IngestWorker) recordTickWithFailures(failed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.consecutiveBad++
	if w.consecutiveBad >= w.cfg.CircuitBreakerThreshold {
		w.pausedUntil = time.Now().Add(w.cfg.CircuitBreakerPause)
		w.logger.Error().
			Int("consecutive_bad", w.consecutiveBad).
			Int("last_failed_count", failed).
			Dur("pause_for", w.cfg.CircuitBreakerPause).
			Msg("ingest worker: circuit breaker tripped — pausing drain loop")
		if w.metrics != nil {
			w.metrics.IngestQueueCircuitTripped.Inc()
		}
	}
}
