package narrator

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// fakeTasksNarr is a TaskGetter returning a scripted task status — lets the
// sweep consult whether the TASK (not just the execution) is terminal.
type fakeTasksNarr struct{ status persistence.TaskStatus }

func (f *fakeTasksNarr) Get(_ context.Context, id string) (*persistence.Task, error) {
	return &persistence.Task{ID: id, ProjectID: "proj-1", Status: f.status}, nil
}

// Incident 2026-07-11: a research task narrated "completed successfully" while
// it was in RECOVERY mode. The narrator emitted a completion line the moment
// an EXECUTION reached a terminal status — but a recover/checkpoint flow ends
// an execution at an intermediate step and spawns a continuation, so the TASK
// is still mid-flight. Completion must key off the TASK being terminal.

// TestSweepIdle_NoCompletionWhileTaskStillActive — execution COMPLETED but the
// task is still RUNNING (recovery continuation coming) → NO completion line.
func TestSweepIdle_NoCompletionWhileTaskStillActive(t *testing.T) {
	h := newTestHarness(t, func(n *Narrator) {
		n.Tasks = &fakeTasksNarr{status: persistence.TaskStatusRunning}
		// Tear down fast so the test doesn't wait forceTeardownAfter.
		n.ForceTeardown = 60 * time.Millisecond
	})
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "recover", Role: "worker"})
	h.awaitLine(2 * time.Second) // the step-started line
	// The execution completes (at the recover checkpoint) while the task runs on.
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	// No completion line may appear.
	h.expectNoLine(300 * time.Millisecond)
}

// A PAUSED task (the …80b0 case: paused after recovery) is likewise not done.
func TestSweepIdle_NoCompletionWhileTaskPaused(t *testing.T) {
	h := newTestHarness(t, func(n *Narrator) {
		n.Tasks = &fakeTasksNarr{status: persistence.TaskStatusPaused}
		n.ForceTeardown = 60 * time.Millisecond
	})
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "recover", Role: "worker"})
	h.awaitLine(2 * time.Second)
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	h.expectNoLine(300 * time.Millisecond)
}

// When the TASK is genuinely terminal, the completion line fires as before.
func TestSweepIdle_CompletionWhenTaskTerminal(t *testing.T) {
	h := newTestHarness(t, func(n *Narrator) {
		n.Tasks = &fakeTasksNarr{status: persistence.TaskStatusCompleted}
	})
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion", row.Kind)
	}
}

// Nil Tasks (not wired) preserves the prior behavior: emit on execution
// terminal (the sweep can't consult task status).
func TestSweepIdle_NilTasksKeepsExecutionTerminalBehavior(t *testing.T) {
	h := newTestHarness(t) // Tasks left nil
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("nil Tasks must keep emitting on execution-terminal; got %q", row.Kind)
	}
}
