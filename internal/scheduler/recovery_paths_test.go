package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// recoveryExecutor is a minimal Executor whose IsExecuting verdict is
// configurable per task ID, letting recoverExpiredLeases tests drive
// the three recovery branches: (1) active executor → renew, (2) idle
// executor within grace → defer, (3) idle executor past grace →
// recover. ExecuteWithContext/Cancel are unused on the recovery path
// but must exist to satisfy the interface.
type recoveryExecutor struct {
	executing map[string]bool
	cancels   int
}

func (e *recoveryExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	return nil
}

func (e *recoveryExecutor) IsExecuting(taskID string) bool {
	return e.executing[taskID]
}

func (e *recoveryExecutor) Cancel(taskID string) error {
	e.cancels++
	return nil
}

// newLeasedExpiredTask builds a task already in the LEASED state with
// a populated lease identity, suitable for AddExpiredLease. attempt /
// maxAttempts let callers exercise the retry vs. dead-letter split.
func newLeasedExpiredTask(id string, attempt, maxAttempts int) *persistence.Task {
	leaseID := "lease-" + id
	holder := "scheduler-1"
	now := time.Now().Add(-10 * time.Minute)
	expired := time.Now().Add(-5 * time.Minute)
	return &persistence.Task{
		ID:             id,
		ProjectID:      "project-recovery",
		Status:         persistence.TaskStatusLeased,
		Priority:       5,
		Attempt:        attempt,
		MaxAttempts:    maxAttempts,
		LeaseID:        &leaseID,
		LeasedBy:       &holder,
		LeasedAt:       &now,
		LeaseExpiresAt: &expired,
		CreatedAt:      now,
	}
}

// TestRecoverExpiredLeases_RequeuesWhenAttemptsRemain — the retry path.
// A task with attempts left (Attempt=1, MaxAttempts=3) whose lease
// expired must be re-queued (not failed) with the attempt counter
// bumped to 2, so a transient orphaning gets another execution.
func TestRecoverExpiredLeases_RequeuesWhenAttemptsRemain(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-retry", 1, 3))

	s := New(repo, &Config{RecoveryBatchSize: 10})
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	got := repo.tasks["task-retry"]
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("status: got %s, want QUEUED (attempts remain)", got.Status)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt: got %d, want 2 (nextAttempt = attempt+1)", got.Attempt)
	}
	// The lease must be cleared on recovery so the task is re-leasable.
	if got.LeaseID != nil {
		t.Errorf("lease ID: got %v, want nil after recovery", *got.LeaseID)
	}
}

// TestRecoverExpiredLeases_DeadLettersAtMaxAttempts — DLQ routing
// boundary. With Attempt=3, MaxAttempts=3, the next attempt (4) would
// exceed the cap, so the task must be routed to FAILED (dead-letter)
// rather than re-queued. The attempt counter is pinned at the cap, not
// bumped past it.
func TestRecoverExpiredLeases_DeadLettersAtMaxAttempts(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-dlq", 3, 3))

	s := New(repo, &Config{RecoveryBatchSize: 10})
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	got := repo.tasks["task-dlq"]
	if got.Status != persistence.TaskStatusFailed {
		t.Fatalf("status: got %s, want FAILED (max attempts exhausted)", got.Status)
	}
	if got.Attempt != 3 {
		t.Errorf("attempt: got %d, want 3 (pinned at cap, not bumped to 4)", got.Attempt)
	}
}

// TestRecoverExpiredLeases_RenewsWhenExecutorStillActive — the common
// long-step case (LLD §6). The DB lease expired but the executor still
// reports IsExecuting=true, so the task must NOT be recovered/re-queued;
// instead its lease is renewed and it stays LEASED. Recovering here
// would let a sibling lease in violation of the per-project cap.
func TestRecoverExpiredLeases_RenewsWhenExecutorStillActive(t *testing.T) {
	repo := NewMockTaskRepository()
	task := newLeasedExpiredTask("task-active", 1, 3)
	repo.AddExpiredLease(task)

	exec := &recoveryExecutor{executing: map[string]bool{"task-active": true}}
	s := NewWithOptions(repo, &Config{RecoveryBatchSize: 10}, WithExecutor(exec))
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	got := repo.tasks["task-active"]
	if got.Status != persistence.TaskStatusLeased {
		t.Fatalf("status: got %s, want LEASED (active executor → renew, not recover)", got.Status)
	}
	if repo.releaseLeaseCalls != 0 {
		t.Errorf("releaseLeaseCalls: got %d, want 0 (must renew, not release)", repo.releaseLeaseCalls)
	}
	if repo.renewLeaseCalls != 1 {
		t.Errorf("renewLeaseCalls: got %d, want 1", repo.renewLeaseCalls)
	}
}

// TestRecoverExpiredLeases_DefersWithinGraceWindow — an idle executor
// observation must not recover immediately. On the first sweep where
// IsExecuting=false, recordRecoveryIdleSince stamps "now"; since
// now-firstSeen (0) is < the grace window, recovery is deferred and the
// task stays LEASED. This guards the per-project at-most-N invariant
// against a single transient idle blip between step transitions.
func TestRecoverExpiredLeases_DefersWithinGraceWindow(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-grace", 1, 3))

	// Idle executor (IsExecuting=false) + a non-zero grace so the
	// first observation falls inside the window.
	exec := &recoveryExecutor{executing: map[string]bool{}}
	s := NewWithOptions(repo, &Config{RecoveryBatchSize: 10, RecoveryIdleGrace: time.Minute}, WithExecutor(exec))
	s.recoverExpiredLeases()

	repo.mu.Lock()
	got := repo.tasks["task-grace"]
	releaseCalls := repo.releaseLeaseCalls
	repo.mu.Unlock()

	if got.Status != persistence.TaskStatusLeased {
		t.Fatalf("status: got %s, want LEASED (deferred within grace window)", got.Status)
	}
	if releaseCalls != 0 {
		t.Errorf("releaseLeaseCalls: got %d, want 0 (deferred, not recovered)", releaseCalls)
	}
	// An idle observation must have been stamped for the next sweep.
	s.mu.Lock()
	_, stamped := s.recoveryIdleSince["task-grace"]
	s.mu.Unlock()
	if !stamped {
		t.Errorf("recoveryIdleSince: expected an idle observation stamped for task-grace")
	}
}

// TestRecoverExpiredLeases_RecoversAfterGraceElapsed — once the idle
// observation predates the grace window, the task is genuinely
// orphaned and must be recovered. We pre-seed recoveryIdleSince with a
// timestamp older than the grace so the first sweep crosses the
// threshold and re-queues (attempts remain).
func TestRecoverExpiredLeases_RecoversAfterGraceElapsed(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-orphan", 1, 3))

	exec := &recoveryExecutor{executing: map[string]bool{}}
	s := NewWithOptions(repo, &Config{RecoveryBatchSize: 10, RecoveryIdleGrace: time.Minute}, WithExecutor(exec))

	// Stamp the idle observation well in the past so now-firstSeen
	// exceeds the grace window on this sweep.
	s.mu.Lock()
	s.recoveryIdleSince = map[string]time.Time{"task-orphan": time.Now().Add(-10 * time.Minute)}
	s.mu.Unlock()

	s.recoverExpiredLeases()

	repo.mu.Lock()
	got := repo.tasks["task-orphan"]
	repo.mu.Unlock()
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("status: got %s, want QUEUED (orphaned past grace, attempts remain)", got.Status)
	}
	// Recovery consumes the idle observation so a fresh re-lease starts clean.
	s.mu.Lock()
	_, stillStamped := s.recoveryIdleSince["task-orphan"]
	s.mu.Unlock()
	if stillStamped {
		t.Errorf("recoveryIdleSince: observation should be cleared after recovery")
	}
}

// TestRecoverExpiredLeases_ClearsIdleWhenExecutorBackToLife — a prior
// idle observation must be cleared the moment the executor reports the
// task as executing again (the earlier IsExecuting=false was
// transient). Otherwise a stale observation could later trip recovery
// on a live task.
func TestRecoverExpiredLeases_ClearsIdleWhenExecutorBackToLife(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-revive", 1, 3))

	exec := &recoveryExecutor{executing: map[string]bool{"task-revive": true}}
	s := NewWithOptions(repo, &Config{RecoveryBatchSize: 10, RecoveryIdleGrace: time.Minute}, WithExecutor(exec))

	// Pre-existing stale idle observation from an earlier sweep.
	s.mu.Lock()
	s.recoveryIdleSince = map[string]time.Time{"task-revive": time.Now().Add(-10 * time.Minute)}
	s.mu.Unlock()

	s.recoverExpiredLeases()

	s.mu.Lock()
	_, stillStamped := s.recoveryIdleSince["task-revive"]
	s.mu.Unlock()
	if stillStamped {
		t.Errorf("recoveryIdleSince: observation must be cleared when executor is active again")
	}
}

// TestRecoverExpiredLeases_FindExpiredErrorIsHandled — a DB error from
// FindExpiredLeases must abort the sweep cleanly (no panic, no
// release) and leave it to the next tick.
func TestRecoverExpiredLeases_FindExpiredErrorIsHandled(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.findExpiredErr = errors.New("db down")

	s := New(repo, &Config{RecoveryBatchSize: 10})
	s.recoverExpiredLeases() // must not panic

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.releaseLeaseCalls != 0 {
		t.Errorf("releaseLeaseCalls: got %d, want 0 (find failed, nothing to release)", repo.releaseLeaseCalls)
	}
}

// TestRecoverExpiredLeases_ReleaseErrorDoesNotClearIdle — when
// ReleaseLease itself fails, the loop logs and continues; crucially it
// must NOT clear the idle observation (the clear lives after a
// successful release), so a later sweep can retry recovery once the DB
// recovers.
func TestRecoverExpiredLeases_ReleaseErrorDoesNotClearIdle(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-relfail", 1, 3))
	repo.releaseLeaseErr = errors.New("release failed")

	exec := &recoveryExecutor{executing: map[string]bool{}}
	s := NewWithOptions(repo, &Config{RecoveryBatchSize: 10, RecoveryIdleGrace: time.Minute}, WithExecutor(exec))

	s.mu.Lock()
	s.recoveryIdleSince = map[string]time.Time{"task-relfail": time.Now().Add(-10 * time.Minute)}
	s.mu.Unlock()

	s.recoverExpiredLeases()

	s.mu.Lock()
	_, stillStamped := s.recoveryIdleSince["task-relfail"]
	s.mu.Unlock()
	if !stillStamped {
		t.Errorf("recoveryIdleSince: observation must survive a failed release for a later retry")
	}
}

// TestRecoverExpiredLeases_NoExecutorRecoversImmediately — without an
// executor wired in (s.executor == nil), neither the renewal branch nor
// the grace-deferral branch applies, so an expired lease is recovered
// straight away. With attempts remaining it re-queues.
func TestRecoverExpiredLeases_NoExecutorRecoversImmediately(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-noexec", 1, 3))

	s := New(repo, &Config{RecoveryBatchSize: 10}) // no WithExecutor
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	got := repo.tasks["task-noexec"]
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("status: got %s, want QUEUED (no executor → recover immediately)", got.Status)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt: got %d, want 2", got.Attempt)
	}
}

// TestRecoverExpiredLeases_EmptyBatchIsNoop — an empty expired-lease
// batch must short-circuit without touching the release path.
func TestRecoverExpiredLeases_EmptyBatchIsNoop(t *testing.T) {
	repo := NewMockTaskRepository()
	s := New(repo, &Config{RecoveryBatchSize: 10})
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.findExpiredCalls != 1 {
		t.Errorf("findExpiredCalls: got %d, want 1", repo.findExpiredCalls)
	}
	if repo.releaseLeaseCalls != 0 {
		t.Errorf("releaseLeaseCalls: got %d, want 0 on empty batch", repo.releaseLeaseCalls)
	}
}

// TestRecoverExpiredLeases_RenewFailureFallsBackToRecovery — when the
// executor is active but RenewLease fails (e.g. the lease really did
// vanish under us), the loop must fall through to the recovery path
// rather than leaving the task stuck LEASED. With attempts remaining it
// re-queues.
func TestRecoverExpiredLeases_RenewFailureFallsBackToRecovery(t *testing.T) {
	repo := NewMockTaskRepository()
	// The batch entry (returned by FindExpiredLeases, source of
	// taskLeaseID) and the stored row (consulted by RenewLease) must be
	// distinct pointers carrying different lease IDs so RenewLease's
	// ownership check returns ErrLeaseNotFound — modelling a lease that
	// vanished under us. AddExpiredLease aliases one pointer into both,
	// so we wire the two maps by hand.
	batchTask := newLeasedExpiredTask("task-renewfail", 1, 3)
	storedTask := newLeasedExpiredTask("task-renewfail", 1, 3)
	stolen := "lease-stolen"
	storedTask.LeaseID = &stolen
	repo.mu.Lock()
	repo.expiredLeases = append(repo.expiredLeases, batchTask)
	repo.tasks["task-renewfail"] = storedTask
	repo.mu.Unlock()

	exec := &recoveryExecutor{executing: map[string]bool{"task-renewfail": true}}
	s := NewWithOptions(repo, &Config{RecoveryBatchSize: 10}, WithExecutor(exec))
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	got := repo.tasks["task-renewfail"]
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("status: got %s, want QUEUED (renew failed → fall back to recovery)", got.Status)
	}
	if repo.renewLeaseCalls != 1 {
		t.Errorf("renewLeaseCalls: got %d, want 1 (attempted before fallback)", repo.renewLeaseCalls)
	}
}

// TestRecoverExpiredLeases_BatchBoundAtRecoveryBatchSize — the sweep
// honours RecoveryBatchSize: with two expired leases and a batch size
// of 1, exactly one task is processed in a single sweep.
func TestRecoverExpiredLeases_BatchBoundAtRecoveryBatchSize(t *testing.T) {
	repo := NewMockTaskRepository()
	repo.AddExpiredLease(newLeasedExpiredTask("task-batch-a", 1, 3))
	repo.AddExpiredLease(newLeasedExpiredTask("task-batch-b", 1, 3))

	s := New(repo, &Config{RecoveryBatchSize: 1})
	s.recoverExpiredLeases()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	// Exactly one of the two should have been released (QUEUED); the
	// other stays LEASED for the next sweep.
	queued := 0
	for _, id := range []string{"task-batch-a", "task-batch-b"} {
		if repo.tasks[id].Status == persistence.TaskStatusQueued {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("queued tasks: got %d, want 1 (batch bound to RecoveryBatchSize=1)", queued)
	}
}
