package narrator

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// TestSweepIdle_TerminalCompletion — the idle-poll fallback (no bus
// event exists for execution completion, per design non-goal #1)
// detects a terminal execution once it's gone quiet and emits the
// completion line, then tears down the per-execution state.
func TestSweepIdle_TerminalCompletion(t *testing.T) {
	torndown := make(chan string, 4)
	h := newTestHarness(t, func(n *Narrator) {
		n.onTeardown = func(id string) { torndown <- id }
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second) // the step-started line; drains it so it doesn't get mistaken for completion

	// Flip to terminal AFTER the state already exists — the sweep
	// samples status on its own cadence, not on a bus signal.
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)

	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion", row.Kind)
	}
	select {
	case id := <-torndown:
		if id != testExecID {
			t.Errorf("torn-down execution = %q, want %q", id, testExecID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state was never torn down after the completion line")
	}
}

// TestSweepIdle_Failed_RendersFailureCompletion — a FAILED terminal
// status renders the "didn't complete successfully" branch.
func TestSweepIdle_Failed_RendersFailureCompletion(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusFailed)

	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion", row.Kind)
	}
	if row.Text != "The task didn't complete successfully." {
		t.Errorf("Text = %q, want the failure completion line", row.Text)
	}
}

// TestSweepIdle_StillRunning_NoLineNoTeardown — while the execution
// is merely quiet (not terminal), the sweep leaves the state alone.
func TestSweepIdle_StillRunning_NoLineNoTeardown(t *testing.T) {
	torndown := make(chan string, 4)
	h := newTestHarness(t, func(n *Narrator) {
		n.onTeardown = func(id string) { torndown <- id }
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	// No further events, status stays RUNNING — several sweep ticks
	// pass with no line and no teardown.
	h.expectNoLine(150 * time.Millisecond)
	select {
	case id := <-torndown:
		t.Fatalf("state must survive while the execution is still running; got teardown of %q", id)
	default:
	}
}

// TestSweepIdle_ForceTeardown_WhenExecutionUnresolvable bounds memory
// for an execution the lookup can no longer resolve (e.g. purged) —
// the state is dropped after ForceTeardown even with no completion
// line, mirroring livepubsub's own idle-stream eviction philosophy.
func TestSweepIdle_ForceTeardown_WhenExecutionUnresolvable(t *testing.T) {
	torndown := make(chan string, 4)
	h := newTestHarness(t, func(n *Narrator) {
		n.ForceTeardown = 40 * time.Millisecond
		n.onTeardown = func(id string) { torndown <- id }
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	h.Executions.unset(testExecID) // Get now fails ⇒ unresolvable

	select {
	case id := <-torndown:
		if id != testExecID {
			t.Errorf("torn-down execution = %q, want %q", id, testExecID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state was never torn down after ForceTeardown elapsed on an unresolvable execution")
	}
}
