package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	workerPollInterval = 5 * time.Second
	workerBatchSize    = 50
)

// Worker drains the memory_embed_queue by fetching batches, embedding them,
// and persisting the resulting vectors back to the DB.
type Worker struct {
	cfg      Config
	repo     *Repository
	embedder *Embedder
	titler   *Titler
	logger   zerolog.Logger
	metrics  *Metrics

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorker creates a Worker but does not start it.
func NewWorker(cfg Config, repo *Repository, embedder *Embedder, logger zerolog.Logger) *Worker {
	return &Worker{
		cfg:      cfg,
		repo:     repo,
		embedder: embedder,
		logger:   logger,
	}
}

// setMetrics attaches a Metrics instance to the Worker.
func (w *Worker) setMetrics(m *Metrics) { w.metrics = m }

// SetTitler wires an optional Titler. When set, the worker generates
// a content_title for each chunk after its embedding is successfully
// stored. Nil-safe: a nil Titler is the legacy path (no title
// generation at ingest; the backfill CLI is the only writer).
func (w *Worker) SetTitler(t *Titler) { w.titler = t }

// Start launches worker goroutines. The number of goroutines is
// cfg.WorkerConcurrency (default 2). The workers stop when ctx is cancelled
// or Stop() is called.
func (w *Worker) Start(ctx context.Context) {
	concurrency := w.cfg.WorkerConcurrency
	if concurrency <= 0 {
		concurrency = 2
	}

	workerCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	for i := 0; i < concurrency; i++ {
		w.wg.Add(1)
		go w.run(workerCtx)
	}

	if w.metrics != nil {
		w.metrics.WorkerUp.Set(1)
	}
	w.logger.Info().Int("workers", concurrency).Msg("memory embed worker started")
}

// Stop signals all workers to stop and waits for them to finish.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	if w.metrics != nil {
		w.metrics.WorkerUp.Set(0)
	}
	w.logger.Info().Msg("memory embed worker stopped")
}

// run is the main loop for a single worker goroutine.
func (w *Worker) run(ctx context.Context) {
	defer w.wg.Done()

	ticker := time.NewTicker(workerPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

// processBatch dequeues one batch, embeds the texts, and stores the vectors.
//
// Failure handling is DLQ-first: any chunk we can't embed or store
// moves to memory_embed_dlq with a retry_after backoff (10min * 2^N,
// capped at 24h). This replaces the old "log and drop" behaviour —
// a 10-minute embed endpoint outage used to turn into a permanent
// RAG index gap because the worker silently skipped failures.
func (w *Worker) processBatch(ctx context.Context) {
	// Step 0: replay any DLQ rows whose retry_after has lapsed. This
	// is how the worker auto-recovers — an outage that ended 20
	// minutes ago gets its chunks back in the queue on the next tick.
	if replayed, err := w.replayDueDLQ(ctx); err != nil {
		w.logger.Warn().Err(err).Msg("memory worker: DLQ replay failed")
	} else if replayed > 0 {
		w.logger.Info().Int("replayed", replayed).Msg("memory worker: DLQ auto-replay")
	}

	chunks, err := w.repo.DequeueEmbedBatch(ctx, workerBatchSize)
	if err != nil {
		w.logger.Warn().Err(err).Msg("memory worker: dequeue failed")
		return
	}
	if len(chunks) == 0 {
		return
	}

	// Contextualise each chunk before embedding so two chunks that
	// share vocabulary but belong to different sources/sections don't
	// collide in vector space. See embed_context.go for the rationale.
	// Stored content stays raw — only the embed input is prefixed, so
	// dedup hashes / search results / display are unaffected.
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = applyEmbedContext(c.SourceName, c.Content)
	}

	embedStart := time.Now()
	vecs, err := w.embedder.Embed(ctx, texts)
	if err != nil || vecs == nil {
		if w.metrics != nil {
			w.metrics.EmbedBatchesTotal.WithLabelValues("error").Inc()
		}
		// Embedding call itself failed — move the whole batch to the
		// DLQ so we keep track of them and auto-retry after the
		// endpoint recovers. Don't re-enqueue in-place: the worker
		// would hammer a dead endpoint until someone noticed.
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		w.logger.Warn().
			Err(err).
			Int("batch_size", len(chunks)).
			Msg("memory worker: embedding failed — moving batch to DLQ")
		for _, c := range chunks {
			retryAfter := time.Now().Add(w.dlqBackoff(0))
			if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "embedding_failed", errMsg, retryAfter); derr != nil {
				w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move failed")
			}
		}
		return
	}
	if w.metrics != nil {
		w.metrics.EmbedBatchesTotal.WithLabelValues("success").Inc()
		w.metrics.EmbedDuration.Observe(time.Since(embedStart).Seconds())
	}

	stored := 0
	dim := w.cfg.EmbeddingDimension
	if dim <= 0 {
		dim = 1536
	}

	for i, c := range chunks {
		if i >= len(vecs) || len(vecs[i]) == 0 {
			// Model skipped this chunk (returned fewer vectors than
			// inputs). Park in DLQ with retry_count = -1 since the
			// empty response is usually a content-too-large signal
			// that won't resolve on retry.
			retryAfter := time.Now().Add(24 * time.Hour)
			if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "empty_embedding", "embedder returned no vector for this chunk", retryAfter); derr != nil {
				w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move (empty) failed")
			}
			_ = w.repo.DLQPark(ctx, c.ID)
			continue
		}
		if len(vecs[i]) != dim {
			// Dimension mismatch means the model is returning a
			// different-sized vector than we're configured for. That's
			// a config/model mismatch the operator must fix — park
			// rather than retry.
			w.logger.Warn().
				Int("got", len(vecs[i])).
				Int("expected", dim).
				Str("model", w.cfg.EmbeddingModel).
				Str("chunk_id", c.ID).
				Msg("memory worker: embedding dimension mismatch — parking in DLQ")
			retryAfter := time.Now().Add(24 * time.Hour)
			msg := fmt.Sprintf("embedder returned dim=%d, expected=%d (model=%s)",
				len(vecs[i]), dim, w.cfg.EmbeddingModel)
			if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "dimension_mismatch", msg, retryAfter); derr != nil {
				w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move (dim) failed")
			}
			_ = w.repo.DLQPark(ctx, c.ID)
			continue
		}
		if err := w.repo.UpdateEmbedding(ctx, c.ID, vecs[i]); err != nil {
			w.logger.Warn().
				Err(err).
				Str("chunk_id", c.ID).
				Msg("memory worker: failed to store embedding — moving to DLQ")
			retryAfter := time.Now().Add(w.dlqBackoff(0))
			if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "store_failed", err.Error(), retryAfter); derr != nil {
				w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move (store) failed")
			}
			continue
		}
		stored++

		// Generate the content_title display label. Display-only —
		// failures log + move on; the viz falls back to markdown
		// heading then source_name. Nil-safe when titler isn't
		// wired (e.g. ChatClient disabled).
		if w.titler != nil {
			title, err := w.titler.Title(ctx, c.Content, c.ProjectID, c.ID)
			if err != nil {
				w.logger.Debug().
					Err(err).
					Str("chunk_id", c.ID).
					Msg("memory worker: title generation failed — leaving NULL")
			} else if title != "" {
				if uerr := w.repo.UpdateContentTitle(ctx, c.ID, title); uerr != nil {
					w.logger.Warn().
						Err(uerr).
						Str("chunk_id", c.ID).
						Msg("memory worker: content_title persist failed")
				}
			}
		}
	}
	if w.metrics != nil && stored > 0 {
		// EmbeddingsStoredTotal is per-project; use the first chunk's project.
		projectID := chunks[0].ProjectID
		w.metrics.EmbeddingsStoredTotal.WithLabelValues(projectID).Add(float64(stored))
	}

	w.logger.Debug().
		Int("embedded", len(chunks)).
		Msg("memory worker: batch embedded")
}

// dlqBackoff returns the retry delay for a DLQ row with the given
// retry_count. Exponential 10 min * 2^n, capped at 24 h so a
// long-dead endpoint doesn't pile rows up at ridiculous retry_after
// timestamps.
func (w *Worker) dlqBackoff(retryCount int) time.Duration {
	if retryCount < 0 {
		return 24 * time.Hour
	}
	base := 10 * time.Minute
	d := base << retryCount // 10m, 20m, 40m, 80m…
	max := 24 * time.Hour
	if d <= 0 || d > max {
		return max
	}
	return d
}

// replayDueDLQ moves DLQ rows whose retry_after has elapsed back to
// the embed queue. Bounded batch so one tick doesn't hold locks too
// long. Returns the number of rows replayed.
func (w *Worker) replayDueDLQ(ctx context.Context) (int, error) {
	rows, err := w.repo.DLQReadyForRetry(ctx, workerBatchSize)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ChunkID
	}
	return w.repo.DLQReplay(ctx, ids)
}
