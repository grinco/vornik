package executor

import (
	"sync"
	"testing"
	"time"
)

// TestWaitForExecutionCleanup_ReturnsTrueWhenEntryCleared pins
// the happy-path contract: when the goroutine's deferred
// cleanupExecution removes the entry from activeExecutions, the
// waiter unblocks and returns true.
func TestWaitForExecutionCleanup_ReturnsTrueWhenEntryCleared(t *testing.T) {
	e := &Executor{
		activeExecutions: map[string]*executionHandle{
			"task-1": {taskID: "task-1"},
		},
	}

	// Goroutine that simulates the runExecution defer: removes the
	// entry from activeExecutions ~30ms after Pause returns.
	go func() {
		time.Sleep(30 * time.Millisecond)
		e.mu.Lock()
		delete(e.activeExecutions, "task-1")
		e.mu.Unlock()
	}()

	got := e.waitForExecutionCleanup("task-1", 1*time.Second)
	if !got {
		t.Fatal("expected waitForExecutionCleanup=true after entry was cleared")
	}
}

// TestWaitForExecutionCleanup_ReturnsFalseOnTimeout — when the
// goroutine doesn't clean up within the timeout, the waiter
// gives up and returns false. This bounds Pause()'s blocking
// time so a stuck goroutine can't hang an operator-initiated
// pause forever; the scheduler's recovery sweep eventually
// catches the orphan.
func TestWaitForExecutionCleanup_ReturnsFalseOnTimeout(t *testing.T) {
	e := &Executor{
		activeExecutions: map[string]*executionHandle{
			"task-stuck": {taskID: "task-stuck"},
		},
	}
	got := e.waitForExecutionCleanup("task-stuck", 75*time.Millisecond)
	if got {
		t.Fatal("expected waitForExecutionCleanup=false when entry stayed populated past the timeout")
	}
	// And the entry must still be there — waitForExecutionCleanup
	// observes, not removes.
	e.mu.Lock()
	_, present := e.activeExecutions["task-stuck"]
	e.mu.Unlock()
	if !present {
		t.Error("waitForExecutionCleanup must not delete the entry; that's the goroutine's job")
	}
}

// TestWaitForExecutionCleanup_ReturnsTrueWhenAlreadyCleared —
// no-op fast path when the entry was never there (or already
// gone). Avoids unnecessary 25ms sleep on the common case.
func TestWaitForExecutionCleanup_ReturnsTrueWhenAlreadyCleared(t *testing.T) {
	e := &Executor{
		activeExecutions: map[string]*executionHandle{},
	}
	start := time.Now()
	got := e.waitForExecutionCleanup("nope", 5*time.Second)
	elapsed := time.Since(start)
	if !got {
		t.Fatal("expected immediate true when entry was never present")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("absent-entry path should return immediately; took %v", elapsed)
	}
}

// TestWaitForExecutionCleanup_ConcurrencySafe drives many
// concurrent waiters while a single goroutine clears the entry
// to ensure the polling loop's lock acquisition stays correct.
// Doesn't validate ordering — just that nobody panics or
// deadlocks.
func TestWaitForExecutionCleanup_ConcurrencySafe(t *testing.T) {
	e := &Executor{
		activeExecutions: map[string]*executionHandle{"shared": {taskID: "shared"}},
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.waitForExecutionCleanup("shared", 1*time.Second)
		}()
	}

	time.Sleep(40 * time.Millisecond)
	e.mu.Lock()
	delete(e.activeExecutions, "shared")
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters did not unblock after entry was cleared")
	}
}
