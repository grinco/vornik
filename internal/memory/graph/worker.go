package graph

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Worker drains the project_memory_chunks rows where
// needs_graph_extraction = TRUE, runs the four-stage pipeline on
// each, and flips the flag on success. Failures leave the flag
// set so the next tick retries (capped by an in-memory consecutive-
// bad counter that opens a circuit breaker after N failed ticks).
//
// Restart safety: partial DB writes from a crashed mid-pipeline
// run are tolerated. The resolver's catalog query picks up
// already-inserted entities on the next attempt and short-circuits;
// the edge upsert merges source_chunks. So a re-run converges
// rather than duplicates.
//
// Single-instance for now. Multi-instance deploys would need
// FOR UPDATE SKIP LOCKED on FetchUnextracted; not worth the
// complexity until backlog metrics justify horizontal scaling.
type Worker struct {
	source   persistence.ChunkGraphExtractionRepository
	pipeline *Pipeline
	logger   zerolog.Logger
	cfg      WorkerConfig
	metrics  *Metrics
	// leaderGate, when non-nil, is consulted at the top of every
	// drain tick. Non-leaders skip the FetchUnextracted+pipeline
	// run so two daemons don't double-extract the same chunks
	// (FetchUnextracted has no FOR UPDATE SKIP LOCKED today —
	// see comment in repository.go).
	leaderGate LeaderGate

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped chan struct{}

	mu             sync.Mutex
	consecutiveBad int
	pausedUntil    time.Time
	lastTick       time.Time
	lastProcessed  int
	lastFailed     int
}

// LeaderGate is the narrow contract Worker uses to skip drain
// ticks on non-leader daemons. Same shape as the memory package's
// LeaderGate; redefined here so the graph subpackage doesn't pull
// the parent memory package into its imports.
type LeaderGate interface {
	IsLeader() bool
}

// SetLeaderGate wires the elected-leader gate. Called by the
// service container after the Worker is constructed. nil clears
// (single-process default).
func (w *Worker) SetLeaderGate(g LeaderGate) {
	if w == nil {
		return
	}
	w.leaderGate = g
}

// WorkerConfig tunes the drain loop. Zero values pick safe
// defaults derived from the LLD §10 cost model — small batches
// keep token-spend per tick predictable; long poll interval keeps
// the worker idle-cheap once the backlog drains.
type WorkerConfig struct {
	// PollInterval between drain ticks. Default 30s — the LLM
	// stages are the bottleneck (≥1s per chunk on Bedrock Haiku),
	// so a tighter interval gains nothing.
	PollInterval time.Duration

	// BatchSize caps how many chunks one tick processes. Default 10.
	// Larger batches risk context-cancellation mid-batch on shutdown
	// and lengthen the time-to-first-flag-cleared during testing.
	BatchSize int

	// MaxParallel — concurrent pipeline runs per tick. Default 1.
	// Bumping helps throughput when chunks are small + the LLM
	// gateway is fast; risk is hammering the LLM with bursts that
	// trigger 429s. Start serial; raise after watching latency.
	MaxParallel int

	// CircuitBreakerThreshold — pause after this many consecutive
	// bad ticks (any failure within a tick counts as bad). Default 5.
	CircuitBreakerThreshold int

	// CircuitBreakerPause — how long the breaker stays tripped.
	// Default 5 minutes; a tripped breaker means the LLM gateway
	// is in trouble and rapid retries make it worse.
	CircuitBreakerPause time.Duration

	// ChunkTimeout caps how long a single chunk's pipeline run may
	// take. Default 8 minutes — covers 4 LLM stages at ~84s each
	// with a 2× margin, while cutting off AWS SDK retry storms
	// (3 retries × 300s backoff can exceed 9 minutes with connection
	// errors). A deadline-exceeded error leaves needs_graph_extraction
	// set so the next tick retries.
	ChunkTimeout time.Duration

	// GaugeRefreshInterval is the cadence for refreshing the
	// catalog gauges (chunks_pending / chunks_done / entities /
	// edges / mentions / entities_by_type) into Prometheus.
	// Decoupled from the tick loop because a tick can take 5–10
	// minutes when the per-chunk pipeline runs serially against
	// gpt-oss-120b — operators want dashboards updating much
	// faster than that. Default 30s.
	GaugeRefreshInterval time.Duration
}

func (c *WorkerConfig) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 10
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = 1
	}
	if c.CircuitBreakerThreshold <= 0 {
		c.CircuitBreakerThreshold = 5
	}
	if c.CircuitBreakerPause <= 0 {
		c.CircuitBreakerPause = 5 * time.Minute
	}
	if c.ChunkTimeout <= 0 {
		c.ChunkTimeout = 8 * time.Minute
	}
	if c.GaugeRefreshInterval <= 0 {
		c.GaugeRefreshInterval = 30 * time.Second
	}
}

// NewWorker constructs a Worker. Caller invokes Start to begin
// draining; Stop to shut down cleanly.
func NewWorker(source persistence.ChunkGraphExtractionRepository, pipeline *Pipeline, logger zerolog.Logger, cfg WorkerConfig) *Worker {
	cfg.applyDefaults()
	return &Worker{
		source:   source,
		pipeline: pipeline,
		logger:   logger,
		cfg:      cfg,
		stopped:  make(chan struct{}),
	}
}

// SetMetrics wires the Prometheus sink. Optional — nil-safe.
// Call before Start. The same Metrics instance should be set on
// the Pipeline so worker-level + pipeline-level counters share
// labels.
func (w *Worker) SetMetrics(m *Metrics) {
	w.metrics = m
	if w.pipeline != nil {
		w.pipeline.Metrics = m
	}
}

// Start launches the drain loop. Returns immediately.
func (w *Worker) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.wg.Add(1)
	go w.run(ctx)
	// Gauges run on a separate, faster cadence: a single tick can
	// take many minutes (the relationship stage on gpt-oss-120b is
	// slow), so coupling gauge refresh to tick completion would
	// leave dashboards stale during long ticks. The gauge loop only
	// touches Prometheus metrics — never DB writes — so it's cheap
	// to fire frequently.
	if w.metrics != nil {
		w.wg.Add(1)
		go w.gaugeLoop(ctx)
	}
	w.logger.Info().
		Dur("poll_interval", w.cfg.PollInterval).
		Dur("gauge_refresh_interval", w.cfg.GaugeRefreshInterval).
		Int("batch_size", w.cfg.BatchSize).
		Int("max_parallel", w.cfg.MaxParallel).
		Msg("KG extraction worker started")
}

// gaugeLoop runs refreshGauges on a fixed cadence independent of
// the chunk-drain loop. Spawned only when metrics are wired.
func (w *Worker) gaugeLoop(ctx context.Context) {
	defer w.wg.Done()
	// Fire once immediately so dashboards have data within a few
	// seconds of daemon start, not after the first interval.
	w.refreshGauges(ctx)
	ticker := time.NewTicker(w.cfg.GaugeRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.refreshGauges(ctx)
		}
	}
}

// Stop signals the drain loop to exit and waits for it.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	close(w.stopped)
	w.logger.Info().Msg("KG extraction worker stopped")
}

// Stopped returns a channel closed when the worker has fully
// stopped. Tests use this to synchronise without polling.
func (w *Worker) Stopped() <-chan struct{} { return w.stopped }

// LastTickStats reports the most recent tick's outcome —
// (timestamp, processed, failed). Operator dashboards display
// this so an operator can tell at a glance whether the worker
// is making progress or stuck.
func (w *Worker) LastTickStats() (time.Time, int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastTick, w.lastProcessed, w.lastFailed
}

func (w *Worker) run(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	// Tick once immediately so a stale backlog at boot starts
	// draining without waiting a full PollInterval.
	if w.leaderGate == nil || w.leaderGate.IsLeader() {
		w.tick(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.leaderGate != nil && !w.leaderGate.IsLeader() {
				continue
			}
			w.tick(ctx)
		}
	}
}

// tick fetches one batch and runs the pipeline on each chunk.
// Tracks pass/fail counts for the circuit breaker. Catalog
// gauges (pending/done/entities/edges/mentions) are NOT touched
// here — the gaugeLoop goroutine refreshes them on its own
// cadence so dashboards stay fresh during long-running ticks.
func (w *Worker) tick(ctx context.Context) {
	if w.circuitOpen() {
		return
	}
	chunks, err := w.source.FetchUnextracted(ctx, w.cfg.BatchSize)
	if err != nil {
		w.logger.Warn().Err(err).Msg("KG worker: FetchUnextracted failed")
		w.recordTickWithFailures(1)
		return
	}
	if len(chunks) == 0 {
		w.recordTickClean(0, 0)
		return
	}

	processed, failed := 0, 0
	if w.cfg.MaxParallel <= 1 {
		for _, c := range chunks {
			if ctx.Err() != nil {
				break
			}
			if w.runOne(ctx, c) {
				processed++
			} else {
				failed++
			}
		}
	} else {
		processed, failed = w.runParallel(ctx, chunks)
	}

	if failed > 0 {
		w.recordTickWithFailuresStats(failed, processed)
		w.logger.Warn().
			Int("processed", processed).
			Int("failed", failed).
			Int("batch", len(chunks)).
			Msg("KG worker: tick finished with failures")
	} else {
		w.recordTickClean(processed, failed)
		if processed > 0 {
			w.logger.Info().
				Int("processed", processed).
				Int("batch", len(chunks)).
				Msg("KG worker: tick complete")
		}
	}
}

// runOne runs the pipeline on a single chunk and flips the flag
// on success. Returns true on success. Records duration +
// success/failure into Prometheus when w.metrics is wired.
func (w *Worker) runOne(ctx context.Context, c persistence.ChunkForExtraction) bool {
	chunkCtx, cancel := context.WithTimeout(ctx, w.cfg.ChunkTimeout)
	defer cancel()
	start := time.Now()
	m, err := w.pipeline.RunChunk(chunkCtx, ChunkInput{ID: c.ID, ProjectID: c.ProjectID, Content: c.Content})
	if w.metrics != nil {
		w.metrics.ExtractionDuration.Observe(time.Since(start).Seconds())
	}
	if err != nil {
		if w.metrics != nil {
			w.metrics.ChunksExtractedTotal.WithLabelValues("failed").Inc()
		}
		w.logger.Warn().Err(err).
			Str("chunk_id", c.ID).
			Str("project_id", c.ProjectID).
			Msg("KG worker: pipeline run failed; chunk stays flagged")
		return false
	}
	if mErr := w.source.MarkExtracted(ctx, c.ID); mErr != nil {
		if w.metrics != nil {
			w.metrics.ChunksExtractedTotal.WithLabelValues("failed").Inc()
		}
		w.logger.Warn().Err(mErr).
			Str("chunk_id", c.ID).
			Msg("KG worker: MarkExtracted failed; chunk stays flagged for retry")
		return false
	}
	if w.metrics != nil {
		w.metrics.ChunksExtractedTotal.WithLabelValues("success").Inc()
	}
	w.logger.Debug().
		Str("chunk_id", c.ID).
		Int("entities_created", m.EntitiesCreated).
		Int("entities_matched", m.EntitiesMatched).
		Int("entities_ambiguous", m.EntitiesAmbiguous).
		Int("edges_upserted", m.EdgesUpserted).
		Int("edges_dropped", m.EdgesDropped).
		Msg("KG worker: chunk extracted")
	return true
}

// runParallel runs MaxParallel pipeline calls concurrently using
// a small semaphore. Returns (processed, failed).
func (w *Worker) runParallel(ctx context.Context, chunks []persistence.ChunkForExtraction) (int, int) {
	sem := make(chan struct{}, w.cfg.MaxParallel)
	var (
		mu        sync.Mutex
		processed int
		failed    int
		wg        sync.WaitGroup
	)
	for _, c := range chunks {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(chunk persistence.ChunkForExtraction) {
			defer wg.Done()
			defer func() { <-sem }()
			ok := w.runOne(ctx, chunk)
			mu.Lock()
			if ok {
				processed++
			} else {
				failed++
			}
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return processed, failed
}

func (w *Worker) circuitOpen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pausedUntil.IsZero() {
		return false
	}
	if time.Now().Before(w.pausedUntil) {
		return true
	}
	w.pausedUntil = time.Time{}
	w.consecutiveBad = 0
	w.logger.Info().Msg("KG worker: circuit breaker reset, resuming")
	return false
}

func (w *Worker) recordTickClean(processed, failed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.consecutiveBad = 0
	w.lastTick = time.Now()
	w.lastProcessed = processed
	w.lastFailed = failed
}

func (w *Worker) recordTickWithFailures(failed int) {
	w.recordTickWithFailuresStats(failed, 0)
}

func (w *Worker) recordTickWithFailuresStats(failed, processed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.consecutiveBad++
	w.lastTick = time.Now()
	w.lastProcessed = processed
	w.lastFailed = failed
	if w.consecutiveBad >= w.cfg.CircuitBreakerThreshold {
		w.pausedUntil = time.Now().Add(w.cfg.CircuitBreakerPause)
		if w.metrics != nil {
			w.metrics.CircuitTrippedTotal.Inc()
		}
		w.logger.Error().
			Int("consecutive_bad", w.consecutiveBad).
			Int("last_failed_count", failed).
			Dur("pause_for", w.cfg.CircuitBreakerPause).
			Msg("KG worker: circuit breaker tripped — pausing drain loop")
	}
}

// refreshGauges polls the chunk-graph repo's Stats and pushes
// the snapshot into the gauge metrics. Called once per tick when
// metrics are wired so dashboards see fresh pending/done /
// entity / edge counts without waiting for chunk activity.
func (w *Worker) refreshGauges(ctx context.Context) {
	if w.metrics == nil {
		return
	}
	stats, err := w.source.Stats(ctx)
	if err != nil || stats == nil {
		return
	}
	w.metrics.ChunksPending.Set(float64(stats.ChunksPending))
	w.metrics.ChunksDone.Set(float64(stats.ChunksDone))
	w.metrics.EntitiesTotal.Set(float64(stats.Entities))
	w.metrics.EdgesTotal.Set(float64(stats.Edges))
	w.metrics.MentionsTotal.Set(float64(stats.Mentions))
	for t, n := range stats.EntitiesByType {
		w.metrics.EntitiesByType.WithLabelValues(t).Set(float64(n))
	}
}
