package memory

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// failureBackoff adapts a background loop's cadence to whether its work is succeeding.
//
// INCIDENT 2026-07-30, customer deployment. The classify-backfill loop ran on a fixed
// 30-second ticker against a shared LLM gateway whose calls — under the daemon's own
// concurrency — took longer than 30 seconds. Every tick therefore launched five more calls
// while the previous five were still burning their timeouts: roughly 500 timeouts an hour,
// the unclassified backlog frozen at 6,684, and the title-backfill loop doing the same
// thing beside it.
//
// The model was not slow. Probed sequentially against the same gateway it answered in
// 0.6 seconds. The load was self-inflicted, and a fixed interval cannot notice that.
//
// So: a loop whose work is failing slows down. Doubling per fully-failed tick, capped, and
// snapping straight back to the configured cadence the moment a tick succeeds.
//
// DELIBERATELY NOT a circuit breaker. A breaker stops calling entirely, which for a
// backfill means the queue stops draining and nobody finds out until someone notices the
// backlog. Slowing down keeps the loop alive as its own health probe: it will pick the pace
// back up on its own when the endpoint recovers, with no operator action.
type failureBackoff struct {
	base       time.Duration
	multiplier int
}

const (
	// maxBackfillBackoff bounds the slowdown. An operator who fixes the endpoint should
	// not wait an hour for the loop to notice, so the worst case stays tolerable.
	maxBackfillBackoff = 30 * time.Minute
	// maxBackfillMultiplier keeps the doubling from overflowing on a long outage; the
	// interval is capped by maxBackfillBackoff long before this bites.
	maxBackfillMultiplier = 1 << 10
)

func newFailureBackoff(base time.Duration) *failureBackoff {
	return &failureBackoff{base: base, multiplier: 1}
}

// interval is the delay to use before the next tick.
func (f *failureBackoff) interval() time.Duration {
	if f == nil || f.base <= 0 {
		return 0
	}
	d := f.base * time.Duration(f.multiplier)
	if d > maxBackfillBackoff {
		return maxBackfillBackoff
	}
	return d
}

// observe records the outcome of one tick: how many units of work it attempted and how
// many failed.
//
// Only a TOTAL failure backs off. A partial failure is the normal, ambiguous case — some
// chunks legitimately fail to classify because the model abstains — and treating that as
// endpoint trouble would throttle a loop that is working correctly. An empty tick is
// neutral: a drained queue must not slow the loop that has to notice new work arriving.
//
// Recovery is immediate rather than a gradual ramp-down. A healthy deployment should not
// crawl through its backlog because of an outage that has already ended.
func (f *failureBackoff) observe(attempted, failed int) {
	if f == nil {
		return
	}
	switch {
	case attempted == 0:
		// Nothing to learn from.
	case failed == attempted:
		if f.multiplier < maxBackfillMultiplier {
			f.multiplier *= 2
		}
	case failed == 0:
		f.multiplier = 1
	default:
		// Partial failure: hold the current cadence. Neither evidence of congestion
		// nor evidence that it has cleared.
	}
}

// observeBackfillTick feeds one tick's outcome to a loop's backoff and logs a cadence
// change, so a slowdown is visible to an operator instead of the loop just going quiet.
//
// Shared by the classify and title backfill loops: both grew the same backoff concern at
// the same time, and a second copy would drift from the first — the same reasoning that
// keeps the report scrubber and the gate stack single-sourced.
//
// A nil result means "no evidence" (a DB counting error, or an idle tick) and leaves the
// cadence alone rather than blaming the endpoint.
func observeBackfillTick(
	logger zerolog.Logger,
	label string,
	backoff *failureBackoff,
	processed, failed int,
	haveResult bool,
) {
	if !haveResult {
		return
	}
	before := backoff.interval()
	backoff.observe(processed, failed)
	after := backoff.interval()
	if after == before {
		return
	}
	logger.Warn().
		Dur("was", before).
		Dur("now", after).
		Int("processed", processed).
		Int("failed", failed).
		Msg(label + " backfill: cadence changed — every unit of work in the last tick failed, " +
			"so the loop is backing off rather than piling more calls onto whatever is failing")
}

// backfillCounts is one tick's outcome, in the shape both backfill loops produce.
type backfillCounts struct {
	Processed, Succeeded, Failed, Skipped, Remaining int
}

// backfillTickHooks parameterises runBackfillTick with the parts that genuinely differ
// between the classify and title loops: which rows to count, which batch to run, and
// which metric family to record into.
type backfillTickHooks struct {
	label          string
	logger         zerolog.Logger
	countRemaining func(context.Context) (int, error)
	runBatch       func(context.Context, int) (backfillCounts, error)
	// observability hooks; each is nil-safe at the call site.
	tickOutcome  func(outcome string)
	chunkCounts  func(succeeded, failed, skipped int)
	setRemaining func(int)
}

// runBackfillTick performs one backfill cycle and reports what happened.
//
// EXTRACTED 2026-07-30. The two loops' tick bodies were already deliberately parallel
// ("Mirrors TitleBackfiller.runOnce"), and adding the same backoff concern to both pushed
// them into genuine duplication — `dupl` flagged it, correctly, and it was my change that
// caused it. Rather than suppress the warning, the shared shape now lives here: the next
// concern added to a backfill loop gets added once.
//
// The returned bool is "did this tick produce evidence about the endpoint". A DB counting
// error says nothing about the model and must not drive backoff; a fully-failed batch does.
func runBackfillTick(ctx context.Context, batchSize int, h backfillTickHooks) (backfillCounts, bool) {
	remaining, err := h.countRemaining(ctx)
	if err != nil {
		if h.tickOutcome != nil {
			h.tickOutcome("errored")
		}
		h.logger.Warn().Err(err).Msg(h.label + " backfill auto-loop: count remaining failed")
		// A counting failure is a DB problem, not evidence about the model.
		return backfillCounts{}, false
	}
	if h.setRemaining != nil {
		h.setRemaining(remaining)
	}
	if remaining == 0 {
		if h.tickOutcome != nil {
			h.tickOutcome("idle")
		}
		return backfillCounts{}, false
	}

	counts, err := h.runBatch(ctx, batchSize)
	if err != nil {
		if h.tickOutcome != nil {
			h.tickOutcome("errored")
		}
		h.logger.Warn().Err(err).Int("remaining_before", remaining).
			Msg(h.label + " backfill auto-loop: batch failed")
		// The whole batch could not run — evidence of a fully-failed tick, so the loop
		// backs off instead of retrying at full rate against whatever is broken.
		return backfillCounts{Processed: batchSize, Failed: batchSize}, true
	}

	if h.tickOutcome != nil {
		h.tickOutcome("progressed")
	}
	if h.chunkCounts != nil {
		h.chunkCounts(counts.Succeeded, counts.Failed, counts.Skipped)
	}
	if h.setRemaining != nil {
		h.setRemaining(counts.Remaining)
	}
	h.logger.Info().
		Int("processed", counts.Processed).
		Int("succeeded", counts.Succeeded).
		Int("failed", counts.Failed).
		Int("skipped", counts.Skipped).
		Int("remaining", counts.Remaining).
		Msg(h.label + " backfill auto-loop: tick complete")
	return counts, true
}

// backfillMetricSet is one backfill loop's metric family. The classify and title families
// have identical shapes, so passing them lets both loops share one hook constructor —
// without this, each loop needed its own metric closures and the tick bodies stayed
// duplicates (dupl flagged exactly that, 2026-07-30).
type backfillMetricSet struct {
	ticks     *prometheus.CounterVec
	chunks    *prometheus.CounterVec
	remaining prometheus.Gauge
}

// newBackfillHooks assembles the hooks for one loop. All metric hooks tolerate a nil set,
// which is the metrics-disabled case.
func newBackfillHooks(
	label string,
	logger zerolog.Logger,
	m *backfillMetricSet,
	countRemaining func(context.Context) (int, error),
	runBatch func(context.Context, int) (backfillCounts, error),
) backfillTickHooks {
	h := backfillTickHooks{
		label:          label,
		logger:         logger,
		countRemaining: countRemaining,
		runBatch:       runBatch,
	}
	if m == nil {
		return h
	}
	h.tickOutcome = func(outcome string) {
		if m.ticks != nil {
			m.ticks.WithLabelValues(outcome).Inc()
		}
	}
	h.chunkCounts = func(succeeded, failed, skipped int) {
		if m.chunks == nil {
			return
		}
		m.chunks.WithLabelValues("succeeded").Add(float64(succeeded))
		m.chunks.WithLabelValues("failed").Add(float64(failed))
		m.chunks.WithLabelValues("skipped").Add(float64(skipped))
	}
	h.setRemaining = func(n int) {
		if m.remaining != nil {
			m.remaining.Set(float64(n))
		}
	}
	return h
}
