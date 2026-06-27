package scheduler

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestRecoverExpiredLeases_NormalizesZeroMaxAttempts — pre-fix, a
// task with MaxAttempts=0 whose lease expired would be re-queued
// indefinitely: the terminal predicate `nextAttempt > MaxAttempts`
// was always false when MaxAttempts==0, and Postgres'
// `COALESCE(NULLIF($5,0))` preserves the existing 0. The recovery
// loop must apply the same `<=0 → 1` normalization that
// TaskCompleted already does so MaxAttempts=0 tasks fail terminally
// on first lease expiry instead of looping forever.
func TestRecoverExpiredLeases_NormalizesZeroMaxAttempts(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-zero-max"
	now := time.Now()
	task := &persistence.Task{
		ID:          "task-zero-max",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusLeased,
		Priority:    5,
		Attempt:     1,
		MaxAttempts: 0, // legacy / unconfigured
		LeaseID:     &leaseID,
		CreatedAt:   now,
	}
	mockRepo.AddExpiredLease(task)

	scheduler := New(mockRepo, &Config{RecoveryBatchSize: 10})
	scheduler.recoverExpiredLeases()

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	got := mockRepo.tasks["task-zero-max"]
	if got.Status != persistence.TaskStatusFailed {
		t.Errorf("status: got %s, want FAILED (MaxAttempts=0 must terminate, not loop)", got.Status)
	}
}

// TestRecoverExpiredLeases_FailsPastMaxAttemptsForMaxAttemptsOne —
// covers the simplest non-zero edge case (MaxAttempts=1, Attempt=1):
// the next attempt would be 2 > 1, so the task must fail
// terminally rather than be re-queued.
func TestRecoverExpiredLeases_FailsPastMaxAttemptsForMaxAttemptsOne(t *testing.T) {
	mockRepo := NewMockTaskRepository()
	leaseID := "lease-max-1"
	task := &persistence.Task{
		ID:          "task-max-1",
		ProjectID:   "project-1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		LeaseID:     &leaseID,
		CreatedAt:   time.Now(),
	}
	mockRepo.AddExpiredLease(task)

	scheduler := New(mockRepo, &Config{RecoveryBatchSize: 10})
	scheduler.recoverExpiredLeases()

	mockRepo.mu.Lock()
	defer mockRepo.mu.Unlock()
	got := mockRepo.tasks["task-max-1"]
	if got.Status != persistence.TaskStatusFailed {
		t.Errorf("status: got %s, want FAILED (MaxAttempts=1 exhausted)", got.Status)
	}
}
