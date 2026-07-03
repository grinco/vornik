package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// Regression for B1/B2 (audit 2026-07-03): pauseWithReason and TaskLogs read
// executionHandle.containerID AFTER releasing e.mu, racing runExecution's
// lock-held write of that field when a step's container starts. A lost race
// meant StopContainer was skipped — the agent container kept running (and
// billing) past the pause, and on shutdown the next boot's pruneAllWorktrees
// yanked the worktree from under the orphan. Cancel already snapshots the id
// under the lock (executor.go); these two did not. Detectable under -race.
//
// hammerContainerID keeps rewriting handle.containerID under e.mu for the
// whole duration of the handler under test, so the unsynchronized read (if
// present) reliably overlaps a write and the detector fires.
func hammerContainerID(e *Executor, h *executionHandle) (stop, done chan struct{}) {
	stop = make(chan struct{})
	done = make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				// Rewrite the same valid id: the race detector instruments the
				// memory access, not the value, so this still races an
				// unsynchronized read while keeping WaitForExit("c1") fast.
				e.mu.Lock()
				h.containerID = "c1"
				e.mu.Unlock()
			}
		}
	}()
	return stop, done
}

func TestExecutor_PauseContainerIDNoRace(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{
		ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning,
	})
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	h := &executionHandle{taskID: "t1", containerID: "c1", cancel: cancel}
	e.activeExecutions["t1"] = h
	e.mu.Unlock()

	stop, done := hammerContainerID(e, h)
	// pauseWithReason blocks in waitForExecutionCleanup until the run
	// goroutine removes the handle; there is no such goroutine here, so
	// remove it after Pause has passed the (racy) containerID read to keep
	// the test fast. The read happens in microseconds; 50ms is ample margin.
	go func() {
		time.Sleep(50 * time.Millisecond)
		e.mu.Lock()
		delete(e.activeExecutions, "t1")
		e.mu.Unlock()
	}()
	status, err := e.Pause("t1")
	close(stop)
	<-done
	require.NoError(t, err)
	require.NotNil(t, status)
}

func TestExecutor_TaskLogsContainerIDNoRace(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	e.mu.Lock()
	h := &executionHandle{taskID: "t1", containerID: "c1"}
	e.activeExecutions["t1"] = h
	e.mu.Unlock()

	stop, done := hammerContainerID(e, h)
	_, err := e.TaskLogs(context.Background(), "t1", 10)
	close(stop)
	<-done
	require.NoError(t, err)
}
