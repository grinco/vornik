package narrator

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// testHarness bundles a Narrator wired to in-memory fakes + fast
// timer knobs (real timers, small durations) so trigger/debounce
// tests run in milliseconds without a fake clock abstraction. Run()
// is started on its own goroutine; t.Cleanup cancels it.
type testHarness struct {
	t          *testing.T
	N          *Narrator
	Sub        *fakeSub
	Store      *fakeStore
	Pub        *fakePub
	Executions *fakeExecutions
	Recorder   *orderedRecorder
	Lines      chan *persistence.ExecutionNarration
	cancel     context.CancelFunc
}

// newTestHarness builds a Narrator wired to in-memory fakes with
// fast timer knobs. Any configure funcs run BEFORE Run() starts on
// its own goroutine — all Narrator field configuration MUST happen
// via configure, not by mutating h.N.* after construction, since the
// Run goroutine reads those fields concurrently once started (the
// race detector catches exactly this mistake).
func newTestHarness(t *testing.T, configure ...func(n *Narrator)) *testHarness {
	t.Helper()
	rec := &orderedRecorder{}
	sub := newFakeSub()
	store := newFakeStore(rec)
	pub := newFakePub(rec)
	execs := newFakeExecutions()
	lines := make(chan *persistence.ExecutionNarration, 256)

	n := &Narrator{
		Sub:              sub,
		Pub:              pub,
		Store:            store,
		Executions:       execs,
		Debounce:         20 * time.Millisecond,
		LongToolThresh:   20 * time.Millisecond,
		MinLineInterval:  15 * time.Millisecond,
		MaxLines:         DefaultMaxLines,
		MaxCostUSD:       DefaultMaxCostUSD,
		IdlePollInterval: 10 * time.Millisecond,
		IdleThreshold:    30 * time.Millisecond,
		onLine: func(row *persistence.ExecutionNarration) {
			lines <- row
		},
	}
	for _, cfg := range configure {
		cfg(n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &testHarness{t: t, N: n, Sub: sub, Store: store, Pub: pub, Executions: execs, Recorder: rec, Lines: lines, cancel: cancel}
	go n.Run(ctx)
	t.Cleanup(cancel)
	return h
}

// awaitLine blocks until the next narration line lands or the
// timeout elapses (test failure on timeout — no silent false
// negatives).
func (h *testHarness) awaitLine(timeout time.Duration) *persistence.ExecutionNarration {
	h.t.Helper()
	select {
	case row := <-h.Lines:
		return row
	case <-time.After(timeout):
		h.t.Fatalf("timed out waiting for a narration line")
		return nil
	}
}

// expectNoLine asserts no line arrives within the window — used to
// pin negative cases (collapsed start, suppressed by min_line_interval).
func (h *testHarness) expectNoLine(window time.Duration) {
	h.t.Helper()
	select {
	case row := <-h.Lines:
		h.t.Fatalf("unexpected narration line: %+v", row)
	case <-time.After(window):
	}
}
