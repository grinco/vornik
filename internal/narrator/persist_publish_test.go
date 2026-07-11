package narrator

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// TestPersistThenPublish_OrderingObserved pins design §5.3's
// load-bearing ordering: for every line, the store Insert is
// observed to happen strictly before the bus Publish.
func TestPersistThenPublish_OrderingObserved(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	log := h.Recorder.snapshot()
	if len(log) != 2 {
		t.Fatalf("expected exactly 2 ordered events (store, publish), got %v", log)
	}
	if log[0] != "store:"+testExecID {
		t.Errorf("first recorded event = %q, want a store append", log[0])
	}
	if log[1] != "publish:"+testExecID {
		t.Errorf("second recorded event = %q, want a publish", log[1])
	}
}

// TestPersistThenPublish_CrashBetweenPersistAndPublish simulates a
// crash right after the store write lands but before the bus
// fan-out completes: the line must be present in storage and
// absent from the bus — never the reverse (design §5.3, §7 — "a
// crash between persist and publish leaves the line in store,
// absent from bus"). Modelled with a Publish that panics: Insert has
// already returned successfully by the time Publish is invoked, so
// the store row exists; the panic represents the process dying
// mid-fan-out, and no live subscriber ever received the event.
func TestPersistThenPublish_CrashBetweenPersistAndPublish(t *testing.T) {
	rec := &orderedRecorder{}
	store := newFakeStore(rec)
	pub := &panicOnPublish{}
	execs := newFakeExecutions()
	execs.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusRunning)

	n := &Narrator{
		Sub:        newFakeSub(),
		Pub:        pub,
		Store:      store,
		Executions: execs,
	}
	n.ensureInit()

	st := newExecutionState(testExecID, "proj-1", "task-1", time.Now())
	n.states[testExecID] = st

	func() {
		defer func() { _ = recover() }() // the simulated crash
		n.emitLine(context.Background(), testExecID, st, triggerStepStarted,
			templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)
	}()

	rows := store.all()
	if len(rows) != 1 {
		t.Fatalf("expected the line to be persisted before the simulated crash, got %d rows", len(rows))
	}
	if rows[0].Text == "" {
		t.Error("persisted row must carry the real narration text")
	}
	if !pub.reached {
		t.Fatal("test premise broken: Publish should have been reached (that's where the crash happens)")
	}
	if pub.delivered {
		t.Fatal("Publish must not have completed delivery — the crash happened mid-fan-out")
	}
	// The store write is observed strictly before the crash — the
	// ordering log only has the "store" entry, never a "publish" one.
	log := rec.snapshot()
	if len(log) != 1 || log[0] != "store:"+testExecID {
		t.Errorf("ordering log = %v, want exactly one store entry (publish never completed)", log)
	}
}

// TestPersistThenPublish_StoreFailure_NeverPublishes — when the
// store write itself fails, emitLine must return before ever calling
// Publish (a persist failure must not create a live-only phantom
// line the story then can't reproduce on reload).
func TestPersistThenPublish_StoreFailure_NeverPublishes(t *testing.T) {
	rec := &orderedRecorder{}
	store := newFakeStore(rec)
	store.failInsert = true
	pub := newFakePub(rec)
	execs := newFakeExecutions()
	execs.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusRunning)

	n := &Narrator{Sub: newFakeSub(), Pub: pub, Store: store, Executions: execs}
	n.ensureInit()
	st := newExecutionState(testExecID, "proj-1", "task-1", time.Now())
	n.states[testExecID] = st

	n.emitLine(context.Background(), testExecID, st, triggerStepStarted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	if len(pub.all()) != 0 {
		t.Fatal("Publish must never be called when the store Insert fails")
	}
	if st.linesEmitted != 0 {
		t.Errorf("linesEmitted = %d, want 0 (a failed persist doesn't count against the line cap)", st.linesEmitted)
	}
}

// panicOnPublish is an EventPublisher that panics mid-call,
// simulating a process crash after the store Insert has already
// returned but before the bus fan-out finishes. reached is set
// before the panic (proving Publish was invoked at all); delivered
// is deliberately never set anywhere in this fake — its zero value
// (false) IS the assertion that delivery never completed.
type panicOnPublish struct {
	reached   bool
	delivered bool
}

func (p *panicOnPublish) Publish(_ context.Context, _, _ string, _ any) int64 {
	p.reached = true
	panic("simulated crash mid-publish")
}
