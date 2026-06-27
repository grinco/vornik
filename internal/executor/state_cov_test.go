package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestStateCov_ResumeTaskWrapsResumeError — ResumeTask is the
// error-only wrapper the UI's ExecutorInterface needs. When the
// underlying Resume fails (here: task already being executed) the
// wrapper must surface that error rather than swallowing it.
func TestStateCov_ResumeTaskWrapsResumeError(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1"}
	e.mu.Unlock()

	err := e.ResumeTask("t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already being executed")
}

// TestStateCov_ResumeTaskHappyPathReturnsNil — the wrapper drops the
// ResumeStatus payload and returns nil on success. Mirror of the
// Resume happy-path setup (cancelled ctx so the spawned goroutine
// exits immediately).
func TestStateCov_ResumeTaskHappyPathReturnsNil(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusPaused,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.mu.Unlock()

	assert.NoError(t, e.ResumeTask("t1"))
}

// TestStateCov_ResumeTaskGetError — when the task lookup fails, Resume
// returns "failed to get task" and the wrapper forwards it.
func TestStateCov_ResumeTaskGetError(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.err = errors.New("db down")
	err := e.ResumeTask("t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get task")
}

// TestStateCov_ResumeGetByTaskIDError — when no execution row exists
// for the task, GetByTaskID errors with "not found" and Resume
// surfaces "failed to get execution record".
func TestStateCov_ResumeGetByTaskIDError(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_, err := e.Resume("t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get execution record")
}

// TestStateCov_ResumeApprovalPendingBranch — when the paused snapshot
// carries an ApprovalPendingStep, Resume promotes it to
// ApprovalGrantedStep and clears the pending field + pause reason
// before flipping to Running. Drives the approval branch (the most
// specific of the resume-state arms).
func TestStateCov_ResumeApprovalPendingBranch(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"approvalPendingStep":"deploy","pausedReason":"operator"}`),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.mu.Unlock()

	status, err := e.Resume("t1")
	require.NoError(t, err)
	require.NotNil(t, status)

	// Read back through the repo's locked accessor (returns a copy) so
	// the assertion doesn't race the spawned runExecution goroutine.
	got, _ := er.Get(context.Background(), "e1")
	st := loadExecutionState(got)
	assert.Equal(t, "deploy", st.ApprovalGrantedStep, "pending approval must be promoted to granted")
	assert.Empty(t, st.ApprovalPendingStep, "pending approval step must be cleared")
	assert.Empty(t, st.PausedReason, "pause reason must be cleared on approval resume")
}

// TestStateCov_ResumeOperatorPausedClearsReason — an operator-paused
// execution (no approval pending) clears its PausedReason on resume so
// a later Recover() doesn't treat it as still operator-parked.
func TestStateCov_ResumeOperatorPausedClearsReason(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"pausedReason":"operator"}`),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.mu.Unlock()

	status, err := e.Resume("t1")
	require.NoError(t, err)
	require.NotNil(t, status)

	got, _ := er.Get(context.Background(), "e1")
	st := loadExecutionState(got)
	assert.Empty(t, st.PausedReason, "operator pause reason must be cleared on resume")
}

// TestStateCov_PauseGetExecutionError — when the execution record
// can't be loaded (GetByTaskID errors), pauseWithReason surfaces
// "failed to get execution record" rather than continuing into the
// status writes. containerID is empty so the stop/wait block is
// skipped and the test doesn't need a cleanup goroutine.
func TestStateCov_PauseGetExecutionError(t *testing.T) {
	e, _, _, _, _ := setup()
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	// No exec record for "t1" → GetByTaskID returns "not found".
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", cancel: cancel}
	e.mu.Unlock()

	_, err := e.Pause("t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get execution record")
}

// TestStateCov_PauseWaitForExitError — when the container is stopped
// but WaitForExit returns an error (didn't drain within the budget),
// pauseWithReason logs the orphan-window warning and proceeds to a
// successful pause. Drives the WaitForExit error branch in the
// container-stop block.
func TestStateCov_PauseWaitForExitError(t *testing.T) {
	e, rt, er, _, tr := setup()
	rt.waitErr = errors.New("did not exit within 30s")

	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning,
	})

	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", containerID: "c1", cancel: cancel}
	e.mu.Unlock()

	// pauseWithReason blocks on waitForExecutionCleanup after cancel();
	// simulate the runExecution defer clearing the entry shortly after.
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.mu.Lock()
		delete(e.activeExecutions, "t1")
		e.mu.Unlock()
	}()

	status, err := e.Pause("t1")
	require.NoError(t, err, "a WaitForExit error must be logged, not fatal")
	require.NotNil(t, status)
	assert.Equal(t, persistence.ExecutionStatusPaused, er.snapshotStatus("e1"))
}

// TestStateCov_CascadeOrphanExecutionsGuards — the cascade sweep is a
// no-op when execRepo is nil or the task ID is empty (the two
// short-circuit guards). No panic, no repo touch.
func TestStateCov_CascadeOrphanExecutionsGuards(t *testing.T) {
	// nil execRepo
	e1 := &Executor{}
	e1.cascadeOrphanExecutions(context.Background(), "t1")

	// empty task ID
	e2, _, _, _, _ := setup()
	e2.cascadeOrphanExecutions(context.Background(), "")
}

// TestStateCov_CascadeOrphanExecutionsSweeps — when non-terminal
// executions exist for the task, the sweep supersedes them (n>0 log
// branch). A terminal execution is left alone.
func TestStateCov_CascadeOrphanExecutionsSweeps(t *testing.T) {
	e, _, er, _, _ := setup()
	_ = er.Create(context.Background(), &persistence.Execution{
		ID: "e-live", TaskID: "t1", Status: persistence.ExecutionStatusRunning,
	})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID: "e-done", TaskID: "t1", Status: persistence.ExecutionStatusCompleted,
	})

	e.cascadeOrphanExecutions(context.Background(), "t1")

	assert.Equal(t, persistence.ExecutionStatusCancelled, er.snapshotStatus("e-live"),
		"non-terminal execution must be superseded")
	assert.Equal(t, persistence.ExecutionStatusCompleted, er.snapshotStatus("e-done"),
		"already-terminal execution must be left untouched")
}

// TestStateCov_CascadeOrphanExecutionsNoOrphans — when every execution
// for the task is already terminal, the sweep returns n==0 and skips
// the info log (the n==0 arm).
func TestStateCov_CascadeOrphanExecutionsNoOrphans(t *testing.T) {
	e, _, er, _, _ := setup()
	_ = er.Create(context.Background(), &persistence.Execution{
		ID: "e-done", TaskID: "t1", Status: persistence.ExecutionStatusCompleted,
	})
	e.cascadeOrphanExecutions(context.Background(), "t1")
	assert.Equal(t, persistence.ExecutionStatusCompleted, er.snapshotStatus("e-done"))
}

// stateCov_supersedeErrRepo wraps MockExecRepo and forces the
// SupersedeNonTerminalForTask sweep to error, so cascadeOrphanExecutions'
// warn-and-bail error arm is exercised.
type stateCov_supersedeErrRepo struct {
	*MockExecRepo
}

func (r *stateCov_supersedeErrRepo) SupersedeNonTerminalForTask(_ context.Context, _ string) (int64, error) {
	return 0, errors.New("supersede sweep failed")
}

// TestStateCov_CascadeOrphanExecutionsSweepError — a sweep error logs
// a warning and returns without panicking; orphan rows linger until
// manual cleanup (the documented best-effort contract).
func TestStateCov_CascadeOrphanExecutionsSweepError(t *testing.T) {
	e, _, _, _, _ := setup()
	e.execRepo = &stateCov_supersedeErrRepo{MockExecRepo: NewMockExecRepo()}
	e.cascadeOrphanExecutions(context.Background(), "t1") // must not panic
}
