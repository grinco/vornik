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
	vecs, err := w.embedBatchByProject(ctx, chunks, texts)
	if err != nil || vecs == nil {
		if len(chunks) > 1 {
			w.logger.Warn().
				Err(err).
				Int("batch_size", len(chunks)).
				Msg("memory worker: batch embed failed — retrying chunks individually")
			w.processIndividually(ctx, chunks)
			return
		}
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
		// The DLQ owns these from here, so release the queue rows. Without this the
		// claim would age out and re-deliver chunks the DLQ is already retrying.
		w.ackEmbedQueue(ctx, chunks)
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
		if err := w.repo.UpdateEmbedding(ctx, c.ID, vecs[i], ContentHash(texts[i])); err != nil {
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
	// Acknowledge every chunk whose outcome is now durable. Deliberately AFTER the
	// per-chunk loop: each path above either stored an embedding or parked the chunk
	// in the DLQ, and both are terminal for the queue. A chunk that reached neither
	// (a store failure that also failed to DLQ) keeps its claim and is re-delivered
	// once the claim ages out — which is the whole point of the lease.
	w.ackEmbedQueue(ctx, chunks)

	if w.metrics != nil && stored > 0 {
		// EmbeddingsStoredTotal is per-project; use the first chunk's project.
		projectID := chunks[0].ProjectID
		w.metrics.EmbeddingsStoredTotal.WithLabelValues(projectID).Add(float64(stored))
	}

	w.logger.Debug().
		Int("embedded", len(chunks)).
		Msg("memory worker: batch embedded")
}

// ackEmbedQueue releases the lease on chunks whose outcome is durable.
//
// Failure to ack is logged, never fatal: the chunk is embedded, and the worst case
// is one redundant re-embed after the claim ages out. Failing the batch here would
// trade a harmless duplicate for a lost one.
func (w *Worker) ackEmbedQueue(ctx context.Context, chunks []MemoryChunk) {
	if len(chunks) == 0 {
		return
	}
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		ids = append(ids, c.ID)
	}
	if err := w.repo.AckEmbedQueue(ctx, ids); err != nil {
		w.logger.Warn().Err(err).Int("chunks", len(ids)).
			Msg("memory worker: failed to release embed-queue claims — chunks will be re-delivered after the claim ages out")
	}
}

// embedBatchByProject embeds a dequeued batch as one provider call PER PROJECT,
// scattering the vectors back into a slice positionally aligned with chunks.
//
// The grouping is a billing requirement, not a tidiness preference.
// DequeueEmbedBatch claims by enqueued_at with no project filter
// (repository.go:370), so one batch routinely mixes projects. A single Embed
// call for the whole batch would have to name one project in its usage row,
// which means billing one tenant for another's embeddings — the precise
// misattribution the spend-attribution design exists to prevent
// (2026-08-12-embed-spend-attribution-design.md §8.4).
//
// Cross-project fairness is unaffected: the dequeue is untouched, only the
// provider calls are split. A single-project batch still makes exactly one call,
// so the common case costs nothing.
//
// Returns (nil, err) if any group fails, preserving the caller's existing
// whole-batch failure handling (retry individually, then DLQ).
func (w *Worker) embedBatchByProject(ctx context.Context, chunks []MemoryChunk, texts []string) ([][]float32, error) {
	// Group indices by project, preserving first-seen order so the call
	// sequence is deterministic — a randomly ordered map walk would make the
	// provider-call order vary run to run and the tests flaky.
	order := make([]string, 0, 4)
	byProject := make(map[string][]int, 4)
	for i, c := range chunks {
		if _, seen := byProject[c.ProjectID]; !seen {
			order = append(order, c.ProjectID)
		}
		byProject[c.ProjectID] = append(byProject[c.ProjectID], i)
	}

	out := make([][]float32, len(chunks))
	for _, projectID := range order {
		idxs := byProject[projectID]
		group := make([]string, len(idxs))
		for j, idx := range idxs {
			group[j] = texts[idx]
		}
		vecs, err := w.embedder.Embed(ctx,
			EmbedScope{ProjectID: projectID, CallSite: EmbedCallSiteIngest}, group)
		if err != nil {
			return nil, err
		}
		if vecs == nil {
			// The degrade signal. Fail the whole batch rather than storing a
			// partial result: the caller's retry-individually path re-tries every
			// chunk, and half-written vectors would leave the rest silently
			// unindexed.
			return nil, nil
		}
		for j, idx := range idxs {
			if j < len(vecs) {
				out[idx] = vecs[j]
			}
		}
	}
	return out, nil
}

func (w *Worker) processIndividually(ctx context.Context, chunks []MemoryChunk) {
	for _, c := range chunks {
		text := applyEmbedContext(c.SourceName, c.Content)
		vecs, err := w.embedder.Embed(ctx,
			EmbedScope{ProjectID: c.ProjectID, CallSite: EmbedCallSiteIngest},
			[]string{text})
		if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
			if w.metrics != nil {
				w.metrics.EmbedBatchesTotal.WithLabelValues("error").Inc()
			}
			errMsg := "embedder returned no vectors"
			if err != nil {
				errMsg = err.Error()
			}
			retryAfter := time.Now().Add(w.dlqBackoff(0))
			if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "embedding_failed", errMsg, retryAfter); derr != nil {
				w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move failed")
			}
			continue
		}
		w.persistEmbeddedChunk(ctx, c, vecs[0], ContentHash(text))
	}
	// Every chunk here reached a durable outcome — stored, or parked in the DLQ
	// which owns retry — so release the leases. This path is reached only after a
	// batch embed failed, which is exactly when a restart is most likely, so
	// forgetting it would leave the orphaning hazard alive on the retry path.
	w.ackEmbedQueue(ctx, chunks)
}

// embedInputHash is the embedding_cache key the vector was stored under — the hash of
// the exact string handed to the embedder, passed down rather than recomputed so the
// column cannot drift from what was cached (migration 150).
func (w *Worker) persistEmbeddedChunk(ctx context.Context, c MemoryChunk, vec []float32, embedInputHash string) {
	dim := w.cfg.EmbeddingDimension
	if dim <= 0 {
		dim = 1536
	}
	if len(vec) != dim {
		w.logger.Warn().
			Int("got", len(vec)).
			Int("expected", dim).
			Str("model", w.cfg.EmbeddingModel).
			Str("chunk_id", c.ID).
			Msg("memory worker: embedding dimension mismatch — parking in DLQ")
		retryAfter := time.Now().Add(24 * time.Hour)
		msg := fmt.Sprintf("embedder returned dim=%d, expected=%d (model=%s)",
			len(vec), dim, w.cfg.EmbeddingModel)
		if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "dimension_mismatch", msg, retryAfter); derr != nil {
			w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move (dim) failed")
		}
		_ = w.repo.DLQPark(ctx, c.ID)
		return
	}
	if err := w.repo.UpdateEmbedding(ctx, c.ID, vec, embedInputHash); err != nil {
		w.logger.Warn().
			Err(err).
			Str("chunk_id", c.ID).
			Msg("memory worker: failed to store embedding — moving to DLQ")
		retryAfter := time.Now().Add(w.dlqBackoff(0))
		if derr := w.repo.DLQMove(ctx, c.ID, c.ProjectID, "store_failed", err.Error(), retryAfter); derr != nil {
			w.logger.Warn().Err(derr).Str("chunk_id", c.ID).Msg("memory worker: DLQ move (store) failed")
		}
		return
	}
	if w.metrics != nil {
		w.metrics.EmbeddingsStoredTotal.WithLabelValues(c.ProjectID).Inc()
	}
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
