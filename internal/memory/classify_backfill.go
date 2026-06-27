package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
)

// ClassifyBackfiller drives the LLM-driven class backfill: walks
// project_memory_chunks rows where content_class is unclassified (or
// empty), asks the Classifier to pick a class per row, and persists
// the verdict via Repository.UpdateChunkClass. Operator-initiated:
// the daemon does not run this automatically. Mirrors
// TitleBackfiller in shape so triage feels consistent between the
// two backfill surfaces.
//
// One chunk = one LLM round-trip. Cost scales with the size of the
// unclassified backlog; in practice operators run this once after a
// schema/role-map change rather than on a tick.
type ClassifyBackfiller struct {
	Repo       *Repository
	Classifier *Classifier
	Logger     zerolog.Logger
	Metrics    *Metrics
	// LeaderGate gates the tick loop on the elected leader.
	// Same shape + nil-safe contract as TitleBackfiller.
	LeaderGate LeaderGate
}

// ClassifyBackfillResult summarises one BackfillBatch call. Same
// shape as title-backfill's BackfillResult so the operator UI can
// reuse the rendering.
type ClassifyBackfillResult struct {
	Processed int      `json:"processed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"` // classifier returned unclassified
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"`
}

// CountRemaining returns how many chunks still need classification.
// Cheap probe used by --dry-run paths to estimate cost before
// kicking off a real run.
func (b *ClassifyBackfiller) CountRemaining(ctx context.Context, projectID string) (int, error) {
	if b == nil || b.Repo == nil {
		return 0, fmt.Errorf("ClassifyBackfiller.CountRemaining: not configured")
	}
	counts, err := b.Repo.CountUnclassifiedByRole(ctx, projectID)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return total, nil
}

// CountRemainingAll returns the total unclassified count across all
// projects. Used by the auto-backfill loop's tick gauge.
func (b *ClassifyBackfiller) CountRemainingAll(ctx context.Context) (int, error) {
	if b == nil || b.Repo == nil {
		return 0, fmt.Errorf("ClassifyBackfiller.CountRemainingAll: not configured")
	}
	return b.Repo.CountUnclassifiedChunks(ctx)
}

// Run drives a periodic auto-backfill loop. Mirrors
// TitleBackfiller.Run: blocks until ctx is cancelled, fires once
// immediately so a daemon restart picks up any leftover work, then
// ticks every interval. interval <= 0 or batchSize <= 0 disable the
// loop (operator runs the backfill CLI on demand). Per-chunk
// failures are warn-logged but never abort the loop — classification
// is not load-bearing for any single chunk.
func (b *ClassifyBackfiller) Run(ctx context.Context, interval time.Duration, batchSize int) {
	if b == nil || b.Repo == nil || b.Classifier == nil {
		return
	}
	if interval <= 0 || batchSize <= 0 {
		b.Logger.Debug().Dur("interval", interval).Int("batch_size", batchSize).
			Msg("classify backfill auto-loop disabled by config")
		return
	}
	b.Logger.Info().
		Dur("interval", interval).
		Int("batch_size", batchSize).
		Msg("classify backfill auto-loop started")
	defer b.Logger.Info().Msg("classify backfill auto-loop stopped")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if b.LeaderGate == nil || b.LeaderGate.IsLeader() {
		b.runOnce(ctx, batchSize)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if b.LeaderGate != nil && !b.LeaderGate.IsLeader() {
				continue
			}
			b.runOnce(ctx, batchSize)
		}
	}
}

// runOnce performs a single backfill cycle: refresh the remaining
// gauge, then call BackfillBatchAcrossProjects when there's work.
// Split from Run so the immediate-fire-at-start path doesn't
// duplicate the ticker logic. Mirrors TitleBackfiller.runOnce.
func (b *ClassifyBackfiller) runOnce(ctx context.Context, batchSize int) {
	remaining, err := b.CountRemainingAll(ctx)
	if err != nil {
		if b.Metrics != nil {
			b.Metrics.ClassifyBackfillTicksTotal.WithLabelValues("errored").Inc()
		}
		b.Logger.Warn().Err(err).Msg("classify backfill auto-loop: count remaining failed")
		return
	}
	if b.Metrics != nil {
		b.Metrics.ClassifyBackfillRemainingChunks.Set(float64(remaining))
	}
	if remaining == 0 {
		if b.Metrics != nil {
			b.Metrics.ClassifyBackfillTicksTotal.WithLabelValues("idle").Inc()
		}
		return
	}
	result, err := b.BackfillBatchAcrossProjects(ctx, batchSize)
	if err != nil {
		if b.Metrics != nil {
			b.Metrics.ClassifyBackfillTicksTotal.WithLabelValues("errored").Inc()
		}
		b.Logger.Warn().Err(err).Int("remaining_before", remaining).
			Msg("classify backfill auto-loop: batch failed")
		return
	}
	if b.Metrics != nil {
		b.Metrics.ClassifyBackfillTicksTotal.WithLabelValues("progressed").Inc()
		b.Metrics.ClassifyBackfillChunksTotal.WithLabelValues("succeeded").Add(float64(result.Succeeded))
		b.Metrics.ClassifyBackfillChunksTotal.WithLabelValues("failed").Add(float64(result.Failed))
		b.Metrics.ClassifyBackfillChunksTotal.WithLabelValues("skipped").Add(float64(result.Skipped))
		b.Metrics.ClassifyBackfillRemainingChunks.Set(float64(result.Remaining))
	}
	b.Logger.Info().
		Int("processed", result.Processed).
		Int("succeeded", result.Succeeded).
		Int("failed", result.Failed).
		Int("skipped", result.Skipped).
		Int("remaining", result.Remaining).
		Msg("classify backfill auto-loop: tick complete")
}

// BackfillBatchAcrossProjects is BackfillBatch's cross-project
// sibling — used by the auto-loop where no single project drives the
// tick. Same per-chunk semantics; the cross-project list is sorted
// oldest-first so the oldest unclassified rows get cleared first.
func (b *ClassifyBackfiller) BackfillBatchAcrossProjects(ctx context.Context, batchSize int) (*ClassifyBackfillResult, error) {
	if b == nil || b.Repo == nil || b.Classifier == nil {
		return nil, fmt.Errorf("ClassifyBackfiller.BackfillBatchAcrossProjects: repo/classifier not configured")
	}
	if batchSize <= 0 {
		batchSize = 10
	}
	rows, err := b.Repo.ListUnclassifiedChunksAcrossProjects(ctx, batchSize)
	if err != nil {
		return nil, fmt.Errorf("list unclassified (all projects): %w", err)
	}
	out := &ClassifyBackfillResult{Processed: len(rows)}
	for _, row := range rows {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		class, cerr := b.Classifier.Classify(ctx, row.Content, row.SourceName, row.ProducerRole, row.ProjectID, row.ID)
		if cerr != nil {
			out.Failed++
			if len(out.Errors) < 5 {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", row.ID, cerr))
			}
			b.Logger.Warn().Err(cerr).Str("chunk_id", row.ID).Str("project_id", row.ProjectID).
				Msg("classify backfill: LLM failed")
			continue
		}
		if class == "" || class == ClassUnclassified {
			out.Skipped++
			continue
		}
		policy := DefaultClassPolicies[class]
		if uerr := b.Repo.UpdateChunkClass(ctx, row.ID, string(class), policy.TTL); uerr != nil {
			out.Failed++
			if len(out.Errors) < 5 {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: persist: %v", row.ID, uerr))
			}
			b.Logger.Warn().Err(uerr).Str("chunk_id", row.ID).Str("project_id", row.ProjectID).
				Msg("classify backfill: persist failed")
			continue
		}
		out.Succeeded++
	}
	remaining, rerr := b.CountRemainingAll(ctx)
	if rerr != nil {
		b.Logger.Debug().Err(rerr).Msg("classify backfill: count remaining (all) failed")
	} else {
		out.Remaining = remaining
	}
	return out, nil
}

// BackfillBatch processes up to batchSize unclassified chunks in
// projectID. Each chunk's LLM call is its own round-trip; failures
// are recorded but do NOT abort the batch — classification is not
// load-bearing for any single chunk and a transient model outage
// shouldn't kill the whole sweep. batchSize <= 0 → 10; the
// repository cap (1000) applies on top.
func (b *ClassifyBackfiller) BackfillBatch(ctx context.Context, projectID string, batchSize int) (*ClassifyBackfillResult, error) {
	if b == nil || b.Repo == nil || b.Classifier == nil {
		return nil, fmt.Errorf("ClassifyBackfiller.BackfillBatch: repo/classifier not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("ClassifyBackfiller.BackfillBatch: projectID required")
	}
	if batchSize <= 0 {
		batchSize = 10
	}
	rows, err := b.Repo.ListUnclassifiedChunks(ctx, projectID, batchSize)
	if err != nil {
		return nil, fmt.Errorf("list unclassified: %w", err)
	}
	out := &ClassifyBackfillResult{Processed: len(rows)}
	for _, row := range rows {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		class, cerr := b.Classifier.Classify(ctx, row.Content, row.SourceName, row.ProducerRole, row.ProjectID, row.ID)
		if cerr != nil {
			out.Failed++
			if len(out.Errors) < 5 {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", row.ID, cerr))
			}
			b.Logger.Warn().Err(cerr).Str("chunk_id", row.ID).Msg("classify backfill: LLM failed")
			continue
		}
		// LLM said unclassified → leave the chunk alone. Counted as
		// skipped so the operator sees how many genuinely ambiguous
		// chunks the model couldn't resolve.
		if class == "" || class == ClassUnclassified {
			out.Skipped++
			continue
		}
		policy := DefaultClassPolicies[class]
		if uerr := b.Repo.UpdateChunkClass(ctx, row.ID, string(class), policy.TTL); uerr != nil {
			out.Failed++
			if len(out.Errors) < 5 {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: persist: %v", row.ID, uerr))
			}
			b.Logger.Warn().Err(uerr).Str("chunk_id", row.ID).Msg("classify backfill: persist failed")
			continue
		}
		out.Succeeded++
	}
	// Refresh the remaining count for the operator's progress
	// display. Non-fatal: a transient DB hiccup just leaves the
	// field at zero on this batch.
	remaining, rerr := b.CountRemaining(ctx, projectID)
	if rerr != nil {
		b.Logger.Debug().Err(rerr).Msg("classify backfill: count remaining failed")
	} else {
		out.Remaining = remaining
	}
	return out, nil
}
