package narrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// fakeTasksNarr is a TaskGetter returning a scripted task status — lets the
// sweep consult whether the TASK (not just the execution) is terminal.
type fakeTasksNarr struct {
	status persistence.TaskStatus
	err    error // when set, Get returns this error (transient lookup failure)
}

func (f *fakeTasksNarr) Get(_ context.Context, id string) (*persistence.Task, error) {
	if f.err != nil {
		return nil, f.err
	}
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
	// ForceTeardown left at its default deliberately. A short one made this
	// negative assertion satisfiable by the STATE VANISHING rather than by
	// the suppression logic under test — expectNoLine cannot tell "correctly
	// suppressed" from "torn down before the sweep ever saw the terminal
	// status", so the test could pass with the §5.8 guard removed. Nothing
	// here waits on forceTeardownAfter, so the default costs no time.
	h := newTestHarness(t, func(n *Narrator) {
		n.Tasks = &fakeTasksNarr{status: persistence.TaskStatusRunning}
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
		// Default ForceTeardown — see the note above on vacuous passes.
	})
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "recover", Role: "worker"})
	h.awaitLine(2 * time.Second)
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	h.expectNoLine(300 * time.Millisecond)
}

// review-20260716-cea0: a TRANSIENT Tasks.Get error must NOT be treated as
// "task not active" — that fail-open path narrated a false "completed
// successfully" for an execution whose task was actually still running (the
// recovery false-completion the guard exists to prevent). On a lookup error the
// sweep suppresses and retries next tick.
func TestSweepIdle_NoCompletionOnTaskLookupError(t *testing.T) {
	h := newTestHarness(t, func(n *Narrator) {
		n.Tasks = &fakeTasksNarr{err: errors.New("db blip")}
		// Default ForceTeardown — see the note above on vacuous passes.
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

// TestSweepIdle_FailedAttemptNarratedWhileTaskRetrying — the complement to the
// recovery suppression: a FAILED execution on a still-active (retrying) task
// MUST be narrated, so the story doesn't end on the last step's "Finished
// step N" looking like success. Regression for headmatch task ...9417765,
// where a failed tester rework-loop was narrated as "everything passed".
func TestSweepIdle_FailedAttemptNarratedWhileTaskRetrying(t *testing.T) {
	// ForceTeardown is deliberately LEFT AT ITS DEFAULT (2h). Do not set a
	// short one here: this test flips the execution to FAILED *after*
	// awaiting the step-started line, and until that flip the execution is
	// still RUNNING — so sweepIdle takes the non-terminal branch, whose only
	// action is `if idle >= forceTeardownAfter() { teardown }`
	// (narrator.go:570). A 60ms ForceTeardown therefore destroyed the state
	// during the gap between the two statements, and the attempt-failed line
	// could never be emitted. It passed alone and failed under `go test
	// ./...`, where scheduling stretches that gap past 60ms — a flake whose
	// symptom (a 15s awaitLine timeout) looked like slowness but was a
	// destroyed precondition. This test asserts nothing about teardown, so
	// the override bought nothing.
	h := newTestHarness(t, func(n *Narrator) {
		n.Tasks = &fakeTasksNarr{status: persistence.TaskStatusRunning} // task still active (will retry)
	})
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "test", Role: "tester"})
	h.awaitLine(2 * time.Second) // the step-started line
	// This attempt FAILS while the task runs on (retry pending).
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusFailed)
	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion (attempt-failed line)", row.Kind)
	}
	if !strings.Contains(row.Text, "problem") {
		t.Fatalf("attempt-failed line must state the failure, got %q", row.Text)
	}
}
