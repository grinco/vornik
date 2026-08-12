package membench

import (
	"context"
	"fmt"
	"time"
)

// Settling: wait for an ingested corpus to finish indexing before scoring it.
//
// vornik embeds through an asynchronous queue; an external service whose retain is
// synchronous has nothing pending. The harness ingests an item and immediately
// recalls it, so only the vornik arm races. On 2026-08-12 that produced a
// fabricated head-to-head: vornik scored recall 0.917 against hindsight's 1.000 on
// six LongMemEval items, and re-running the identical items against the same corpus
// once indexed scored 1.000. The per-item retrieved-chunk counts were 8, 1, 7, 3,
// 8, 8 — the shape of a race, not of a quality difference.
//
// WHY THE QUEUE AND NOT A READINESS THRESHOLD. Waiting for readiness to reach 1.0
// is wrong: a deployment with NO embedder configured sits at 0.0 for ever and is a
// legitimate keyword-only configuration, so a threshold wait would hang and then
// fail a valid baseline. Pending ingest work distinguishes the two cases the way the
// mechanism itself does — a draining queue has depth, a steady state has none.

// defaultSettleTimeout bounds the wait. Generous, because the real corpus takes
// 25-50 minutes to embed at the measured 8-17 chunks/sec on CPU, and a timeout that
// fires early converts a slow queue into a failed run.
const defaultSettleTimeout = 45 * time.Minute

// defaultSettlePollInterval is how often the queue is re-read while waiting.
const defaultSettlePollInterval = 2 * time.Second

// IngestQueueReporter is an optional MemorySystem capability: how much ingested
// content is still waiting to become searchable.
//
// Separate from EmbeddingReadinessReporter because they answer different questions.
// Readiness is a RATIO describing the corpus as it stands; this is WORK OUTSTANDING.
// A corpus can be 4% embedded and stable (no embedder wired) or 4% embedded and
// climbing (a queue mid-drain), and only the second is worth waiting for. The
// fabricated 2026-08-12 comparison came from treating those as the same state.
type IngestQueueReporter interface {
	PendingIngest(ctx context.Context) (int64, error)
}

// settle blocks until the system reports no ingest work outstanding, and returns
// the embedding readiness observed at the moment scoring may begin.
//
// That timing is the point. Readiness used to be sampled at result assembly, after
// every queue had drained, so it could only report the optimistic end state and
// never what any individual item was actually scored against.
//
// Systems that cannot report a queue are not waited on at all: an external service
// with a synchronous retain has nothing pending, and a deployment with no embedder
// never will. Refusing there would block valid baselines over a property those
// systems do not have.
func (r *Runner) settle(ctx context.Context) (*float64, error) {
	if r.settleDisabled {
		return nil, nil
	}
	q, ok := r.System.(IngestQueueReporter)
	if !ok {
		return nil, nil
	}
	timeout := r.SettleTimeout
	if timeout <= 0 {
		timeout = defaultSettleTimeout
	}
	interval := r.SettlePollInterval
	if interval <= 0 {
		interval = defaultSettlePollInterval
	}

	deadline := time.Now().Add(timeout)
	for {
		pending, err := q.PendingIngest(ctx)
		if err != nil {
			// Refuse rather than degrade. An unreadable queue is how a corpus
			// mid-drain gets measured as though it were settled — and the signal
			// meant to catch that spent weeks returning 403 while the run stayed
			// stamped trustworthy.
			return nil, fmt.Errorf("membench: cannot read the ingest queue, so this run "+
				"cannot show it measured a settled corpus: %w", err)
		}
		if pending <= 0 {
			// Settled. Readiness may still be below 1.0 — a deployment with no
			// embedder, or chunks parked in the DLQ — which is a different
			// measurement, not a broken run. Record it and let trust logic speak.
			return r.readiness(ctx), nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("membench: ingest never finished: %d item(s) still queued "+
				"after %s. Scoring now would report a retrieval number for an indexing "+
				"backlog — check the embed queue and DLQ", pending, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// SettleDisabled skips the wait for ingest to finish.
//
// A method rather than a bare field so the choice is explicit at the call site:
// disabling this reintroduces the exact race that produced a fabricated
// head-to-head result, and it should be visible in review.
func (r *Runner) SettleDisabled() { r.settleDisabled = true }
