package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// w2ExecRepo is a minimal ExecutionRepository double; only GetByTaskID is
// exercised by the paused-execution branches.
type w2ExecRepo struct {
	exec *persistence.Execution
}

func newW2ExecRepo() *w2ExecRepo { return &w2ExecRepo{} }

func (r *w2ExecRepo) Create(ctx context.Context, e *persistence.Execution) error { return nil }
func (r *w2ExecRepo) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	if r.exec == nil {
		return nil, persistence.ErrNotFound
	}
	return r.exec, nil
}
func (r *w2ExecRepo) UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error {
	return nil
}
func (r *w2ExecRepo) RecordCompletion(ctx context.Context, id string, result []byte) error {
	return nil
}
func (r *w2ExecRepo) RecordFailure(ctx context.Context, id, msg, code string) error { return nil }

// w2OptsCapturingRepo wraps MockTaskRepository to record the last
// LeaseOptions threaded into LeaseTask — used to assert archived-project
// exclusion + priority defaults are wired through.
type w2OptsCapturingRepo struct {
	*MockTaskRepository
	lastOpts *persistence.LeaseOptions
}

func newW2OptsCapturingRepo() *w2OptsCapturingRepo {
	return &w2OptsCapturingRepo{MockTaskRepository: NewMockTaskRepository()}
}

func (r *w2OptsCapturingRepo) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	captured := opts
	r.lastOpts = &captured
	return r.MockTaskRepository.LeaseTask(ctx, opts)
}

// w2Registry is a ProjectRegistry double carrying limits + archived IDs.
type w2Registry struct {
	limits   map[string]int
	archived []string
}

func (r w2Registry) ProjectConcurrencyLimits() map[string]int { return r.limits }
func (r w2Registry) ProjectPriorities() map[string]int        { return nil }
func (r w2Registry) ArchivedProjectIDs() []string             { return r.archived }

// Wave-2 scheduler tests. New file (recovery sweep already covered in
// recovery_paths_test.go); these target the still-under-covered branches:
// TaskCompleted retry/DLQ split + lookup-failure, releaseExecutorLease's
// paused / terminated / interrupted dispatch outcomes, leaseTaskWithContext
// error + archived-project exclusion, the idle-observation memo, the jitter
// floor, and state-machine transition-matrix gaps. Prefix TestW2Sched.

// ---- small helpers / fixtures ---------------------------------------------

// w2Executor is a tunable executor double. busyTicks controls how many
// IsExecuting() polls report true before the executor "settles" idle; after
// that dispatchViaExecutor reloads the task and classifies the outcome.
type w2Executor struct {
	busyTicks    int
	calls        int
	cancelCalled int
	executeErr   error
	finalStatus  persistence.TaskStatus
	finalErr     string
	repo         *MockTaskRepository
	onExecute    func()
}

func (e *w2Executor) ExecuteWithContext(ctx context.Context, taskID string) error {
	if e.executeErr != nil {
		return e.executeErr
	}
	if e.onExecute != nil {
		e.onExecute()
	}
	if e.repo != nil && e.finalStatus != "" {
		e.repo.mu.Lock()
		if t, ok := e.repo.tasks[taskID]; ok {
			t.Status = e.finalStatus
			if e.finalErr != "" {
				msg := e.finalErr
				t.LastError = &msg
			}
		}
		e.repo.mu.Unlock()
	}
	return nil
}

func (e *w2Executor) IsExecuting(taskID string) bool {
	e.calls++
	return e.calls <= e.busyTicks
}

func (e *w2Executor) Cancel(taskID string) error {
	e.cancelCalled++
	return nil
}

func w2LeasedTask(id string) *persistence.Task {
	lease := "lease-" + id
	return &persistence.Task{
		ID:        id,
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &lease,
	}
}

// ---- TaskCompleted: success / retry / DLQ / lookup-failure ----------------

// TestW2SchedTaskCompletedSuccessReleasesCompleted pins TaskCompleted's
// success branch: COMPLETED status, no retry math, running count decremented.
func TestW2SchedTaskCompletedSuccessReleasesCompleted(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.TaskCompleted("t1", taskLeaseID(task), true, "")
	require.NoError(t, err)
	assert.Equal(t, 0, s.runningCount)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusCompleted, repo.tasks["t1"].Status)
}

// TestW2SchedTaskCompletedRetriesWhenAttemptsRemain — failure with
// attempt < max re-queues with the attempt incremented.
func TestW2SchedTaskCompletedRetriesWhenAttemptsRemain(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Attempt = 1
	task.MaxAttempts = 3
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.TaskCompleted("t1", taskLeaseID(task), false, "transient")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	updated := repo.tasks["t1"]
	assert.Equal(t, persistence.TaskStatusQueued, updated.Status)
	assert.Equal(t, 2, updated.Attempt)
	require.NotNil(t, updated.LastError)
	assert.Equal(t, "transient", *updated.LastError)
}

// TestW2SchedTaskCompletedDeadLettersAtMaxAttempts — failure with
// attempt == max lands FAILED (the DLQ split).
func TestW2SchedTaskCompletedDeadLettersAtMaxAttempts(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Attempt = 3
	task.MaxAttempts = 3
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.TaskCompleted("t1", taskLeaseID(task), false, "permanent")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusFailed, repo.tasks["t1"].Status)
}

// TestW2SchedTaskCompletedNormalizesZeroAttemptToOne — attempt=0,max=0 is
// normalized to attempt=1,max=1, so a single failure dead-letters
// immediately (no retry) rather than looping forever.
func TestW2SchedTaskCompletedNormalizesZeroAttemptToOne(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1") // Attempt=0, MaxAttempts=0
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.TaskCompleted("t1", taskLeaseID(task), false, "boom")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusFailed, repo.tasks["t1"].Status,
		"attempt==max==1 must dead-letter, not retry")
}

// TestW2SchedTaskCompletedFailsPermanentlyWhenLookupFails — the Get-error
// branch: if the task can't be reloaded, TaskCompleted can't run the retry
// math, so it fails permanently with the supplied error. Covers the
// 93.3%-only branch.
func TestW2SchedTaskCompletedFailsPermanentlyWhenLookupFails(t *testing.T) {
	repo := NewMockTaskRepository()
	// No task added → Get returns ErrNotFound. ReleaseLease would also
	// ErrNotFound, but the decision (fail permanently) is what we assert:
	// it must NOT attempt retry math and it returns the ReleaseLease error.
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.TaskCompleted("missing", "lease-x", false, "boom")
	// ReleaseLease on a missing task returns ErrNotFound — the point is the
	// code took the fail-permanently branch (one Get, one ReleaseLease).
	assert.ErrorIs(t, err, persistence.ErrNotFound)
	assert.Equal(t, 0, s.runningCount, "slot still freed even on lookup failure")
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, 1, repo.releaseLeaseCalls, "exactly one release, no retry re-release")
}

// TestW2SchedDecrementRunningClampsAtZero — decrementRunning must never
// underflow below zero even if called more times than increments.
func TestW2SchedDecrementRunningClampsAtZero(t *testing.T) {
	s := New(NewMockTaskRepository(), &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1
	s.decrementRunning()
	s.decrementRunning() // extra call
	assert.Equal(t, 0, s.RunningCount())
}

// ---- releaseExecutorLease: outcome → action matrix ------------------------

// TestW2SchedReleaseExecutorLeasePausedRequeuesPending — a paused dispatch
// reloads the task; a still-LEASED/RUNNING status is rewritten to PENDING
// before release (so it doesn't lease again while paused).
func TestW2SchedReleaseExecutorLeasePausedRequeuesPending(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Status = persistence.TaskStatusRunning
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchPaused, "")
	require.NoError(t, err)
	assert.Equal(t, 0, s.runningCount)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusPending, repo.tasks["t1"].Status)
}

// TestW2SchedReleaseExecutorLeasePausedKeepsNonRunningStatus — when the
// reloaded paused task is already at-rest (e.g. PAUSED), the release keeps
// that status rather than forcing PENDING.
func TestW2SchedReleaseExecutorLeasePausedKeepsNonRunningStatus(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Status = persistence.TaskStatusPaused
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchPaused, "")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusPaused, repo.tasks["t1"].Status)
}

// TestW2SchedReleaseExecutorLeaseTerminatedOnlyFreesSlot — dispatchTerminated
// (external cancel) must NOT touch the task row; it only frees the slot.
func TestW2SchedReleaseExecutorLeaseTerminatedOnlyFreesSlot(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Status = persistence.TaskStatusCancelled
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchTerminated, "")
	require.NoError(t, err)
	assert.Equal(t, 0, s.runningCount)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusCancelled, repo.tasks["t1"].Status)
	assert.Equal(t, 0, repo.releaseLeaseCalls, "terminated must not re-release")
}

// TestW2SchedReleaseExecutorLeaseInterruptedRequeues — shutdown interrupt on
// a non-terminal task re-queues it (without burning an attempt) so a restart
// picks it back up.
func TestW2SchedReleaseExecutorLeaseInterruptedRequeues(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Status = persistence.TaskStatusRunning
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchInterrupted, "shutdown")
	require.NoError(t, err)
	assert.Equal(t, 0, s.runningCount)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusQueued, repo.tasks["t1"].Status)
}

// TestW2SchedReleaseExecutorLeaseInterruptedSkipsTerminal — if the
// interrupted task already reached a terminal status, the release path must
// leave it alone (no requeue race that would resurrect a finished task).
func TestW2SchedReleaseExecutorLeaseInterruptedSkipsTerminal(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Status = persistence.TaskStatusCompleted
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchInterrupted, "shutdown")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusCompleted, repo.tasks["t1"].Status)
	assert.Equal(t, 0, repo.releaseLeaseCalls, "terminal interrupted task must not be re-released")
}

// TestW2SchedReleaseExecutorLeaseInterruptedSkipsPausedExecution — when the
// execution row is PAUSED, the interrupted release short-circuits (the pause
// path already owns the row) and does not requeue.
func TestW2SchedReleaseExecutorLeaseInterruptedSkipsPausedExecution(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Status = persistence.TaskStatusRunning
	repo.AddTask(task)
	execRepo := newW2ExecRepo()
	execRepo.exec = &persistence.Execution{TaskID: "t1", Status: persistence.ExecutionStatusPaused}
	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 30}, WithExecutionRepository(execRepo))
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchInterrupted, "shutdown")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusRunning, repo.tasks["t1"].Status,
		"paused execution short-circuit must not requeue")
	assert.Equal(t, 0, repo.releaseLeaseCalls)
}

// TestW2SchedReleaseExecutorLeaseDefaultFailsViaTaskCompleted — the default
// (dispatchFailed) outcome routes through TaskCompleted with success=false
// and a synthesized error when none is provided.
func TestW2SchedReleaseExecutorLeaseDefaultFailsViaTaskCompleted(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	task.Attempt = 1
	task.MaxAttempts = 1 // exhausted → FAILED
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchFailed, "")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	updated := repo.tasks["t1"]
	assert.Equal(t, persistence.TaskStatusFailed, updated.Status)
	require.NotNil(t, updated.LastError)
	assert.Equal(t, "executor reported task failure", *updated.LastError,
		"empty errorMsg + FAILED status synthesizes the default reason")
}

// TestW2SchedReleaseExecutorLeaseSuccessCompletes — dispatchSucceeded routes
// through TaskCompleted(success=true) → COMPLETED.
func TestW2SchedReleaseExecutorLeaseSuccessCompletes(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.releaseExecutorLease(task, dispatchSucceeded, "")
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusCompleted, repo.tasks["t1"].Status)
}

// ---- dispatch / lease acquisition -----------------------------------------

// TestW2SchedDispatchViaExecutorInterruptedOnCtxCancel — a cancelled context
// while the executor is still busy yields dispatchInterrupted (requeue without
// burning an attempt), not dispatchFailed.
func TestW2SchedDispatchViaExecutorInterruptedOnCtxCancel(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	// busyTicks high so IsExecuting stays true until ctx cancels.
	s.executor = &w2Executor{repo: repo, busyTicks: 1000}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	outcome, msg := s.dispatchViaExecutor(ctx, task)
	assert.Equal(t, dispatchInterrupted, outcome)
	assert.Contains(t, msg, "context canceled")
}

// TestW2SchedDispatchViaExecutorPausedExecution — executor settles idle, the
// task is left non-terminal, but the execution row is PAUSED → dispatchPaused.
func TestW2SchedDispatchViaExecutorPausedExecution(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("t1")
	repo.AddTask(task)
	execRepo := newW2ExecRepo()
	execRepo.exec = &persistence.Execution{TaskID: "t1", Status: persistence.ExecutionStatusPaused}
	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 30}, WithExecutionRepository(execRepo))
	// Leave status RUNNING (non-terminal) after execute, idle immediately.
	s.executor = &w2Executor{repo: repo, busyTicks: 0, finalStatus: persistence.TaskStatusRunning}

	outcome, _ := s.dispatchViaExecutor(context.Background(), task)
	assert.Equal(t, dispatchPaused, outcome)
}

// TestW2SchedLeaseTaskWithContextPropagatesRepoError — a repo LeaseTask error
// (other than ErrNoTasksAvailable) surfaces to the caller unchanged.
func TestW2SchedLeaseTaskWithContextPropagatesRepoError(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.leaseTaskErr = errors.New("db down")
	s := New(repo, &Config{LeaseDurationSeconds: 30})

	task, err := s.leaseTaskWithContext(context.Background())
	assert.Nil(t, task)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

// TestW2SchedLeaseTaskWithContextNilContextSafe — a nil context must not
// panic; it falls back to context.Background().
func TestW2SchedLeaseTaskWithContextNilContextSafe(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddTask(&persistence.Task{
		ID: "t1", ProjectID: "p", Status: persistence.TaskStatusQueued, CreatedAt: time.Now(),
	})
	s := New(repo, &Config{LeaseDurationSeconds: 30})

	//nolint:staticcheck // intentionally passing nil to exercise the guard
	task, err := s.leaseTaskWithContext(nil)
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, "t1", task.ID)
}

// TestW2SchedLeaseExcludesArchivedProjects — leaseTaskWithContext threads the
// registry's ArchivedProjectIDs into LeaseOptions.ExcludedProjects. We assert
// the option is wired (the matrix the lease query depends on).
func TestW2SchedLeaseExcludesArchivedProjects(t *testing.T) {
	repo := newW2OptsCapturingRepo()
	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 30},
		WithProjectRegistry(w2Registry{
			limits:   map[string]int{"p": 2},
			archived: []string{"archived-proj"},
		}))
	// No queued tasks → ErrNoTasksAvailable, but the opts are captured first.
	_, _ = s.leaseTaskWithContext(context.Background())

	require.NotNil(t, repo.lastOpts)
	assert.Equal(t, []string{"archived-proj"}, repo.lastOpts.ExcludedProjects)
	assert.Equal(t, 50, repo.lastOpts.ProjectPriorityDefault,
		"unconfigured projects default to priority 50")
	assert.Equal(t, map[string]int{"p": 2}, repo.lastOpts.ProjectConcurrencyLimits)
}

// ---- recovery idle memo + jitter floor ------------------------------------

// TestW2SchedRecordRecoveryIdleSinceReturnsEarliest — the second call for the
// same task returns the FIRST observation, not `now` — so the grace window is
// measured from when idleness began.
func TestW2SchedRecordRecoveryIdleSinceReturnsEarliest(t *testing.T) {
	s := New(NewMockTaskRepository(), &Config{LeaseDurationSeconds: 30})
	first := time.Now()
	got1 := s.recordRecoveryIdleSince("t1", first)
	assert.Equal(t, first, got1)

	later := first.Add(10 * time.Second)
	got2 := s.recordRecoveryIdleSince("t1", later)
	assert.Equal(t, first, got2, "must return the earliest observation, not the later one")
}

// TestW2SchedClearRecoveryIdleSinceResetsMemo — clearing then re-recording
// stamps a fresh observation (the transient-false reset path).
func TestW2SchedClearRecoveryIdleSinceResetsMemo(t *testing.T) {
	s := New(NewMockTaskRepository(), &Config{LeaseDurationSeconds: 30})
	first := time.Now()
	s.recordRecoveryIdleSince("t1", first)
	s.clearRecoveryIdleSince("t1")

	later := first.Add(time.Minute)
	got := s.recordRecoveryIdleSince("t1", later)
	assert.Equal(t, later, got, "after clear, the next observation becomes the new earliest")
}

// TestW2SchedComputeJitteredRenewIntervalZeroBaseFloors — a zero/negative
// lease duration floors to 100ms rather than producing a zero interval that
// would busy-spin the renewal loop.
func TestW2SchedComputeJitteredRenewIntervalZeroBaseFloors(t *testing.T) {
	assert.Equal(t, 100*time.Millisecond, computeJitteredRenewInterval(0))
	assert.Equal(t, 100*time.Millisecond, computeJitteredRenewInterval(-5))
}

// TestW2SchedComputeJitteredRenewIntervalTinyConfigClampsToFloor — a 1s lease
// (base=500ms) can jitter down toward the 100ms floor; across many draws the
// minimum is never below 100ms.
func TestW2SchedComputeJitteredRenewIntervalTinyConfigClampsToFloor(t *testing.T) {
	for i := 0; i < 500; i++ {
		got := computeJitteredRenewInterval(1)
		assert.GreaterOrEqual(t, got, 100*time.Millisecond)
	}
}

// TestW2SchedErrorMsgOrStatus covers all three branches of the helper.
func TestW2SchedErrorMsgOrStatus(t *testing.T) {
	assert.Equal(t, "explicit", errorMsgOrStatus("explicit", persistence.TaskStatusFailed))
	assert.Equal(t, "executor reported task failure",
		errorMsgOrStatus("", persistence.TaskStatusFailed))
	assert.Equal(t, "", errorMsgOrStatus("", persistence.TaskStatusQueued))
}

// TestW2SchedLeaseClearedByTransition — true when the reloaded task is out of
// RUNNING/LEASED (deliberate clear), false when still RUNNING/LEASED or the
// reload fails (treat as a genuine lost lease).
func TestW2SchedLeaseClearedByTransition(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddTask(&persistence.Task{ID: "awaiting", ProjectID: "p", Status: persistence.TaskStatusAwaitingInput})
	repo.AddTask(&persistence.Task{ID: "still-running", ProjectID: "p", Status: persistence.TaskStatusRunning})
	s := New(repo, &Config{LeaseDurationSeconds: 30})

	assert.True(t, s.leaseClearedByTransition(context.Background(), "awaiting"),
		"AWAITING_INPUT means lease was cleared by a benign transition")
	assert.False(t, s.leaseClearedByTransition(context.Background(), "still-running"),
		"RUNNING means the lease is still meant to be held")
	assert.False(t, s.leaseClearedByTransition(context.Background(), "missing"),
		"reload failure must be treated as a real lost lease, not a benign clear")
}

// ---- ExternalWaitMonitor.SetLeaderGate ------------------------------------

type w2LeaderGate struct{ leader bool }

func (g w2LeaderGate) IsLeader() bool { return g.leader }

// TestW2SchedSetLeaderGateAttaches — SetLeaderGate stores the gate (0%
// before) and is a no-op on a nil monitor.
func TestW2SchedSetLeaderGateAttaches(t *testing.T) {
	m := NewExternalWaitMonitor(nil, nil, nil, nil, 0, zerolog.Nop())
	m.SetLeaderGate(w2LeaderGate{leader: true})
	require.NotNil(t, m.leaderGate)
	assert.True(t, m.leaderGate.IsLeader())

	var nilMonitor *ExternalWaitMonitor
	assert.NotPanics(t, func() { nilMonitor.SetLeaderGate(w2LeaderGate{}) })
}

// ---- state-machine transition-matrix gaps ---------------------------------

// TestW2SchedStateMachineExternalEventRequeues — AWAITING_EXTERNAL → QUEUED
// is valid under both external-signal triggers; from any other origin it is
// rejected (the AWAITING_EXTERNAL re-queue contract).
func TestW2SchedStateMachineExternalEventRequeues(t *testing.T) {
	for _, trig := range []TransitionTrigger{TriggerExternalEvent, TriggerExternalDeadline} {
		assert.NoError(t, ValidateTransition(
			persistence.TaskStatusAwaitingExternal, persistence.TaskStatusQueued, trig), string(trig))
		assert.Error(t, ValidateTransition(
			persistence.TaskStatusRunning, persistence.TaskStatusQueued, trig),
			"%s only from AWAITING_EXTERNAL", trig)
		assert.Error(t, ValidateTransition(
			persistence.TaskStatusAwaitingExternal, persistence.TaskStatusRunning, trig),
			"%s must land QUEUED", trig)
	}
}

// TestW2SchedStateMachineExternalWaitOnlyFromRunning — RUNNING →
// AWAITING_EXTERNAL is the only valid external_wait transition.
func TestW2SchedStateMachineExternalWaitOnlyFromRunning(t *testing.T) {
	assert.NoError(t, ValidateTransition(
		persistence.TaskStatusRunning, persistence.TaskStatusAwaitingExternal, TriggerExternalWait))
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusQueued, persistence.TaskStatusAwaitingExternal, TriggerExternalWait))
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusRunning, persistence.TaskStatusAwaitingInput, TriggerExternalWait))
}

// TestW2SchedStateMachineEnqueueOriginMatrix — TriggerEnqueue accepts the
// documented re-queueable origins and rejects archived/terminal/leasy ones.
func TestW2SchedStateMachineEnqueueOriginMatrix(t *testing.T) {
	allowed := []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
		persistence.TaskStatusPaused,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
	}
	for _, from := range allowed {
		assert.NoError(t, ValidateTransition(from, persistence.TaskStatusQueued, TriggerEnqueue),
			"enqueue from %s should be allowed", from)
	}
	// AWAITING_APPROVAL is NOT an enqueue origin (it needs approve/reject).
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusAwaitingApproval, persistence.TaskStatusQueued, TriggerEnqueue))
	// Enqueue must always land QUEUED.
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusPending, persistence.TaskStatusRunning, TriggerEnqueue))
	// FAILED is terminal — caught by the absorbing-state guard.
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusFailed, persistence.TaskStatusQueued, TriggerEnqueue))
}

// TestW2SchedStateMachineFailedMayCloseButNotReopen — the one terminal→terminal
// exception (FAILED → CLOSED via operator close) is allowed; FAILED → anything
// else stays rejected.
func TestW2SchedStateMachineFailedMayCloseButNotReopen(t *testing.T) {
	assert.NoError(t, ValidateTransition(
		persistence.TaskStatusFailed, persistence.TaskStatusClosed, TriggerOperatorClose))
	// CANCELLED stays fully absorbing — no close.
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed, TriggerOperatorClose))
	// FAILED cannot be cancelled/queued.
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusFailed, persistence.TaskStatusCancelled, TriggerOperatorCancel))
}

// TestW2SchedStateMachinePauseFromAwaiting — an awaiting task can be paused
// (covers the IsAwaitingInput arm of the pause guard), but a terminal task
// cannot.
func TestW2SchedStateMachinePauseFromAwaiting(t *testing.T) {
	assert.NoError(t, ValidateTransition(
		persistence.TaskStatusAwaitingExternal, persistence.TaskStatusPaused, TriggerOperatorPause))
	assert.NoError(t, ValidateTransition(
		persistence.TaskStatusRunning, persistence.TaskStatusPaused, TriggerOperatorPause))
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusCompleted, persistence.TaskStatusPaused, TriggerOperatorPause),
		"COMPLETED is neither active nor awaiting → not pausable")
	// Pause must land PAUSED.
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerOperatorPause))
}

// TestW2SchedStateMachineChildrenDoneLandsTerminalOnly — WAITING_FOR_CHILDREN
// may complete or fail, but not requeue.
func TestW2SchedStateMachineChildrenDoneLandsTerminalOnly(t *testing.T) {
	assert.NoError(t, ValidateTransition(
		persistence.TaskStatusWaitingForChildren, persistence.TaskStatusCompleted, TriggerChildrenDone))
	assert.NoError(t, ValidateTransition(
		persistence.TaskStatusWaitingForChildren, persistence.TaskStatusFailed, TriggerChildrenDone))
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusWaitingForChildren, persistence.TaskStatusQueued, TriggerChildrenDone))
	assert.Error(t, ValidateTransition(
		persistence.TaskStatusRunning, persistence.TaskStatusCompleted, TriggerChildrenDone),
		"children_done only from WAITING_FOR_CHILDREN")
}
