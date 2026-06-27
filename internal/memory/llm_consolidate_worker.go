package memory

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/leaderelection"
)

// LLMConsolidateWorker drives the periodic LLM-tier narrative
// pass. Sits on top of the LLM-free Consolidator (which writes
// terms_json + chunks_scanned to project_gists) and layers a
// natural-language summary into the same row's narrative
// columns.
//
// Cadence and behaviour:
//   - One-goroutine timer loop, immediate first tick on start.
//   - Default cadence 1 hour — slower than the LLM-free tier
//     (10 min) because the LLM cost-per-tick dominates;
//     narratives shift much more slowly than the term cloud.
//   - Skips projects with no existing gist (the LLM-free worker
//     hasn't run yet) — the LLM tier always layers on top.
//   - Per-tick / per-project outcome labels mirror Consolidate's
//     so dashboards extend with a metric-name swap.
//
// Disabled when Writer or Interval are nil/<=0 — operators opt
// in via config.Memory.LLMConsolidateEnabled.
type LLMConsolidateWorker struct {
	Writer   *NarrativeWriter
	Repo     *Repository
	Projects ProjectLister
	Interval time.Duration

	// SampleSize is how many chunk excerpts to feed the LLM per
	// project. 0 → 8. Bigger drives token cost up linearly; the
	// summary doesn't materially improve beyond ~10 chunks.
	SampleSize int
	// SampleBytesPerChunk caps each excerpt before joining. 0 →
	// 1024. Bounds prompt size when chunks are long markdown
	// documents.
	SampleBytesPerChunk int

	Logger  zerolog.Logger
	Metrics *Metrics
	// LeaderGate gates the tick on the elected leader. Two
	// daemons running this loop would duplicate LLM cost on
	// every project. Nil-safe.
	LeaderGate LeaderGate

	stopped chan struct{}
}

// Run drives the periodic loop. Blocks until ctx is cancelled or
// the worker is structurally disabled (zero Interval, nil Writer,
// nil Repo, nil Projects).
func (w *LLMConsolidateWorker) Run(ctx context.Context) {
	if w == nil || w.Writer == nil || w.Repo == nil || w.Projects == nil {
		return
	}
	if w.Interval <= 0 {
		w.Logger.Debug().Dur("interval", w.Interval).
			Msg("memory llm-consolidate worker disabled by config")
		return
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)

	w.Logger.Info().Dur("interval", w.Interval).Int("sample_size", w.effectiveSampleSize()).
		Msg("memory llm-consolidate worker started")
	defer w.Logger.Info().Msg("memory llm-consolidate worker stopped")

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	if w.LeaderGate == nil || w.LeaderGate.IsLeader() {
		w.tick(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
				continue
			}
			w.tick(ctx)
		}
	}
}

// Stopped returns a channel closed when Run exits. Tests use it
// to synchronise without polling.
func (w *LLMConsolidateWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

// tick runs one pass over every project. Per-project failure is
// logged + counted but never halts the tick.
func (w *LLMConsolidateWorker) tick(ctx context.Context) {
	projects := w.Projects.ListProjectIDs()
	if len(projects) == 0 {
		w.recordTickOutcome("idle")
		return
	}
	progressed, failed := 0, 0
	for _, pid := range projects {
		if ctx.Err() != nil {
			return
		}
		if err := w.processOne(ctx, pid); err != nil {
			failed++
			w.recordProjectOutcome(pid, "errored")
			w.Logger.Warn().Err(err).Str("project", pid).
				Msg("llm-consolidate worker: project failed")
			continue
		}
		progressed++
		w.recordProjectOutcome(pid, "ok")
	}
	switch {
	case progressed > 0:
		w.recordTickOutcome("progressed")
	case failed > 0:
		w.recordTickOutcome("errored")
	default:
		w.recordTickOutcome("idle")
	}
}

// processOne pulls the existing gist + a chunk sample, calls the
// NarrativeWriter, writes the narrative back. Returns
// ErrGistNotFound translated to nil (the LLM-free worker hasn't
// caught up; skipping is the right behaviour, not an error).
func (w *LLMConsolidateWorker) processOne(ctx context.Context, projectID string) error {
	// Leader-epoch fence (review B1): guard the LLM call + narrative write
	// against a stale (TTL-expired-then-resumed) leader that the IsLeader
	// tick gate would still wave through. VerifyEpoch fails closed on a
	// superseded epoch / unreadable lock row, so we never spend the LLM
	// call or UpsertNarrative behind a newer leader. Plain IsLeader-only or
	// nil gate proceeds (pre-fence behaviour).
	if proceed, reason := leaderelection.DangerousWriteAllowed(ctx, w.LeaderGate); !proceed {
		w.Logger.Warn().Str("reason", reason).Str("project", projectID).
			Msg("memory_llm_consolidate: leader epoch fence — skipping narrative pass")
		leaderelection.LeaderFenceRejected("memory_llm_consolidate")
		return nil
	}
	start := time.Now()
	gist, err := w.Repo.GetGist(ctx, projectID)
	if errors.Is(err, ErrGistNotFound) {
		// LLM-free worker hasn't filled the row yet for this
		// project; skip silently — next tick will retry.
		return nil
	}
	if err != nil {
		return err
	}
	sample, err := w.buildSample(ctx, projectID)
	if err != nil {
		return err
	}
	narrative, err := w.Writer.Write(ctx, gist.Terms, sample, projectID)
	if err != nil {
		return err
	}
	if narrative == "" {
		// Writer returned empty without error — no usable summary.
		// Don't overwrite an existing narrative with empty bytes;
		// the prior tick's text remains valid until we have a
		// better one.
		return nil
	}
	if err := w.Repo.UpsertNarrative(ctx, projectID, narrative, w.Writer.Model, time.Now().UTC()); err != nil {
		return err
	}
	if w.Metrics != nil {
		w.Metrics.NarrativeDurationSeconds.WithLabelValues(projectID).Observe(time.Since(start).Seconds())
	}
	return nil
}

// buildSample fetches up to SampleSize chunks for the project,
// truncates each to SampleBytesPerChunk, and joins them with a
// "---" separator. Empty sample returns "" so the writer can
// detect "too thin to summarise" and bail.
func (w *LLMConsolidateWorker) buildSample(ctx context.Context, projectID string) (string, error) {
	limit := w.effectiveSampleSize()
	contents, err := w.Repo.ListChunkContents(ctx, projectID, limit)
	if err != nil {
		return "", err
	}
	cap := w.SampleBytesPerChunk
	if cap <= 0 {
		cap = 1024
	}
	var b strings.Builder
	for i, c := range contents {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if len(c) > cap {
			c = c[:cap]
		}
		if i > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString(c)
	}
	return b.String(), nil
}

func (w *LLMConsolidateWorker) effectiveSampleSize() int {
	if w.SampleSize > 0 {
		return w.SampleSize
	}
	return 8
}

func (w *LLMConsolidateWorker) recordTickOutcome(outcome string) {
	if w == nil || w.Metrics == nil || w.Metrics.NarrativeTicksTotal == nil {
		return
	}
	w.Metrics.NarrativeTicksTotal.WithLabelValues(outcome).Inc()
}

func (w *LLMConsolidateWorker) recordProjectOutcome(projectID, outcome string) {
	if w == nil || w.Metrics == nil || w.Metrics.NarrativeProjectsTotal == nil {
		return
	}
	w.Metrics.NarrativeProjectsTotal.WithLabelValues(projectID, outcome).Inc()
}
