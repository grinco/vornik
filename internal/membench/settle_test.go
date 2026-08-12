package membench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A benchmark must measure a SETTLED corpus.
//
// vornik embeds asynchronously through a queue; hindsight's retain is synchronous.
// The harness ingests an item and immediately recalls it, so the vornik arm races
// its own embed queue while the external arm never does. On 2026-08-12 that handed
// hindsight a fabricated recall win: 0.917 against 1.000 on six LongMemEval items,
// where re-running the identical six items against the SAME fully-embedded corpus
// scored 1.000. Per-item retrieved-chunk counts were 8/1/7/3/8/8 — the shape of a
// race, not of a retrieval quality difference.
//
// Two independent failures let it through, and both are "absence read as a value":
//
//  1. EmbeddingReadiness called an ADMIN-only route while the adapter authenticates
//     with a companion key. It returned 403 on every run ever made with a companion
//     key, readiness() mapped the error to nil, and the run was still stamped
//     trustworthy. The signal added in §13.11 to catch exactly this had never once
//     worked.
//  2. Readiness was sampled at result assembly — AFTER scoring — so even with
//     access it would have reported the drained end state. A post-hoc sample of a
//     draining queue is systematically optimistic: it cannot show that the second
//     item was scored against a corpus that was still being indexed.
//
// So readiness is no longer merely reported. The runner WAITS for it, per item,
// before scoring — and refuses when it cannot establish it.

// settlingSystem models a draining ingest queue, which is what settle waits on.
type settlingSystem struct {
	*fakeSystem
	polls      int
	drainAfter int   // queue reaches 0 on this poll
	stuckAt    int64 // a queue that never drains
	err        error
	readiness  float64
}

func (s *settlingSystem) PendingIngest(_ context.Context) (int64, error) {
	s.polls++
	if s.err != nil {
		return 0, s.err
	}
	if s.stuckAt > 0 {
		return s.stuckAt, nil
	}
	if s.drainAfter > 0 && s.polls < s.drainAfter {
		return 7, nil
	}
	return 0, nil
}

func (s *settlingSystem) EmbeddingReadiness(_ context.Context) (float64, error) {
	if s.readiness > 0 {
		return s.readiness, nil
	}
	return 1.0, nil
}

func settleRunner(t *testing.T, sys MemorySystem) *Runner {
	t.Helper()
	r := tier2Runner(t, sys)
	r.SettleTimeout = 2 * time.Second
	r.SettlePollInterval = 10 * time.Millisecond
	return r
}

func TestSettle_WaitsUntilTheCorpusIsSearchable(t *testing.T) {
	sys := &settlingSystem{fakeSystem: newFakeSystem("vornik"), drainAfter: 3}
	r := settleRunner(t, sys)

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sys.polls < 3 {
		t.Errorf("polled %d times; the runner did not wait for readiness to reach 1.0, "+
			"which is how the vornik arm scored a corpus that was still being indexed",
			sys.polls)
	}
	// And the readiness it reports must be the one observed while SCORING, not a
	// post-hoc sample of a queue that has since drained.
	if res.EmbeddingReadiness == nil {
		t.Fatal("no readiness recorded")
	}
	if *res.EmbeddingReadiness != 1.0 {
		t.Errorf("readiness = %v, want 1.0", *res.EmbeddingReadiness)
	}
}

// TestSettle_RefusesWhenTheQueueCannotBeRead is the 403 regression. An unreachable
// signal must stop the run, not degrade to a silent guess: the whole point is that a
// mid-drain corpus scores like a settled one.
func TestSettle_RefusesWhenTheQueueCannotBeRead(t *testing.T) {
	sys := &settlingSystem{fakeSystem: newFakeSystem("vornik"), err: errors.New("http 403")}

	_, err := settleRunner(t, sys).Run(context.Background(), "")
	if err == nil {
		t.Fatal("a run whose readiness could not be read was allowed to score; that is " +
			"exactly the 403 that let a partially-embedded corpus be measured as if warm")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should carry the underlying cause, or an operator cannot tell "+
			"a permissions problem from a slow queue", err)
	}
}

// TestSettle_RefusesWhenTheQueueNeverDrains: a corpus with work still queued is not
// a measurable corpus. Timing out must fail the run rather than score whatever
// happens to be indexed, which reports a retrieval number for an indexing backlog.
func TestSettle_RefusesWhenTheQueueNeverDrains(t *testing.T) {
	sys := &settlingSystem{fakeSystem: newFakeSystem("vornik"), stuckAt: 12}
	r := settleRunner(t, sys)
	r.SettleTimeout = 100 * time.Millisecond

	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("a corpus stuck at 40% embedded was scored anyway")
	}
	if !strings.Contains(err.Error(), "12") {
		t.Errorf("error %q should state how much work was still queued", err)
	}
}

// TestSettle_SystemsThatCannotReportAreNotBlocked keeps the external arm runnable.
// An external service with no readiness concept is not a system racing a queue —
// hindsight's retain is synchronous, so there is nothing to wait for. Refusing here
// would block the comparison for a property the other system does not have.
//
// This is a real asymmetry and it is disclosed rather than papered over: the vornik
// arm waits, the external arm has nothing to wait for.
func TestSettle_SystemsThatCannotReportAreNotBlocked(t *testing.T) {
	// plain fakeSystem implements no EmbeddingReadinessReporter at all.
	if _, err := settleRunner(t, newFakeSystem("external")).Run(context.Background(), ""); err != nil {
		t.Errorf("a system with no readiness concept was blocked: %v", err)
	}
}

// TestSettle_SettleDisabledSkipsTheWait gives an operator an escape hatch for a
// deployment whose readiness reporting is broken — but it must be CHOSEN. A bare
// zero-value field would let the wait vanish by omission, which is how the original
// signal came to be silently absent for weeks.
func TestSettle_SettleDisabledSkipsTheWait(t *testing.T) {
	sys := &settlingSystem{fakeSystem: newFakeSystem("vornik"), err: errors.New("http 403")}
	r := tier2Runner(t, sys)
	r.SettleDisabled()

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Errorf("SettleDisabled() must skip the wait entirely: %v", err)
	}
}

// TestSettle_OmittingTheTimeoutStillWaits: a zero SettleTimeout means "use the
// default", NOT "skip". Every existing caller leaves the field unset, and if zero
// meant skip they would all silently keep the old broken behaviour.
func TestSettle_OmittingTheTimeoutStillWaits(t *testing.T) {
	sys := &settlingSystem{fakeSystem: newFakeSystem("vornik"), err: errors.New("http 403")}
	r := tier2Runner(t, sys)
	r.SettleTimeout = 0

	if _, err := r.Run(context.Background(), ""); err == nil {
		t.Fatal("an unset SettleTimeout skipped the wait; zero must mean the default, or " +
			"the fix is inert for every caller that does not opt in")
	}
}

// TestSettle_AnEmptyQueueWithLowReadinessIsNotAnError is the case that killed the
// first version of this fix, and the reason settle watches the QUEUE rather than a
// readiness threshold.
//
// A deployment with no embedder configured sits at readiness 0.0 permanently. That
// is a legitimate keyword-only configuration, not a warming queue — waiting for it
// to reach 1.0 would hang for the full timeout and then fail a valid baseline. An
// empty queue means settled, whatever the ratio says.
func TestSettle_AnEmptyQueueWithLowReadinessIsNotAnError(t *testing.T) {
	sys := &settlingSystem{fakeSystem: newFakeSystem("no-embedder"), readiness: 0.04}
	r := settleRunner(t, sys)

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("a settled keyword-only corpus was refused: %v", err)
	}
	if res.EmbeddingReadiness == nil || *res.EmbeddingReadiness != 0.04 {
		t.Errorf("readiness = %v, want 0.04 recorded — the run measured something "+
			"different, and a reader has to be able to see that", res.EmbeddingReadiness)
	}
}
