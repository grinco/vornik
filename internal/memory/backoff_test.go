package memory

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// INCIDENT 2026-07-30, customer deployment. The classify-backfill loop ran on a fixed
// 30-second ticker against a shared LLM gateway whose calls, under the daemon's own
// concurrency, took longer than 30 seconds. So every tick launched five more calls while
// the previous five were still burning their timeouts. Roughly 500 timeouts an hour, the
// unclassified backlog frozen at 6,684, and the same behaviour from the title-backfill
// loop alongside it.
//
// The model was not slow: probed sequentially against the same gateway it answered in
// 0.6 seconds. The load was self-inflicted, and a fixed interval cannot notice that.
//
// A loop whose work is failing must slow down. That is the whole of this file.
func TestFailureBackoff_SlowsDownWhenEverythingFails(t *testing.T) {
	base := 60 * time.Second
	b := newFailureBackoff(base)

	if got := b.interval(); got != base {
		t.Fatalf("initial interval = %v, want the configured %v", got, base)
	}

	// A tick where every unit of work failed.
	b.observe(5, 5)
	first := b.interval()
	if first <= base {
		t.Fatalf("interval after a total failure = %v, want > %v", first, base)
	}

	// Sustained failure keeps backing off.
	b.observe(5, 5)
	second := b.interval()
	if second <= first {
		t.Fatalf("interval = %v, want > previous %v under sustained failure", second, first)
	}
}

// It must not back off forever: an operator who fixes the endpoint should not wait an hour
// for the loop to notice. The cap bounds how bad it can get.
func TestFailureBackoff_IsCapped(t *testing.T) {
	b := newFailureBackoff(60 * time.Second)
	for i := 0; i < 50; i++ {
		b.observe(10, 10)
	}
	if got := b.interval(); got > maxBackfillBackoff {
		t.Fatalf("interval = %v, want <= the %v cap", got, maxBackfillBackoff)
	}
}

// Recovery must be immediate, not gradual. Once work is succeeding again the loop should
// return to its configured cadence at once — a slow ramp-down would leave a healthy
// deployment draining its backlog at a crawl for no reason.
func TestFailureBackoff_ResetsOnSuccess(t *testing.T) {
	base := 60 * time.Second
	b := newFailureBackoff(base)
	for i := 0; i < 5; i++ {
		b.observe(5, 5)
	}
	if b.interval() == base {
		t.Fatal("precondition: should be backed off")
	}

	b.observe(5, 0) // a clean tick
	if got := b.interval(); got != base {
		t.Fatalf("interval after success = %v, want an immediate return to %v", got, base)
	}
}

// A PARTIAL failure is the ambiguous case and must not trigger backoff: some chunks
// legitimately fail to classify (the model abstains), and treating that as endpoint
// trouble would throttle a loop that is working fine.
func TestFailureBackoff_IgnoresPartialFailure(t *testing.T) {
	base := 60 * time.Second
	b := newFailureBackoff(base)
	b.observe(5, 2) // 2 of 5 failed
	if got := b.interval(); got != base {
		t.Fatalf("interval after partial failure = %v, want the base %v — partial failure "+
			"is normal, not congestion", got, base)
	}
}

// An empty tick (nothing to do) is not failure and must not back off — otherwise a
// drained queue would slow the loop that has to notice new work arriving.
func TestFailureBackoff_EmptyTickIsNeutral(t *testing.T) {
	base := 60 * time.Second
	b := newFailureBackoff(base)
	b.observe(0, 0)
	if got := b.interval(); got != base {
		t.Fatalf("interval after an empty tick = %v, want %v", got, base)
	}
}

// A zero or negative base disables the loop upstream; the helper must not divide by zero
// or return something nonsensical if constructed anyway.
func TestFailureBackoff_DegenerateBase(t *testing.T) {
	b := newFailureBackoff(0)
	b.observe(5, 5)
	if got := b.interval(); got < 0 {
		t.Fatalf("interval = %v, want a non-negative duration", got)
	}
}

// The loop must actually USE the backoff, not just own one. A total-failure tick has to
// change the cadence, which is the property that would have stopped the 2026-07-30
// pile-up.
func TestClassifyBackfiller_ObserveTickBacksOffOnTotalFailure(t *testing.T) {
	b := &ClassifyBackfiller{Logger: zerolog.Nop()}
	backoff := newFailureBackoff(30 * time.Second)

	b.observeTick(backoff, &ClassifyBackfillResult{Processed: 5, Failed: 5})
	if got := backoff.interval(); got <= 30*time.Second {
		t.Fatalf("interval = %v, want > 30s after a fully-failed tick", got)
	}

	b.observeTick(backoff, &ClassifyBackfillResult{Processed: 5, Failed: 0})
	if got := backoff.interval(); got != 30*time.Second {
		t.Fatalf("interval = %v, want an immediate return to 30s after a clean tick", got)
	}
}

// A nil result means "no evidence" — a DB counting error, or an idle tick — and must leave
// the cadence alone rather than blaming the endpoint.
func TestClassifyBackfiller_ObserveTickIgnoresNoEvidence(t *testing.T) {
	b := &ClassifyBackfiller{Logger: zerolog.Nop()}
	backoff := newFailureBackoff(30 * time.Second)
	b.observeTick(backoff, nil)
	if got := backoff.interval(); got != 30*time.Second {
		t.Fatalf("interval = %v, want the base 30s", got)
	}
}

// The skipped-but-not-failed case is the classifier abstaining, which is normal. It must
// NOT be read as congestion, or a deployment whose model declines a lot of chunks would
// throttle itself for no reason.
func TestClassifyBackfiller_SkippedIsNotFailure(t *testing.T) {
	b := &ClassifyBackfiller{Logger: zerolog.Nop()}
	backoff := newFailureBackoff(30 * time.Second)
	b.observeTick(backoff, &ClassifyBackfillResult{Processed: 5, Skipped: 5, Failed: 0})
	if got := backoff.interval(); got != 30*time.Second {
		t.Fatalf("interval = %v, want 30s — abstention is not endpoint trouble", got)
	}
}

// The titler loop needs the same protection: on 2026-07-30 it was the DOMINANT source of
// timeouts once the classifier had been backed off — 15 of 25 in a five-minute window.
func TestTitleBackfiller_ObserveTickBacksOffOnTotalFailure(t *testing.T) {
	b := &TitleBackfiller{Logger: zerolog.Nop()}
	backoff := newFailureBackoff(300 * time.Second)

	b.observeTick(backoff, &BackfillResult{Processed: 25, Failed: 25})
	if got := backoff.interval(); got <= 300*time.Second {
		t.Fatalf("interval = %v, want > 300s after a fully-failed tick", got)
	}

	b.observeTick(backoff, &BackfillResult{Processed: 25, Failed: 0})
	if got := backoff.interval(); got != 300*time.Second {
		t.Fatalf("interval = %v, want an immediate return to 300s", got)
	}
}
