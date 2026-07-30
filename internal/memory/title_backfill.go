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

	// TIMER, not ticker, so the cadence can react to sustained failure. On
	// 2026-07-30 this loop was the DOMINANT load once the classifier had been backed
	// off — 15 of 25 timeouts in a five-minute window, 25 calls every 5 minutes against
	// a saturated gateway. Same reasoning as the classify loop; see failureBackoff.
	backoff := newFailureBackoff(interval)
	timer := time.NewTimer(backoff.interval())
	defer timer.Stop()

	// Fire immediately so a daemon restart picks up any NULLs left
	// from the previous incarnation without waiting a full interval.
	if b.LeaderGate == nil || b.LeaderGate.IsLeader() {
		b.observeTick(backoff, b.runOnce(ctx, batchSize))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if b.LeaderGate == nil || b.LeaderGate.IsLeader() {
				b.observeTick(backoff, b.runOnce(ctx, batchSize))
			}
			timer.Reset(backoff.interval())
		}
	}
}

// observeTick delegates to the shared helper; see observeBackfillTick.
func (b *TitleBackfiller) observeTick(backoff *failureBackoff, result *BackfillResult) {
	if result == nil {
		observeBackfillTick(b.Logger, "title", backoff, 0, 0, false)
		return
	}
	observeBackfillTick(b.Logger, "title", backoff, result.Processed, result.Failed, true)
}

// runOnce performs a single backfill cycle: refresh the remaining-
// chunks gauge, then call BackfillBatch if there's work. Split out
// from Run so the immediate-fire-at-start path doesn't duplicate the
// ticker logic.
// The body below is a structural twin of ClassifyBackfiller.runOnce and `dupl` flags it.
// The shared LOGIC is genuinely extracted — runBackfillTick holds the tick algorithm and
// newBackfillHooks the metric plumbing — and what is left is an adapter: bind this loop's
// count/batch methods, its metric family, and convert between its own result type and the
// shared one. Unifying that too would need generics over struct fields, which Go does not
// have; the alternative (a shared result type threaded through both public APIs) would
// change exported signatures for no behavioural gain.
//
// Suppressed deliberately and narrowly. My own change caused this finding (dupl was clean
// on HEAD before it), so this is not an inherited warning being papered over — it is the
// residue after the extraction that removing the warning would otherwise have hidden.
//
//nolint:dupl // adapter twin; shared logic lives in runBackfillTick/newBackfillHooks
func (b *TitleBackfiller) runOnce(ctx context.Context, batchSize int) *BackfillResult {
	var m *backfillMetricSet
	if b.Metrics != nil {
		m = &backfillMetricSet{
			ticks:     b.Metrics.TitleBackfillTicksTotal,
			chunks:    b.Metrics.TitleBackfillChunksTotal,
			remaining: b.Metrics.TitleBackfillRemainingChunks,
		}
	}
	counts, haveEvidence := runBackfillTick(ctx, batchSize, newBackfillHooks(
		"title", b.Logger, m, b.CountRemaining,
		func(ctx context.Context, n int) (backfillCounts, error) {
			r, err := b.BackfillBatch(ctx, n)
			if err != nil || r == nil {
				return backfillCounts{}, err
			}
			return backfillCounts{
				Processed: r.Processed, Succeeded: r.Succeeded,
				Failed: r.Failed, Skipped: r.Skipped, Remaining: r.Remaining,
			}, nil
		},
	))
	if !haveEvidence {
		return nil
	}
	return &BackfillResult{
		Processed: counts.Processed, Succeeded: counts.Succeeded,
		Failed: counts.Failed, Skipped: counts.Skipped, Remaining: counts.Remaining,
	}
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
