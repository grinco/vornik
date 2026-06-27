package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// concurrencyTestExecutor blocks every dispatch on a shared
// release channel — letting the test pin runningCount up at
// dispatch time without the close-the-channel-twice race of
// blockingExecutor (which assumed a single dispatch per
// scheduler instance).
type concurrencyTestExecutor struct {
	executing atomic.Int64
	release   chan struct{}
}

func newConcurrencyTestExecutor() *concurrencyTestExecutor {
	return &concurrencyTestExecutor{release: make(chan struct{})}
}

func (e *concurrencyTestExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	e.executing.Add(1)
	go func() {
		<-e.release
		e.executing.Add(-1)
	}()
	return nil
}

func (e *concurrencyTestExecutor) IsExecuting(taskID string) bool {
	return e.executing.Load() > 0
}

func (e *concurrencyTestExecutor) Cancel(taskID string) error { return nil }

// TestScheduler_NoOverDispatch_ConcurrentSchedule asserts the
// runningCount cap holds when two schedule() callers race.
//
// Original bug: schedule() snapshotted runningCount under the
// lock, released the lock, then dispatched up to (max - count)
// tasks. Two goroutines both saw the same count, both leased +
// dispatched up to N tasks, exceeding MaxConcurrency by up to
// N-1. Each surplus task consumed an executor slot and a budget
// row — a real over-spend / over-allocation vector on busy
// daemons where the ticker + Wake() commonly fire close
// together.
//
// Fix: reserve a slot atomically (mu.Lock → runningCount++ →
// mu.Unlock → lease). On lease failure the reservation is
// rolled back. This test seeds enough tasks for many slots,
// fires lots of concurrent schedule() goroutines, and asserts
// runningCount never exceeds MaxConcurrency.
func TestScheduler_NoOverDispatch_ConcurrentSchedule(t *testing.T) {
	repo := NewMockTaskRepository()
	// Seed a generous queue — way more than MaxConcurrency so
	// every reservation can find a real task to lease.
	now := time.Now()
	for i := 0; i < 50; i++ {
		repo.AddTask(&persistence.Task{
			ID:        string(rune('a'+i%26)) + string(rune('a'+(i/26))),
			ProjectID: "project-1",
			Status:    persistence.TaskStatusQueued,
			Priority:  i,
			CreatedAt: now,
		})
	}

	const maxConcurrency = 3
	config := &Config{
		MaxConcurrency:       maxConcurrency,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	scheduler := New(repo, config)
	// Inject an executor that blocks indefinitely so leased tasks
	// stay "running" and runningCount stays at its peak — letting
	// us check the cap after the race rather than only the
	// dispatched-then-finished count.
	exec := newConcurrencyTestExecutor()
	scheduler.executor = exec

	// Fire many schedule() calls in parallel. Each races with
	// the others through the lock-acquire / lease / increment
	// sequence.
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduler.schedule()
		}()
	}
	wg.Wait()

	// Sample runningCount with the lock held. Must not exceed
	// the configured cap, regardless of how many goroutines
	// raced. Pre-fix, this assertion failed with values up to
	// 20 (every goroutine dispatching one task).
	scheduler.mu.Lock()
	running := scheduler.runningCount
	scheduler.mu.Unlock()
	if running > maxConcurrency {
		t.Fatalf("runningCount = %d, exceeds MaxConcurrency = %d (over-dispatch)", running, maxConcurrency)
	}
	if running < 1 {
		t.Fatalf("runningCount = %d, expected at least 1 dispatched task", running)
	}

	// Cleanup: release the blocked executor goroutines so the
	// test doesn't leak them.
	close(exec.release)
}

// TestScheduler_ReservationRollbackOnLeaseFailure asserts that a
// failed lease attempt rolls back the reservation so the slot
// becomes available again. Without the rollback, transient
// repo errors would permanently shrink the effective capacity.
func TestScheduler_ReservationRollbackOnLeaseFailure(t *testing.T) {
	repo := NewMockTaskRepository()
	// No tasks → every LeaseTask returns ErrNoTasksAvailable.

	config := &Config{
		MaxConcurrency:       2,
		LeaseDurationSeconds: 60,
		PollInterval:         10 * time.Millisecond,
		RecoveryInterval:     1 * time.Hour,
	}
	scheduler := New(repo, config)

	// Run a few schedule() passes. Each reserves a slot, leases,
	// fails (no work), rolls back. After all of them runningCount
	// must be back to 0.
	for i := 0; i < 5; i++ {
		scheduler.schedule()
	}

	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.runningCount != 0 {
		t.Fatalf("runningCount = %d after failed leases, want 0 (rollback failed)", scheduler.runningCount)
	}
}
