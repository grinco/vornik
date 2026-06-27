package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// fastSettleExecutor returns from ExecuteWithContext immediately and
// reports IsExecuting=false on the next poll, letting tests drive
// dispatchViaExecutor through its post-execution status branches
// without time.Sleep.
type fastSettleExecutor struct {
	repo            *MockTaskRepository
	taskFinalStatus persistence.TaskStatus
	finalError      string // populates LastError when present
	executeErr      error
	cancelCalled    int
}

func (b *fastSettleExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	if b.executeErr != nil {
		return b.executeErr
	}
	if b.repo != nil {
		// Update both status and (when present) last_error so
		// dispatchViaExecutor's post-execution Get returns the
		// finished shape the test asserts on.
		b.repo.mu.Lock()
		if task, ok := b.repo.tasks[taskID]; ok {
			task.Status = b.taskFinalStatus
			if b.finalError != "" {
				msg := b.finalError
				task.LastError = &msg
			}
		}
		b.repo.mu.Unlock()
	}
	return nil
}

func (b *fastSettleExecutor) IsExecuting(taskID string) bool { return false }

func (b *fastSettleExecutor) Cancel(taskID string) error {
	b.cancelCalled++
	return nil
}

// TestDispatchViaExecutor_TerminatedOnCancelled — task was externally
// cancelled while the executor was running. The post-execution Get
// observes CANCELLED → outcome is dispatchTerminated and the lease
// release path keeps the CANCELLED status.
func TestDispatchViaExecutor_TerminatedOnCancelled(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &leaseID,
	}
	repo.AddTask(task)

	s := New(repo, &Config{LeaseDurationSeconds: 30})
	exec := &fastSettleExecutor{repo: repo, taskFinalStatus: persistence.TaskStatusCancelled}
	s.executor = exec

	outcome, errMsg := s.dispatchViaExecutor(context.Background(), task)
	assert.Equal(t, dispatchTerminated, outcome)
	assert.Empty(t, errMsg)
}

// TestDispatchViaExecutor_FailedWithExplicitError — task is FAILED
// with a non-empty LastError. dispatchFailed surfaces the LastError
// verbatim.
func TestDispatchViaExecutor_FailedWithExplicitError(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &leaseID,
	}
	repo.AddTask(task)

	s := New(repo, &Config{LeaseDurationSeconds: 30})
	exec := &fastSettleExecutor{
		repo:            repo,
		taskFinalStatus: persistence.TaskStatusFailed,
		finalError:      "agent OOM",
	}
	s.executor = exec

	outcome, errMsg := s.dispatchViaExecutor(context.Background(), task)
	assert.Equal(t, dispatchFailed, outcome)
	assert.Equal(t, "agent OOM", errMsg)
}

// TestDispatchViaExecutor_FailedWithoutLastError — task is FAILED but
// no LastError is set. dispatchFailed surfaces a default "task
// finished with status FAILED" message.
func TestDispatchViaExecutor_FailedWithoutLastError(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &leaseID,
	}
	repo.AddTask(task)

	s := New(repo, &Config{LeaseDurationSeconds: 30})
	exec := &fastSettleExecutor{repo: repo, taskFinalStatus: persistence.TaskStatusFailed}
	s.executor = exec

	outcome, errMsg := s.dispatchViaExecutor(context.Background(), task)
	assert.Equal(t, dispatchFailed, outcome)
	assert.Contains(t, errMsg, "FAILED")
}

// TestDispatchViaExecutor_NonTerminalStatusIsFailed — executor returned
// idle but the task is still RUNNING (e.g. step transition not yet
// finalised). dispatchFailed surfaces a non-terminal-status message.
func TestDispatchViaExecutor_NonTerminalStatusIsFailed(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &leaseID,
	}
	repo.AddTask(task)

	s := New(repo, &Config{LeaseDurationSeconds: 30})
	// taskFinalStatus stays Leased — dispatchViaExecutor sees a non-terminal status.
	exec := &fastSettleExecutor{repo: repo, taskFinalStatus: persistence.TaskStatusRunning}
	s.executor = exec

	outcome, errMsg := s.dispatchViaExecutor(context.Background(), task)
	assert.Equal(t, dispatchFailed, outcome)
	assert.Contains(t, errMsg, "non-terminal")
}

// TestDispatchViaExecutor_ExecuteWithContextErrorReturnsFailed — the
// upfront call to executor.ExecuteWithContext returns an error → the
// outcome is dispatchFailed with the error's Error() string.
func TestDispatchViaExecutor_ExecuteWithContextErrorReturnsFailed(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &leaseID,
	}
	repo.AddTask(task)

	s := New(repo, &Config{LeaseDurationSeconds: 30})
	exec := &fastSettleExecutor{repo: repo, executeErr: errors.New("startup failure")}
	s.executor = exec

	outcome, errMsg := s.dispatchViaExecutor(context.Background(), task)
	assert.Equal(t, dispatchFailed, outcome)
	assert.Equal(t, "startup failure", errMsg)
}

// TestCompleteTask_SuccessReleasesLeaseAsCompleted pins the simple
// success path: status COMPLETED, no retry math, decrementRunning
// fires.
func TestCompleteTask_SuccessReleasesLeaseAsCompleted(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{ID: "t1", ProjectID: "p", Status: persistence.TaskStatusLeased, LeaseID: &leaseID}
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.completeTask(task, true, "")
	assert.NoError(t, err)
	assert.Equal(t, 0, s.runningCount)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Greater(t, repo.releaseLeaseCalls, 0)
}

// TestCompleteTask_FailureRetriesWhenAttemptsRemain — task with
// attempt < max_attempts re-queues for retry on failure.
func TestCompleteTask_FailureRetriesWhenAttemptsRemain(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:          "t1",
		ProjectID:   "p",
		Status:      persistence.TaskStatusLeased,
		LeaseID:     &leaseID,
		Attempt:     1,
		MaxAttempts: 3,
	}
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.completeTask(task, false, "transient db failure")
	assert.NoError(t, err)
	// Status reset to QUEUED for retry (mock applies it via ReleaseLease).
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Greater(t, repo.releaseLeaseCalls, 0)
	updated := repo.tasks["t1"]
	require.NotNil(t, updated)
	assert.Equal(t, persistence.TaskStatusQueued, updated.Status)
	require.NotNil(t, updated.LastError)
	assert.Equal(t, "transient db failure", *updated.LastError)
}

// TestCompleteTask_FailureFinalWhenAttemptsExhausted — attempt ==
// max_attempts → status FAILED, no further retry.
func TestCompleteTask_FailureFinalWhenAttemptsExhausted(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:          "t1",
		ProjectID:   "p",
		Status:      persistence.TaskStatusLeased,
		LeaseID:     &leaseID,
		Attempt:     3,
		MaxAttempts: 3,
	}
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1

	err := s.completeTask(task, false, "permanent failure")
	assert.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	updated := repo.tasks["t1"]
	require.NotNil(t, updated)
	assert.Equal(t, persistence.TaskStatusFailed, updated.Status)
}

// TestCompleteTask_NormalizesAttemptZeroToOne — a task with attempt=0
// (legacy / first creation) is treated as attempt=1, then increments to 2.
func TestCompleteTask_NormalizesAttemptZeroToOne(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:          "t1",
		ProjectID:   "p",
		Status:      persistence.TaskStatusLeased,
		LeaseID:     &leaseID,
		Attempt:     0,
		MaxAttempts: 3,
	}
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})

	err := s.completeTask(task, false, "boom")
	assert.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	// attempt was 0, normalized to 1, then bumped to 2 for the retry.
	updated := repo.tasks["t1"]
	require.NotNil(t, updated)
	assert.Equal(t, 2, updated.Attempt)
}

// TestIsTerminalTaskStatus covers every branch.
func TestIsTerminalTaskStatusBranches(t *testing.T) {
	terminal := []persistence.TaskStatus{
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	}
	for _, s := range terminal {
		assert.True(t, isTerminalTaskStatus(s), "%s should be terminal", s)
	}
	nonTerminal := []persistence.TaskStatus{
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusPaused,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
	}
	for _, s := range nonTerminal {
		assert.False(t, isTerminalTaskStatus(s), "%s should be non-terminal", s)
	}
}

// TestDispatchTask_FallbackWithoutExecutor — when s.executor is nil,
// dispatchTask falls back to completeTask(success=true) — exercising
// the fallback branch that's otherwise hard to reach.
func TestDispatchTask_FallbackWithoutExecutor(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		LeaseID:   &leaseID,
		Attempt:   1,
	}
	repo.AddTask(task)
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	// Increment running so completeTask's decrementRunning doesn't underflow.
	s.runningCount = 1
	// dispatchTask runs dispatchWg.Done, so add the counter the caller
	// usually adds before spawning the goroutine.
	s.dispatchWg.Add(1)
	s.dispatchTask(task)
	// Wait for the goroutine (we called dispatchTask synchronously here)
	// to drain — it ran on this stack so dispatchWg should be back to 0.
	done := make(chan struct{})
	go func() { s.dispatchWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatchWg did not drain")
	}
	// completeTask called ReleaseLease → mock recorded the release.
	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Greater(t, repo.releaseLeaseCalls, 0)
}
