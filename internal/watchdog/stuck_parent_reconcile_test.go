package watchdog

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

type fakeParentSweeper struct{ calls int }

func (f *fakeParentSweeper) SweepStuckWaitingForChildren(_ context.Context) { f.calls++ }

// Audited 2026-08-18. Four production paths terminalise a CHILD task without
// driving the parent-unblock hook, so the parent can sit in
// WAITING_FOR_CHILDREN with every child already terminal:
//
//	internal/ui/execution_actions.go  cancelOne     — cancels the task, no hook
//	internal/chat/parser.go           executeCancelTask — no executor reference at all
//	internal/watchdog/watchdog.go     approval timeout  — UpdateStatus(CANCELLED)
//	internal/watchdog/watchdog.go     handleStuck fail  — task marked FAILED
//
// (executor.Cancel likewise cascades orphan EXECUTIONS but never calls the
// hook.) This is occurrences #3-#6 of a bug class already at #2: #1 was an
// operator-CLOSED child never waking its parent (2026-05-21), #2 was
// child-cancel across three entry points (2026-06-07, 3f1f3e76).
//
// The only existing backstop, executor.sweepStuckWaitingForChildren, runs at
// DAEMON STARTUP ONLY. So a parent stranded by any of those paths stays
// stranded until someone restarts the daemon — which is precisely why the
// production ledger shows no stuck parents today: this box restarts often, and
// no child has reached FAILED in the last 30 days. Correct by luck, not by
// construction.
//
// Running the same idempotent sweep on the watchdog tick contains all four
// without touching any of their call sites. It cannot change a task's fate that
// the sweep would not already have reached at the next restart.
func TestReconcileStuckParents_RunsTheSweepOnEveryTick(t *testing.T) {
	sweeper := &fakeParentSweeper{}
	w := New(Config{}, &stubExecRepo{}, nil, zerolog.Nop(), nil).
		WithParentUnblockSweeper(sweeper)

	w.reconcileStuckParents(context.Background())
	w.reconcileStuckParents(context.Background())

	assert.Equal(t, 2, sweeper.calls,
		"the sweep is idempotent and must run per tick, not once per process lifetime — "+
			"startup-only is what let a stranded parent wait for a restart")
}

// Unwired stays a no-op so a deployment that has not passed the sweeper keeps a
// working watchdog rather than panicking on every scan.
func TestReconcileStuckParents_NilSweeperIsNoop(_ *testing.T) {
	w := New(Config{}, &stubExecRepo{}, nil, zerolog.Nop(), nil)
	w.reconcileStuckParents(context.Background())
}
