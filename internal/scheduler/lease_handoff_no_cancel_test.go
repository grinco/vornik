package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
)

// handoffDuringExecExecutor simulates the real race that produced the
// spurious-cancel bug: while the dispatch-watch goroutine is still
// polling IsExecuting (the executor goroutine is finalizing a lead
// hand-off), the task is flipped RUNNING → AWAITING_INPUT and its lease
// cleared. The next RenewLease then fails with ErrLeaseNotFound. Before
// the fix the watch loop escalated that to executor.Cancel, clobbering
// AWAITING_INPUT with CANCELLED.
type handoffDuringExecExecutor struct {
	repo     *MockTaskRepository
	maxPolls int // report IsExecuting=true for this many polls, then false

	mu           sync.Mutex
	polls        int
	cancelCalled int
}

func (e *handoffDuringExecExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	return nil
}

func (e *handoffDuringExecExecutor) IsExecuting(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.polls++
	if e.polls == 1 {
		// First observation: the lead emitted a checkpoint. Mirror
		// handleCheckpointOutcome's RUNNING→AWAITING_INPUT + ClearLease.
		e.repo.mu.Lock()
		if t, ok := e.repo.tasks[taskID]; ok {
			t.Status = persistence.TaskStatusAwaitingInput
			t.LeaseID = nil
		}
		e.repo.mu.Unlock()
	}
	return e.polls <= e.maxPolls
}

func (e *handoffDuringExecExecutor) Cancel(taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelCalled++
	return nil
}

// TestDispatchViaExecutor_NoCancelWhenLeaseClearedByHandoff is the
// regression guard for T-…2d9fe309771eb138 (2026-06-03): a lead
// checkpoint flipped the task to AWAITING_INPUT and cleared the lease;
// the still-spinning watch loop's renewals failed with ErrLeaseNotFound
// and the loop escalated to executor.Cancel — cancelling the task the
// operator was about to answer. The fix recognises the deliberate
// transition out of RUNNING/LEASED and stops renewing without cancel.
func TestDispatchViaExecutor_NoCancelWhenLeaseClearedByHandoff(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	// Store the DB-side row; the executor will clear its lease to mimic
	// the hand-off. The scheduler's dispatch-watch loop, in production,
	// holds a SEPARATE leased copy returned by LeaseTask — so it keeps
	// renewing against its own leaseID even after the DB row's lease is
	// gone. Pass a distinct pointer to reproduce that.
	repo.AddTask(&persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusRunning,
		LeaseID:   &leaseID,
	})
	leasedCopy := "lease-1"
	task := &persistence.Task{
		ID:        "task-1",
		ProjectID: "p",
		Status:    persistence.TaskStatusRunning,
		LeaseID:   &leasedCopy,
	}

	// LeaseDurationSeconds=1 → renewInterval ~0.5s. The watch loop polls
	// every 100ms; maxPolls=32 keeps it alive ~3.2s — long enough that
	// the pre-fix code would have hit its 3-failure escalation
	// (~2.5s, failures ~1s apart) and called executor.Cancel. The fix
	// stops renewing after the first ErrLeaseNotFound, so it never
	// escalates.
	s := New(repo, &Config{LeaseDurationSeconds: 1})
	exec := &handoffDuringExecExecutor{repo: repo, maxPolls: 32}
	s.executor = exec

	outcome, _ := s.dispatchViaExecutor(context.Background(), task)

	// The benign-clear path must never cancel.
	assert.Equal(t, 0, exec.cancelCalled, "executor.Cancel must not fire on a deliberate lease-clear")
	// A renewal was actually attempted against the cleared lease
	// (otherwise the test isn't exercising the branch).
	assert.GreaterOrEqual(t, repo.renewLeaseCalls, 1, "expected at least one renewal attempt against the cleared lease")
	// The task keeps the status the hand-off stamped — not CANCELLED.
	cur, err := repo.Get(context.Background(), task.ID)
	assert.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusAwaitingInput, cur.Status)
	// dispatchViaExecutor itself doesn't re-classify a non-terminal
	// at-rest status into success; the lease-guarded release path keeps
	// AWAITING_INPUT intact downstream. We only assert it's not the
	// terminated-by-cancel outcome.
	assert.NotEqual(t, dispatchTerminated, outcome)
}

// TestLeaseClearedByTransition covers the helper's classification:
// only RUNNING/LEASED count as "still ours" (a genuine lost lease);
// every other status — including the benign waiting states and the
// terminal ones — means the lease was deliberately cleared.
func TestLeaseClearedByTransition(t *testing.T) {
	cases := []struct {
		status persistence.TaskStatus
		want   bool
	}{
		{persistence.TaskStatusRunning, false},
		{persistence.TaskStatusLeased, false},
		{persistence.TaskStatusAwaitingInput, true},
		{persistence.TaskStatusAwaitingExternal, true},
		{persistence.TaskStatusPaused, true},
		{persistence.TaskStatusCompleted, true},
		{persistence.TaskStatusCancelled, true},
		{persistence.TaskStatusQueued, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			repo := NewMockTaskRepository()
			repo.AddTask(&persistence.Task{ID: "task-1", ProjectID: "p", Status: tc.status})
			s := New(repo, &Config{LeaseDurationSeconds: 30})
			assert.Equal(t, tc.want, s.leaseClearedByTransition(context.Background(), "task-1"))
		})
	}

	// Reload failure (task gone) is treated as "not a deliberate clear"
	// so the caller keeps its escalation safety net for genuine DB
	// blips / lost rows.
	t.Run("get-error", func(t *testing.T) {
		repo := NewMockTaskRepository()
		s := New(repo, &Config{LeaseDurationSeconds: 30})
		assert.False(t, s.leaseClearedByTransition(context.Background(), "missing"))
	})
}
