package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// reenterableBlockingHandler is blockingSystemHandler with one difference that
// the resume tests need: it can be entered more than once. blockingSystemHandler
// closes `entered` on every call, so a resumed execution re-entering the step
// panics on a double close. Here the signal fires once and later entries pass
// straight through the (already released) gate.
type reenterableBlockingHandler struct {
	name    string
	once    sync.Once
	entered chan struct{}
	gate    chan struct{}
	retErr  error
}

func (h *reenterableBlockingHandler) Name() string { return h.name }

func (h *reenterableBlockingHandler) Execute(_ context.Context, _ SystemStepInput) (SystemStepResult, error) {
	h.once.Do(func() { close(h.entered) })
	<-h.gate
	return SystemStepResult{}, h.retErr
}

// releaseWhenPaused closes the gate once the task row reads PAUSED, so the
// blocked goroutine can observe its cancelled context and clean up. Without
// it, pauseWithReason's waitForExecutionCleanup burns its full 30s budget
// waiting for a goroutine the test is still holding.
func releaseWhenPaused(tr *MockTaskRepo, gate chan struct{}) {
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if tk, err := tr.Get(context.Background(), "t1"); err == nil &&
				tk.Status == persistence.TaskStatusPaused {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(gate)
	}()
}

// pauseClaimTestExecutor builds an executor whose single workflow step is a
// system handler that blocks until its gate is released. Shared by the tests
// below, which differ only in what they do at the seam.
func pauseClaimTestExecutor(t *testing.T, retErr error) (*Executor, *MockExecRepo, *MockTaskRepo, chan struct{}, chan struct{}) {
	t.Helper()

	entered := make(chan struct{})
	gate := make(chan struct{})
	handler := &reenterableBlockingHandler{
		name:    "test.block",
		entered: entered,
		gate:    gate,
		retErr:  retErr,
	}
	reg := NewSystemHandlerRegistry()
	reg.Register(handler)

	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "implement",
				Steps: map[string]registry.WorkflowStep{
					"implement": {Type: "system", Handler: "test.block", OnFail: "recover", OnSuccess: "done"},
					"recover":   {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED", Message: "system step recovery exhausted"},
				},
			},
		},
	})
	tr.AddTask(&persistence.Task{
		ID:          "t1",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	})
	return e, er, tr, entered, gate
}

// TestShutdown_PausesEvenWhenTheGoroutineExitsFirst — regression guard for the
// backlog item "Shutdown's pause races the in-flight goroutine's own state
// write, losing PausedReason" (filed 2026-09-03).
//
// Measured before the fix: 9 failures in 6000 runs of
// TestShutdown_SystemStepOnFailDoesNotMarkFailedOnShutdown, and every one of
// the nine failed ALL THREE assertions together (task RUNNING, execution
// RUNNING, reason empty). That triple is the tell: nothing was clobbered,
// because the pause never wrote anything at all.
//
// The mechanism: setting shuttingDown is also the signal every on_fail guard
// in the workflow loop waits for. The instant Shutdown flips it, the in-flight
// goroutine stops routing, returns through runExecution's isShuttingDown arm,
// and its deferred cleanupExecution DELETES ITS OWN HANDLE. pauseWithReason
// then looks up a task that is no longer in activeExecutions, returns
// ErrNoActiveExecution, and Shutdown logs "pause failed (execution will be
// recovered as RUNNING)" and moves on. One boot later, Recover()'s orphan
// sweep marks the RUNNING row FAILED/ORPHANED: the checkpoint is abandoned and
// an attempt is burnt by nothing but a restart.
//
// This test forces that interleaving rather than hoping for it: the seam fires
// after Shutdown has snapshotted the active set, releases the blocked step,
// and waits for the goroutine to be gone before the pause loop runs. Presence
// in activeExecutions is a liveness fact, not an authority to write
// (pause-write-ownership design §3/§4) — so the pause must still land.
func TestShutdown_PausesEvenWhenTheGoroutineExitsFirst(t *testing.T) {
	e, er, tr, entered, gate := pauseClaimTestExecutor(t,
		errors.New("system step failed: service unavailable"))

	// The seam: by the time the pause loop runs, the goroutine has bailed
	// on shutdown and removed its own handle.
	e.testHookAfterShutdownSnapshot = func() {
		close(gate)
		deadline := time.Now().Add(2 * time.Second)
		for e.IsExecuting("t1") && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		assert.False(t, e.IsExecuting("t1"),
			"seam precondition: the goroutine must have exited before the pause loop")
	}

	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler — step not in flight")
	}
	require.True(t, e.IsExecuting("t1"), "precondition: execution must be active before Shutdown")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx), "graceful shutdown must drain within budget")

	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusPaused, task.Status,
		"shutdown must pause the task even though its goroutine exited first, got %s", task.Status)

	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status,
		"execution row must be PAUSED — left RUNNING it is ORPHANED by Recover()'s sweep on next boot")
	st := loadExecutionState(exec)
	assert.Equal(t, PauseReasonShutdown, st.PausedReason,
		"pause reason must be 'shutdown' so Recover() auto-resumes from the checkpoint")
}

// TestShutdown_DoesNotReopenAnExecutionThatFinishedFirst — the safety property
// the fix must not trade away. Removing the activeExecutions presence check
// also removes the accidental guarantee it carried: that the execution had not
// already reached a terminal state. A blind UpdateStatus(PAUSED) would reopen
// a row the goroutine had just COMPLETED, and Recover() would then re-run a
// finished workflow on the next start. The status writes are conditional
// (compare-and-set) for exactly this case (design §5.3).
func TestShutdown_DoesNotReopenAnExecutionThatFinishedFirst(t *testing.T) {
	// retErr nil: the system step SUCCEEDS, so the workflow runs to its
	// COMPLETED terminal instead of routing on_fail.
	e, er, tr, entered, gate := pauseClaimTestExecutor(t, nil)

	e.testHookAfterShutdownSnapshot = func() {
		close(gate)
		deadline := time.Now().Add(2 * time.Second)
		for e.IsExecuting("t1") && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}

	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler — step not in flight")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx))

	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusCompleted, exec.Status,
		"a completed execution must not be reopened as PAUSED by a shutdown that lost the race")

	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status,
		"a completed task must not be reopened as PAUSED")
}

// TestPause_SurvivesAStaleCheckpointWrite — the second, latent hazard in the
// same seam, and the one the backlog item actually described: the in-flight
// goroutine holds an in-memory executionState loaded BEFORE the pause stamped
// PausedReason, so its next checkpoint writes that stale snapshot back and the
// reason is gone. Recover() dispatches on the reason, so a PAUSED execution
// without one is skipped — the task is stuck across a restart with nothing
// recording why.
//
// The claim closes it at the funnel every snapshot write passes through: a
// claimed pause is stamped on the way past, whoever is writing (design §5.4).
// Deliberately NOT tested here: suppression of the write itself. A step that
// genuinely finished during the SIGTERM drain must keep its checkpoint.
func TestPause_SurvivesAStaleCheckpointWrite(t *testing.T) {
	e, er, tr, entered, gate := pauseClaimTestExecutor(t,
		errors.New("system step failed: service unavailable"))

	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler — step not in flight")
	}

	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)

	// The goroutine's copy of the state: loaded before the pause lands.
	stale := loadExecutionState(exec)
	require.Empty(t, stale.PausedReason, "precondition: loaded before the pause")

	releaseWhenPaused(tr, gate)
	_, err = e.Pause("t1")
	require.NoError(t, err)

	// ...and written back after it. Pre-fix this erases the reason.
	require.NoError(t, e.saveExecutionState(context.Background(), exec, stale))

	after, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, PauseReasonOperator, loadExecutionState(after).PausedReason,
		"a checkpoint written from a pre-pause snapshot must not erase the pause reason")
}

// TestResume_ClearsTheClaimSoLaterWritesDoNotRestampIt — the other side of
// §5.4: once the operator resumes, the reason must stay cleared, or every
// subsequent checkpoint would re-stamp a pause that is over and Recover()
// would treat a live execution as resumable-paused after the next restart.
func TestResume_ClearsTheClaimSoLaterWritesDoNotRestampIt(t *testing.T) {
	e, er, tr, entered, gate := pauseClaimTestExecutor(t,
		errors.New("system step failed: service unavailable"))

	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler — step not in flight")
	}

	releaseWhenPaused(tr, gate)
	_, err := e.Pause("t1")
	require.NoError(t, err)

	// The resumed execution runs to its own terminal (the gate is open
	// now, so the step fails and the workflow unwinds); every state write
	// it makes on the way is a chance to re-stamp a released claim.
	_, err = e.Resume("t1")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !e.IsExecuting("t1") },
		5*time.Second, 5*time.Millisecond, "resumed execution must finish")

	after, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Empty(t, loadExecutionState(after).PausedReason,
		"a resumed execution must not have its pause reason re-stamped by a later checkpoint")
}

// TestRun_MakesNoWholeRowExecutionUpdate — regression guard for the second
// clobber, the one the backlog item hypothesised and the audit found in a real
// path (pause-write-ownership design §3.2).
//
// runExecution persisted the resolved workflow id with a fire-and-forget
// `execRepo.Update(ctx, execution)`. Update writes the WHOLE row from the
// struct it is handed — including Status (stamped RUNNING twenty lines
// earlier) and StateSnapshot (as loaded at dispatch). A pause landing in that
// window was erased by it: reason gone, RUNNING reinstated, on the operator
// path as much as the shutdown one, and before the first step runs — so no
// pause test would ever have caught it.
//
// The fix is to stop making a write that carries fields nobody asked for, so
// the assertion is on the write itself: the run path must issue no whole-row
// Update at all.
func TestRun_MakesNoWholeRowExecutionUpdate(t *testing.T) {
	e, er, _, entered, gate := pauseClaimTestExecutor(t, nil)
	close(gate) // let the step finish immediately

	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler")
	}
	require.Eventually(t, func() bool { return !e.IsExecuting("t1") },
		5*time.Second, 5*time.Millisecond, "execution must finish")

	assert.Equal(t, 0, er.UpdateCalls(),
		"the run path must not write the whole execution row: a stale Status/StateSnapshot "+
			"in that struct erases a concurrent pause (design §3.2)")

	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, "wf1", exec.WorkflowID,
		"the resolved workflow id must still be persisted — by the narrow writer")
}

// TestShutdown_CancelWins — the fourth writer (review round 1, F4). A cancel
// that lands between the claim and the pause's status write must stand: the
// conditional write is refused, the row stays CANCELLED, and the claim is
// released so no later checkpoint stamps a pause reason onto a cancelled
// execution.
func TestShutdown_CancelWins(t *testing.T) {
	e, er, tr, entered, gate := pauseClaimTestExecutor(t,
		errors.New("system step failed: service unavailable"))

	e.testHookAfterShutdownSnapshot = func() {
		// Cancel lands after the pause is claimed, before it is written.
		assert.NoError(t, e.Cancel("t1"))
		close(gate)
		deadline := time.Now().Add(2 * time.Second)
		for e.IsExecuting("t1") && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}

	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx))

	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusCancelled, exec.Status,
		"a cancelled execution must not be reopened as PAUSED by a shutdown that lost the race")

	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusCancelled, task.Status,
		"a cancelled task must not be flipped to PAUSED")

	// The claim must be gone: a later state write must not stamp a reason.
	require.NoError(t, e.saveExecutionState(context.Background(), exec, loadExecutionState(exec)))
	after, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	assert.Empty(t, loadExecutionState(after).PausedReason,
		"a refused pause must release its claim — a cancelled execution has no pause reason")
}
