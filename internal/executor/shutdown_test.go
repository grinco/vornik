package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestRecover_AutoResumesShutdownPaused — the headline contract: an
// execution paused via Shutdown() (state.PausedReason = "shutdown",
// status = PAUSED) gets flipped back to RUNNING and resumed on the
// next daemon start. Without this, every graceful restart leaves
// in-flight work permanently paused.
func TestRecover_AutoResumesShutdownPaused(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now(),
	})

	state := executionState{
		CurrentStepID: "implement",
		PausedReason:  PauseReasonShutdown,
	}
	snap, _ := json.Marshal(state)
	exec := &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		ProjectID:     "p1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: snap,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.Recover(context.Background()))

	// Status flipped back to RUNNING in the row.
	got, err := er.Get(context.Background(), "e1")
	require.NoError(t, err)
	assert.Equal(t, persistence.ExecutionStatusRunning, got.Status,
		"shutdown-paused execution must be flipped back to RUNNING for the resume goroutine")

	// And the executor is tracking it as active again. We don't
	// assert on the goroutine actually completing because the mock
	// runtime would need a real container plan; tracking-as-active
	// is the same evidence Recover provides for any RUNNING row.
	assert.True(t, e.IsExecuting("t1"))
}

// TestRecover_LeavesOperatorPausedAlone — operator-paused executions
// (manual vornikctl Pause) must NOT auto-resume. They wait for an
// explicit Resume call. Without this guard, an operator-staged pause
// (e.g. "freeze this task while I investigate") gets clobbered on
// every daemon restart.
func TestRecover_LeavesOperatorPausedAlone(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t2",
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now(),
	})

	state := executionState{
		CurrentStepID: "review",
		PausedReason:  PauseReasonOperator,
	}
	snap, _ := json.Marshal(state)
	exec := &persistence.Execution{
		ID:            "e2",
		TaskID:        "t2",
		ProjectID:     "p1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: snap,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.Recover(context.Background()))

	got, _ := er.Get(context.Background(), "e2")
	assert.Equal(t, persistence.ExecutionStatusPaused, got.Status,
		"operator-paused execution must stay PAUSED across restarts")
	assert.False(t, e.IsExecuting("t2"),
		"operator-paused execution must not be added to activeExecutions")
}

// TestRecover_LeavesAwaitingChildrenAlone — delegation pauses
// (parent waiting for child tasks) resume via the scheduler when
// the last child completes, NOT via Recover. The same guard as
// operator-paused applies.
func TestRecover_LeavesAwaitingChildrenAlone(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t3",
		ProjectID: "p1",
		Status:    persistence.TaskStatusWaitingForChildren,
		CreatedAt: time.Now(),
	})

	state := executionState{
		CurrentStepID: "delegate",
		PausedReason:  PauseReasonAwaitingChildren,
	}
	snap, _ := json.Marshal(state)
	exec := &persistence.Execution{
		ID:            "e3",
		TaskID:        "t3",
		ProjectID:     "p1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: snap,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.Recover(context.Background()))

	got, _ := er.Get(context.Background(), "e3")
	assert.Equal(t, persistence.ExecutionStatusPaused, got.Status)
	assert.False(t, e.IsExecuting("t3"))
}

// TestRecover_LeavesUnknownReasonAlone — backward compat: if a row
// is PAUSED but the snapshot has no PausedReason (legacy data, or
// state corruption), don't auto-resume. The default behavior should
// be conservative; an operator can always explicitly Resume.
func TestRecover_LeavesUnknownReasonAlone(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t4",
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now(),
	})

	// No PausedReason field set.
	exec := &persistence.Execution{
		ID:            "e4",
		TaskID:        "t4",
		ProjectID:     "p1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"currentStepId":"implement"}`),
	}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.Recover(context.Background()))

	got, _ := er.Get(context.Background(), "e4")
	assert.Equal(t, persistence.ExecutionStatusPaused, got.Status,
		"PAUSED with no reason must stay PAUSED — conservative default")
	assert.False(t, e.IsExecuting("t4"))
}

// TestRunExecution_ShutdownBailoutDoesNotMarkFailed — the race that
// caused a real task to fail on graceful restart: pauseWithReason
// stops the agent container, the executor goroutine in WaitForExit
// returns "podman wait failed", and the existing error path walked
// into the retry loop, exhausted attempts, and overwrote the row's
// PAUSED status with FAILED. The shuttingDown bail-out in the error
// handler exits the goroutine cleanly so pause's DB write stays
// visible and Recover() picks it up on next start.
//
// Test ordering matches the real lifecycle:
//  1. Execute starts a goroutine that hits WaitForExit blocking
//     on a gate (simulates a normal in-flight execution).
//  2. Shutdown begins → shuttingDown=true.
//  3. The gate releases with an error (simulating
//     pauseWithReason stopping the container, which makes
//     WaitForExit return).
//  4. Goroutine sees the error AND shuttingDown — must bail
//     cleanly, not write FAILED.
func TestRunExecution_ShutdownBailoutDoesNotMarkFailed(t *testing.T) {
	rt := NewMockRuntime()
	gate := make(chan struct{})
	rt.waitGate = gate
	rt.waitErr = errors.New("podman wait failed: exit status 1")
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "run",
				Steps:      map[string]registry.WorkflowStep{"run": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
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

	// Step 1: kick off execution — goroutine will block in
	// WaitForExit until we release the gate.
	require.NoError(t, e.Execute("t1"))
	require.Eventually(t, func() bool {
		return e.IsExecuting("t1")
	}, time.Second, 5*time.Millisecond)

	// Step 2: simulate Shutdown's flag flip.
	e.mu.Lock()
	e.shuttingDown = true
	e.mu.Unlock()

	// Step 3: release the gate so WaitForExit returns the error.
	close(gate)

	// Step 4: goroutine must exit without flipping the task to
	// FAILED and without consuming retry budget.
	require.Eventually(t, func() bool {
		return !e.IsExecuting("t1")
	}, time.Second, 10*time.Millisecond)

	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.NotEqual(t, persistence.TaskStatusFailed, task.Status,
		"task must not be marked FAILED during shutdown bailout — got %s", task.Status)
	rt.mu.Lock()
	starts := rt.startCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, starts,
		"shutdown bailout must not consume retry attempts — got %d container starts", starts)
}

// TestShutdown_DrainsInFlightExecutionToPausedForResume — the
// goroutine/shutdown complement to the existing direct-pause and
// shutdown-bailout unit tests. Where TestStateCov_PauseWaitForExitError
// calls pauseWithReason() directly and TestRunExecution_ShutdownBailoutDoesNotMarkFailed
// flips shuttingDown by hand, this drives the REAL Shutdown(ctx)
// entrypoint against a genuinely in-flight agent step: a runExecution
// goroutine blocked inside WaitForExit (the window an agent/LLM step
// occupies while the container runs).
//
// The whole drain must wind down cleanly:
//   - no panic / no daemon crash,
//   - the execution row ends PAUSED with state.PausedReason = "shutdown"
//     (the marker Recover() keys on to auto-resume on next start),
//   - the task row ends PAUSED (so a late handleFailure/handleSuccess
//     can't stamp a terminal status over the pause),
//   - the in-flight goroutine bails via the shuttingDown arm rather
//     than the retry loop, so the executor drains to zero active.
//
// Determinism: the run reaches WaitForExit (signalled on `entered`)
// before Shutdown begins; pauseWithReason's StopContainer + the
// shared waitGate releasing with waitErr is what makes WaitForExit
// return for BOTH the pause's own wait and the runExecution goroutine.
// No sleeps — every wait is on a channel or require.Eventually.
func TestShutdown_DrainsInFlightExecutionToPausedForResume(t *testing.T) {
	rt := NewMockRuntime()
	gate := make(chan struct{})
	entered := make(chan struct{})
	rt.waitGate = gate
	rt.waitEntered = entered
	// When the gate releases, both the runExecution goroutine's
	// WaitForExit and pauseWithReason's own WaitForExit observe this
	// error — exactly what happens live when pause SIGTERMs the
	// container out from under an in-flight step.
	rt.waitErr = errors.New("podman wait failed: container stopped")
	// StopContainer — called by pauseWithReason during Shutdown, AFTER
	// shuttingDown is set — is what releases the gate, enforcing the live
	// causal order so the run goroutine always observes shuttingDown and
	// bails. The earlier manual close(gate) raced that flag and flaked
	// under `go test ./...` parallel load (2026-06-20).
	rt.stopReleasesWaitGate = true

	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "implement",
				Steps:      map[string]registry.WorkflowStep{"implement": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
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

	// Kick off the execution and wait until the agent step is actually
	// in flight (goroutine blocked in WaitForExit).
	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached WaitForExit — agent step not in flight")
	}
	require.True(t, e.IsExecuting("t1"), "precondition: execution must be active before Shutdown")

	// Run the real graceful Shutdown. It sets shuttingDown, calls
	// pauseWithReason (StopContainer + its own WaitForExit, which will
	// block on the shared gate), then Stop() drains the wg. Run it in a
	// goroutine so we can release the gate to unblock both WaitForExit
	// calls.
	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- e.Shutdown(ctx)
	}()

	// No manual gate release: StopContainer (inside Shutdown's
	// pauseWithReason, after shuttingDown is set) closes the gate, so both
	// WaitForExit calls return and the run goroutine takes the clean
	// shuttingDown bail-out arm — deterministically, in the live causal
	// order rather than racing a manual close.

	select {
	case err := <-shutdownErr:
		require.NoError(t, err, "graceful shutdown must drain within budget, not time out")
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return — drain stalled")
	}

	// Executor fully drained — no lingering active execution.
	require.Eventually(t, func() bool {
		return !e.IsExecuting("t1")
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 0, e.ActiveCount(), "all in-flight executions must drain on shutdown")

	// Execution row PAUSED with reason=shutdown — the contract Recover()
	// reads on next start to auto-resume.
	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status,
		"in-flight execution must end PAUSED after graceful shutdown")
	st := loadExecutionState(exec)
	assert.Equal(t, PauseReasonShutdown, st.PausedReason,
		"the pause reason must be 'shutdown' so Recover() auto-resumes it")

	// Task row PAUSED — guards the late-finaliser race.
	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusPaused, task.Status,
		"task must be PAUSED (not FAILED/COMPLETED) after graceful shutdown")

	// The goroutine bailed via the shutdown arm, NOT the retry loop:
	// exactly one container start.
	assert.Equal(t, 1, rt.StartCalls(),
		"shutdown drain must not consume retry attempts — got %d starts", rt.StartCalls())
}

// sharedWorkflowResolver returns the same single-step agent workflow used
// by both halves of the full-cycle test below. Both executors must resolve
// to the identical workflow so the resumed run re-enters the same step the
// shutdown paused at.
func sharedWorkflowResolver() *MockWorkflowResolver {
	return &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "implement",
				Steps:      map[string]registry.WorkflowStep{"implement": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	}
}

// TestShutdownResume_FullCycleResumesToCompleted is the Tier-2
// (https://docs.vornik.io "Graceful shutdown + resume") end-to-end characterization
// the existing tests stop short of. The two existing halves each cover only
// one hop:
//
//   - TestShutdown_DrainsInFlightExecutionToPausedForResume drives the real
//     Shutdown(ctx) on an in-flight execution and asserts it lands PAUSED
//     with reason=shutdown — but then stops; it never restarts.
//   - TestRecover_AutoResumesShutdownPaused flips a hand-built shutdown-PAUSED
//     row back to RUNNING via Recover() and asserts IsExecuting — but it
//     explicitly does NOT drive the resumed run to completion ("the mock
//     runtime would need a real container plan").
//
// What was missing — and what this adds — is the WHOLE cycle in one
// deterministic pass over the SAME persistence:
//
//  1. Executor #1 has a genuinely in-flight agent step (runExecution blocked
//     inside WaitForExit). Shutdown(ctx) pauses it → exec row PAUSED,
//     state.PausedReason = "shutdown".
//  2. A FRESH executor #2 is constructed over the SAME exec/artifact/task
//     repos (a daemon restart: process gone, DB survived) and Recover() is
//     called.
//  3. The paused execution auto-resumes, re-enters the SAME step, and drives
//     to COMPLETED — task row COMPLETED, exec row COMPLETED.
//
// The two anti-bug guarantees the backlog item names explicitly
// ("no double-spend/double-lease"):
//
//   - NO double-start: executor #2 uses a SEPARATE MockRuntime, so its
//     StartCalls() counts ONLY resume-side container starts. Resuming a
//     clean shutdown-pause (no surviving container — Shutdown SIGTERMed it)
//     must start the step's container exactly once. A second start would be
//     a double-spend (the agent/LLM step billed twice).
//   - NO double-lease / double-drive: recoverExecution's activeExecutions
//     guard means exactly one runExecution goroutine drives the task. We
//     assert the executor drains back to zero active after completion, so no
//     second goroutine lingers re-driving (the 2026-05-12 two-RUNNING-rows
//     class of bug).
//
// Determinism: phase 1 reuses the proven shutdown-drain seams
// (waitEntered → run is genuinely in WaitForExit before Shutdown begins;
// stopReleasesWaitGate → pauseWithReason's StopContainer is what releases
// the gate, enforcing the live causal order so the run goroutine observes
// shuttingDown and bails — the manual-close race that flaked on 2026-06-20).
// Phase 2's runtime is ungated with waitCode 0 + a COMPLETED result.json, so
// the resumed run completes without a sleep; every wait is a channel select
// or require.Eventually.
func TestShutdownResume_FullCycleResumesToCompleted(t *testing.T) {
	// Shared persistence — survives the simulated daemon restart.
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	tr.AddTask(&persistence.Task{
		ID:          "t1",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	})

	// ---- Phase 1: in-flight execution, real graceful Shutdown → PAUSED. ----
	rt1 := NewMockRuntime()
	gate := make(chan struct{})
	entered := make(chan struct{})
	rt1.waitGate = gate
	rt1.waitEntered = entered
	rt1.waitErr = errors.New("podman wait failed: container stopped")
	// StopContainer (called by pauseWithReason, AFTER shuttingDown is set)
	// releases the gate, so the run goroutine deterministically takes the
	// shuttingDown bail-out arm rather than racing a manual close.
	rt1.stopReleasesWaitGate = true

	e1 := NewWithOptions(rt1, er, ar, tr, nil)
	e1.config.RetryDelay = 0
	e1.SetWorkflowResolver(sharedWorkflowResolver())

	require.NoError(t, e1.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached WaitForExit — agent step not in flight")
	}
	require.True(t, e1.IsExecuting("t1"), "precondition: execution must be active before Shutdown")

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- e1.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownErr:
		require.NoError(t, err, "graceful shutdown must drain within budget")
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return — drain stalled")
	}

	// The drain left the execution PAUSED with reason=shutdown — the
	// checkpoint Recover() keys on. (Asserted as the precondition for
	// phase 2 rather than re-proving the whole drain contract, which is
	// TestShutdown_DrainsInFlightExecutionToPausedForResume's job.)
	pausedExec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, pausedExec)
	require.Equal(t, persistence.ExecutionStatusPaused, pausedExec.Status,
		"precondition: in-flight execution must be PAUSED after graceful shutdown")
	require.Equal(t, PauseReasonShutdown, loadExecutionState(pausedExec).PausedReason,
		"precondition: pause reason must be 'shutdown' so Recover() auto-resumes")

	// ---- Phase 2: fresh executor over the SAME repos → Recover() → COMPLETED. ----
	// A SEPARATE runtime so StartCalls() counts ONLY resume-side starts.
	// Ungated, exit 0, COMPLETED result.json → the resumed step runs to
	// completion without a sleep.
	rt2 := NewMockRuntime()
	rt2.waitCode = 0
	rt2.outputJSON = `{"status":"COMPLETED","message":"resumed and finished after restart"}`

	e2 := NewWithOptions(rt2, er, ar, tr, nil)
	e2.config.RetryDelay = 0
	e2.SetWorkflowResolver(sharedWorkflowResolver())

	require.NoError(t, e2.Recover(context.Background()))

	// The resumed run drives to terminal. Once it leaves activeExecutions
	// the cleanup defer (which runs AFTER the terminal persistence writes)
	// has completed, so every status read below is a happens-after barrier.
	require.Eventually(t, func() bool {
		return !e2.IsExecuting("t1")
	}, 5*time.Second, 10*time.Millisecond, "resumed execution must finish and drain")

	// Execution row COMPLETED with the resumed agent's result bytes.
	doneExec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, doneExec)
	assert.Equal(t, persistence.ExecutionStatusCompleted, doneExec.Status,
		"the shutdown-paused execution must resume and reach COMPLETED on the fresh executor")
	require.NotEmpty(t, doneExec.Result, "RecordCompletion must persist the resumed agent's result")

	// Task row COMPLETED.
	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status,
		"the task must finalize COMPLETED after the resumed run")

	// NO double-start: the resume started the step's container exactly once.
	// (rt1's single start is on the other runtime; rt2 sees only resume.)
	assert.Equal(t, 1, rt2.StartCalls(),
		"resume must start the step container exactly once — a second start is a double-spend")

	// NO double-lease / double-drive: the executor drained back to zero —
	// exactly one runExecution goroutine drove the task to terminal, none
	// lingering to re-drive it (the two-RUNNING-rows-for-one-task class).
	assert.Equal(t, 0, e2.ActiveCount(),
		"exactly one resume goroutine drove the task; none must linger re-driving it")
}

// TestShutdown_OnFailStepDoesNotMarkFailedOnShutdown — restart-induced
// in-flight FAILED, 2026-06-21. Signature B of the incident: a step with an
// on_fail target would route through on_fail even during shutdown, exhaust
// retries walking the failure graph, and land FAILED instead of PAUSED. The
// Fix-2 shutdown guard in the step-failure routing must short-circuit on_fail
// when the executor is shutting down and return the error upward so the
// existing shuttingDown bail-out arm handles it, leaving the task PAUSED.
//
// Seam: same as TestShutdown_DrainsInFlightExecutionToPausedForResume but the
// workflow has an on_fail transition ("implement" → "recover"). If the guard is
// absent the executor routes to "recover" and, finding it blocked by shutdown,
// eventually exhausts attempts and writes FAILED.
func TestShutdown_OnFailStepDoesNotMarkFailedOnShutdown(t *testing.T) {
	rt := NewMockRuntime()
	gate := make(chan struct{})
	entered := make(chan struct{})
	rt.waitGate = gate
	rt.waitEntered = entered
	rt.waitErr = errors.New("podman wait failed: container stopped")
	// StopContainer (called by pauseWithReason AFTER shuttingDown is set)
	// releases the gate — same causal order as the live incident: the run
	// goroutine observes shuttingDown and must bail before routing to on_fail.
	rt.stopReleasesWaitGate = true

	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	// Workflow with an on_fail path: "implement" --fail--> "recover" --fail--> "failed"
	// This is the exact shape that caused Signature B: on_fail was routed
	// during teardown, walking the failure graph to terminal FAILED.
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "implement",
				Steps: map[string]registry.WorkflowStep{
					"implement": {Type: "agent", Role: "worker", OnSuccess: "done", OnFail: "recover"},
					"recover":   {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED", Message: "Adaptive routing failed (last step: route)"},
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

	// Kick off the execution and wait until the implement step is in WaitForExit.
	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached WaitForExit — agent step not in flight")
	}
	require.True(t, e.IsExecuting("t1"), "precondition: execution must be active before Shutdown")

	// Run the real Shutdown — same path as the incident.
	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- e.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownErr:
		require.NoError(t, err, "graceful shutdown must drain within budget")
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return — drain stalled")
	}

	require.Eventually(t, func() bool {
		return !e.IsExecuting("t1")
	}, 2*time.Second, 10*time.Millisecond)

	// Task must land PAUSED — the shutdown guard must prevent the on_fail
	// routing from walking the failure graph during teardown, and the
	// existing shuttingDown bail-out arm in runExecution must write PAUSED
	// (not FAILED) so Recover() auto-resumes it on next start.
	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusPaused, task.Status,
		"task with on_fail target must be PAUSED (not FAILED) during shutdown (Signature B, 2026-06-21) — got %s", task.Status)

	// The execution row must also be PAUSED with reason=shutdown so
	// Recover() can distinguish this from an operator-paused execution.
	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status,
		"execution must be PAUSED after shutdown drain (on_fail path)")
	st := loadExecutionState(exec)
	assert.Equal(t, PauseReasonShutdown, st.PausedReason,
		"pause reason must be 'shutdown' so Recover() auto-resumes it")

	// The goroutine bailed via the shutdown arm, not the retry loop.
	assert.Equal(t, 1, rt.StartCalls(),
		"shutdown drain must not consume retry attempts — got %d starts", rt.StartCalls())
}

// blockingSystemHandler is a test-only SystemHandler that blocks on a gate
// channel before returning. Signals "entered" once the Execute body is
// reached so the test can synchronise Shutdown with the goroutine being
// genuinely in-flight inside the handler.
type blockingSystemHandler struct {
	name    string
	entered chan struct{}
	gate    chan struct{}
	retErr  error
}

func (h *blockingSystemHandler) Name() string { return h.name }
func (h *blockingSystemHandler) Execute(_ context.Context, _ SystemStepInput) (SystemStepResult, error) {
	if h.entered != nil {
		close(h.entered)
	}
	<-h.gate
	return SystemStepResult{}, h.retErr
}

// TestShutdown_SystemStepOnFailDoesNotMarkFailedOnShutdown — Blocker-1
// regression guard for the non-agent on_fail paths. The Fix-2
// shutdown guard must fire on ALL on_fail routing sites, not just the
// agent step. This test exercises the system-step on_fail path
// (workflow.go ~752/779): a system handler fails, step.OnFail is set,
// and the daemon shuts down before the routing decision is taken.
// Without the guard the executor routes to on_fail, which starts a
// fresh agent container against a closing socket, exhausts retry
// budget, and stamps FAILED instead of PAUSED.
//
// Seam: a blocking system handler plays the role the mock runtime's
// waitGate plays for agent steps — the test synchronises Shutdown with
// the goroutine genuinely blocked inside the handler, so shuttingDown
// is set before the handler error is processed and the on_fail guard
// fires.
func TestShutdown_SystemStepOnFailDoesNotMarkFailedOnShutdown(t *testing.T) {
	entered := make(chan struct{})
	gate := make(chan struct{})

	handler := &blockingSystemHandler{
		name:    "test.block",
		entered: entered,
		gate:    gate,
		retErr:  errors.New("system step failed: service unavailable"),
	}

	reg := NewSystemHandlerRegistry()
	reg.Register(handler)

	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0

	// Workflow: system step with on_fail → recover (agent), recover
	// succeeds → done. If the shutdown guard is absent the executor
	// routes "implement" → "recover" (an agent step), starts a
	// container, exhausts retries, stamps FAILED.
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
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

	// Kick off execution — goroutine blocks inside the system handler.
	require.NoError(t, e.Execute("t1"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached the system handler — step not in flight")
	}
	require.True(t, e.IsExecuting("t1"), "precondition: execution must be active before Shutdown")

	// Trigger shutdown BEFORE releasing the handler so shuttingDown is
	// set when the guard runs.
	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- e.Shutdown(ctx)
	}()

	// Give Shutdown time to set shuttingDown and call pauseWithReason.
	// The system step has no container, so pauseWithReason returns
	// immediately after writing PAUSED to the DB. We then release the
	// gate so the goroutine can observe shuttingDown and bail.
	require.Eventually(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.shuttingDown
	}, time.Second, 5*time.Millisecond, "shuttingDown must be set before we release the gate")

	// Release the handler so the goroutine sees the error + on_fail check.
	close(gate)

	select {
	case err := <-shutdownErr:
		require.NoError(t, err, "graceful shutdown must drain within budget")
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return — drain stalled")
	}

	require.Eventually(t, func() bool {
		return !e.IsExecuting("t1")
	}, 2*time.Second, 10*time.Millisecond)

	// Task must be PAUSED — the shutdown guard must have short-circuited
	// the on_fail route to "recover" and returned the error upward for
	// the shuttingDown bail-out arm to handle cleanly.
	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusPaused, task.Status,
		"system step on_fail must not route during shutdown — task must be PAUSED, got %s", task.Status)

	// Execution row PAUSED with reason=shutdown.
	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusPaused, exec.Status,
		"execution must be PAUSED after shutdown (system step on_fail path)")
	st := loadExecutionState(exec)
	assert.Equal(t, PauseReasonShutdown, st.PausedReason,
		"pause reason must be 'shutdown' so Recover() auto-resumes it")

	// No container was started — the system step has no runtime, and the
	// guard must have prevented routing to the "recover" agent step.
	assert.Equal(t, 0, rt.StartCalls(),
		"shutdown guard must prevent routing to on_fail agent step — got %d container starts", rt.StartCalls())
}

// TestShutdown_RejectsNewExecutions — the shuttingDown flag prevents
// the scheduler from leasing additional work into a draining
// executor. Without this, a tight shutdown race could lease a fresh
// task that immediately gets aborted with no checkpoint.
func TestShutdown_RejectsNewExecutions(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "fresh",
		ProjectID: "p1",
		Status:    persistence.TaskStatusQueued,
		CreatedAt: time.Now(),
	})

	// Set the shuttingDown flag without going through full Shutdown()
	// (which would Stop and wait, neither needed for this assertion).
	e.mu.Lock()
	e.shuttingDown = true
	e.mu.Unlock()

	err := e.ExecuteWithContext(context.Background(), "fresh")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutting down",
		"new executions must be rejected with a clear shutdown signal")
	assert.False(t, e.IsExecuting("fresh"))
}
