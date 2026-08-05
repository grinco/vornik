package memory

import (
	"context"
	"fmt"
	"sync"
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

	// offset walks the oldest-first queue past rows nothing can classify.
	//
	// LIVELOCK, observed in production 2026-07-30: the loop selected
	// `ORDER BY created_at ASC LIMIT 25`, the classifier declined all 25, and a
	// declined row is never written — so the same 25 came back every tick and
	// permanently blocked the 1,174 behind them. `remaining` sat at 1199 across
	// ticks with succeeded=0, forever.
	//
	// Advancing by the number skipped walks past them, so every classifiable row
	// is reached. It resets on an empty page so newly-ingested rows are picked up.
	// Not persisted: a restart re-walks from the oldest, which costs one wasted
	// page and is self-correcting. The complete fix is a durable per-row
	// "attempted" marker, which needs a migration and is noted in the backlog.
	offset int

	// projectOffsets is the equivalent cursor for operator-driven,
	// project-scoped batches.  The CLI sends one HTTP request per batch, so a
	// local variable cannot survive from one request to the next.  Guard the
	// same project's operation: concurrently advancing that cursor would
	// otherwise make a batch skip arbitrary rows. Different projects must not
	// block each other's database or LLM calls.
	projectMu      sync.Mutex
	projectLocks   map[string]*sync.Mutex
	projectOffsets map[string]int
}

// ClassifyBackfillResult summarises one BackfillBatch call. Same
// shape as title-backfill's BackfillResult so the operator UI can
// reuse the rendering.
type ClassifyBackfillResult struct {
	Processed int `json:"processed"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"` // classifier returned unclassified
	Remaining int `json:"remaining"`
	// Exhausted means this sweep reached the end of the current queue. Remaining
	// can still be non-zero: those rows were deliberately left unclassified
	// because the model abstained. It lets the CLI finish an exhaustive pass
	// without mistaking honest abstentions for a livelock.
	Exhausted bool     `json:"exhausted,omitempty"`
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

	// A TIMER rather than a ticker, so the delay can adapt to whether the work is
	// succeeding. INCIDENT 2026-07-30: a fixed 30s ticker against a gateway whose calls
	// took longer than 30s launched five more calls every tick while the previous five
	// were still burning their timeouts — ~500 timeouts an hour and a frozen backlog.
	// A loop whose work is failing has to slow down; see failureBackoff.
	backoff := newFailureBackoff(interval)
	timer := time.NewTimer(backoff.interval())
	defer timer.Stop()

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
func (b *ClassifyBackfiller) observeTick(backoff *failureBackoff, result *ClassifyBackfillResult) {
	if result == nil {
		observeBackfillTick(b.Logger, "classify", backoff, 0, 0, false)
		return
	}
	observeBackfillTick(b.Logger, "classify", backoff, result.Processed, result.Failed, true)
}

// runOnce performs a single backfill cycle: refresh the remaining
// gauge, then call BackfillBatchAcrossProjects when there's work.
// Split from Run so the immediate-fire-at-start path doesn't
// duplicate the ticker logic. Mirrors TitleBackfiller.runOnce.
// Structural twin of TitleBackfiller.runOnce — see the note there for why the residue
// after extracting runBackfillTick/newBackfillHooks cannot itself be deduplicated.
//
//nolint:dupl // adapter twin; shared logic lives in runBackfillTick/newBackfillHooks
func (b *ClassifyBackfiller) runOnce(ctx context.Context, batchSize int) *ClassifyBackfillResult {
	var m *backfillMetricSet
	if b.Metrics != nil {
		m = &backfillMetricSet{
			ticks:     b.Metrics.ClassifyBackfillTicksTotal,
			chunks:    b.Metrics.ClassifyBackfillChunksTotal,
			remaining: b.Metrics.ClassifyBackfillRemainingChunks,
		}
	}
	counts, haveEvidence := runBackfillTick(ctx, batchSize, newBackfillHooks(
		"classify", b.Logger, m, b.CountRemainingAll,
		func(ctx context.Context, n int) (backfillCounts, error) {
			r, err := b.BackfillBatchAcrossProjects(ctx, n)
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
	return &ClassifyBackfillResult{
		Processed: counts.Processed, Succeeded: counts.Succeeded,
		Failed: counts.Failed, Skipped: counts.Skipped, Remaining: counts.Remaining,
	}
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
	rows, err := b.Repo.ListUnclassifiedChunksAcrossProjectsFrom(ctx, batchSize, b.offset)
	if err != nil {
		return nil, fmt.Errorf("list unclassified (all projects): %w", err)
	}
	if len(rows) == 0 && b.offset > 0 {
		// Walked off the end. Start over so rows ingested since the last pass, and
		// any whose role mapping has since been added, get another look.
		b.offset = 0
		rows, err = b.Repo.ListUnclassifiedChunksAcrossProjectsFrom(ctx, batchSize, 0)
		if err != nil {
			return nil, fmt.Errorf("list unclassified (all projects, rewound): %w", err)
		}
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
			// The LLM abstained. Before giving up, try the deterministic role map —
			// it is what the ingest path itself uses, and for a chunk whose
			// producer_role is known it is a better answer than leaving the row
			// unclassified forever.
			if byRole, _ := ClassifyByRole(row.ProducerRole); byRole != ClassUnclassified {
				if uerr := b.Repo.UpdateChunkClass(ctx, row.ID, string(byRole),
					DefaultClassPolicies[byRole].TTL); uerr == nil {
					out.Succeeded++
					continue
				}
			}
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
	lock := b.lockForProject(projectID)
	lock.Lock()
	defer lock.Unlock()
	offset := b.projectOffsets[projectID]
	rows, err := b.Repo.ListUnclassifiedChunksFrom(ctx, projectID, batchSize, offset)
	if err != nil {
		return nil, fmt.Errorf("list unclassified: %w", err)
	}
	if len(rows) == 0 && offset > 0 {
		// The pass reached its end. Rewind for a later operator run (new
		// data or a changed model may make prior abstentions classifiable),
		// but tell this run not to cycle back to its first row.
		b.projectOffsets[projectID] = 0
		remaining, rerr := b.CountRemaining(ctx, projectID)
		if rerr != nil {
			b.Logger.Debug().Err(rerr).Msg("classify backfill: count remaining failed")
		}
		return &ClassifyBackfillResult{Remaining: remaining, Exhausted: true}, nil
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
			// The LLM abstained. Before giving up, try the deterministic role map —
			// it is what the ingest path itself uses, and for a chunk whose
			// producer_role is known it is a better answer than leaving the row
			// unclassified forever.
			if byRole, _ := ClassifyByRole(row.ProducerRole); byRole != ClassUnclassified {
				if uerr := b.Repo.UpdateChunkClass(ctx, row.ID, string(byRole),
					DefaultClassPolicies[byRole].TTL); uerr == nil {
					out.Succeeded++
					continue
				}
			}
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
	// Deletions from successful updates shift the SQL offset. Restarting after
	// any success is conservative but correct; advancing after a no-progress
	// batch is what reaches rows that all preceding abstentions would hide.
	if out.Succeeded > 0 {
		b.projectOffsets[projectID] = 0
	} else if out.Processed > 0 {
		b.projectOffsets[projectID] = offset + out.Processed
	}
	return out, nil
}

// lockForProject returns a stable lock for one project's sweep cursor. The
// brief map lock protects only lock/cursor creation; model calls remain
// concurrent across projects while sequential requests to one project retain
// the cursor's ordering guarantee.
func (b *ClassifyBackfiller) lockForProject(projectID string) *sync.Mutex {
	b.projectMu.Lock()
	defer b.projectMu.Unlock()
	if b.projectLocks == nil {
		b.projectLocks = make(map[string]*sync.Mutex)
	}
	if b.projectOffsets == nil {
		b.projectOffsets = make(map[string]int)
	}
	lock := b.projectLocks[projectID]
	if lock == nil {
		lock = &sync.Mutex{}
		b.projectLocks[projectID] = lock
	}
	return lock
}
