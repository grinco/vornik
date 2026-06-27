package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
)

// TitleBackfiller drives the one-shot LLM title backfill. It reads
// chunks with NULL content_title, asks the Titler to label each, and
// persists the result. Serial by design (matches the operator's
// "don't hammer the gateway" preference) — a future flag could add
// concurrency, but the current rate-of-ingest is the natural cap on
// how often this needs to run.
//
// Run() drives the same BackfillBatch loop on a periodic ticker so
// chunks that the inline titler missed (LLM timeout, empty response)
// don't stay NULL forever. Without this loop a transient titler
// failure becomes a permanent display regression in the vector-cloud
// UI; the auto-loop closes that gap.
type TitleBackfiller struct {
	Repo    *Repository
	Titler  *Titler
	Logger  zerolog.Logger
	Metrics *Metrics
	// LeaderGate, when non-nil, is consulted at the top of
	// every tick. Non-leaders skip — two daemons running this
	// loop concurrently would race on the same NULL-titled
	// chunks and pay duplicate LLM cost.
	LeaderGate LeaderGate
}

// BackfillResult summarises one BackfillBatch call. Remaining is a
// snapshot taken after the batch completes — callers loop until
// Remaining == 0 (or until they hit their own --max cap).
type BackfillResult struct {
	Processed int      `json:"processed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"` // empty content / titler returned ""
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"` // first few, capped
}

// CountRemaining returns how many chunks still need a title. Cheap —
// used by --dry-run to estimate the cost before kicking off the run.
func (b *TitleBackfiller) CountRemaining(ctx context.Context) (int, error) {
	if b == nil || b.Repo == nil {
		return 0, fmt.Errorf("TitleBackfiller.CountRemaining: not configured")
	}
	return b.Repo.CountChunksMissingTitle(ctx)
}

// Run drives a periodic auto-backfill loop. It blocks until ctx is
// cancelled. Each tick refreshes the remaining-chunks gauge, then
// calls BackfillBatch if there's anything to process. Failure paths
// are logged at Warn (the chunk's title isn't load-bearing — display
// falls back) but never abort the loop. Designed for long-running
// daemon use; the bound on per-tick spend comes from batchSize.
//
// interval <= 0 or batchSize <= 0 cause Run to return immediately
// without ticking; callers use that to disable the loop via config.
func (b *TitleBackfiller) Run(ctx context.Context, interval time.Duration, batchSize int) {
	if b == nil || b.Repo == nil || b.Titler == nil {
		return
	}
	if interval <= 0 || batchSize <= 0 {
		b.Logger.Debug().Dur("interval", interval).Int("batch_size", batchSize).
			Msg("title backfill auto-loop disabled by config")
		return
	}
	b.Logger.Info().
		Dur("interval", interval).
		Int("batch_size", batchSize).
		Msg("title backfill auto-loop started")
	defer b.Logger.Info().Msg("title backfill auto-loop stopped")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Fire immediately so a daemon restart picks up any NULLs left
	// from the previous incarnation without waiting a full interval.
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

// runOnce performs a single backfill cycle: refresh the remaining-
// chunks gauge, then call BackfillBatch if there's work. Split out
// from Run so the immediate-fire-at-start path doesn't duplicate the
// ticker logic.
func (b *TitleBackfiller) runOnce(ctx context.Context, batchSize int) {
	remaining, err := b.CountRemaining(ctx)
	if err != nil {
		if b.Metrics != nil {
			b.Metrics.TitleBackfillTicksTotal.WithLabelValues("errored").Inc()
		}
		b.Logger.Warn().Err(err).Msg("title backfill auto-loop: count remaining failed")
		return
	}
	if b.Metrics != nil {
		b.Metrics.TitleBackfillRemainingChunks.Set(float64(remaining))
	}
	if remaining == 0 {
		if b.Metrics != nil {
			b.Metrics.TitleBackfillTicksTotal.WithLabelValues("idle").Inc()
		}
		return
	}
	result, err := b.BackfillBatch(ctx, batchSize)
	if err != nil {
		if b.Metrics != nil {
			b.Metrics.TitleBackfillTicksTotal.WithLabelValues("errored").Inc()
		}
		b.Logger.Warn().Err(err).Int("remaining_before", remaining).
			Msg("title backfill auto-loop: batch failed")
		return
	}
	if b.Metrics != nil {
		b.Metrics.TitleBackfillTicksTotal.WithLabelValues("progressed").Inc()
		b.Metrics.TitleBackfillChunksTotal.WithLabelValues("succeeded").Add(float64(result.Succeeded))
		b.Metrics.TitleBackfillChunksTotal.WithLabelValues("failed").Add(float64(result.Failed))
		b.Metrics.TitleBackfillChunksTotal.WithLabelValues("skipped").Add(float64(result.Skipped))
		b.Metrics.TitleBackfillRemainingChunks.Set(float64(result.Remaining))
	}
	b.Logger.Info().
		Int("processed", result.Processed).
		Int("succeeded", result.Succeeded).
		Int("failed", result.Failed).
		Int("skipped", result.Skipped).
		Int("remaining", result.Remaining).
		Msg("title backfill auto-loop: tick complete")
}

// BackfillBatch processes up to batchSize chunks serially and returns
// the result + a refreshed Remaining count. Each chunk's title call
// gets its own LLM round-trip; failures are recorded but do not
// abort the batch — display titles are not load-bearing.
//
// batchSize <= 0 → 10. The cap inside ListChunksMissingTitle (1000)
// applies on top.
func (b *TitleBackfiller) BackfillBatch(ctx context.Context, batchSize int) (*BackfillResult, error) {
	if b == nil || b.Repo == nil || b.Titler == nil {
		return nil, fmt.Errorf("TitleBackfiller.BackfillBatch: repo/titler not configured")
	}
	if batchSize <= 0 {
		batchSize = 10
	}
	rows, err := b.Repo.ListChunksMissingTitle(ctx, batchSize)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	out := &BackfillResult{Processed: len(rows)}
	for _, row := range rows {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		title, terr := b.Titler.Title(ctx, row.Content, row.ProjectID, row.ID)
		if terr != nil {
			out.Failed++
			if len(out.Errors) < 5 {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", row.ID, terr))
			}
			// Warn (not Debug) because the backfill is operator-
			// initiated — they ran it explicitly, so a failure is
			// load-bearing context they want to see in the daemon
			// logs without raising the global log level.
			b.Logger.Warn().Err(terr).Str("chunk_id", row.ID).Msg("backfill: title failed")
			continue
		}
		if title == "" {
			out.Skipped++
			continue
		}
		if uerr := b.Repo.UpdateContentTitle(ctx, row.ID, title); uerr != nil {
			out.Failed++
			if len(out.Errors) < 5 {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: persist: %v", row.ID, uerr))
			}
			b.Logger.Warn().Err(uerr).Str("chunk_id", row.ID).Msg("backfill: persist failed")
			continue
		}
		out.Succeeded++
	}
	remaining, rerr := b.Repo.CountChunksMissingTitle(ctx)
	if rerr != nil {
		// Non-fatal — the loop will recheck next batch. Just leave
		// remaining at 0 to avoid a misleading negative number.
		b.Logger.Debug().Err(rerr).Msg("backfill: count remaining failed")
	} else {
		out.Remaining = remaining
	}
	return out, nil
}
