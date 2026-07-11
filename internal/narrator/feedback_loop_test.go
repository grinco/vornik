package narrator

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// Incident 2026-07-11 "narrator spam": the narrator publishes each line back
// onto the live bus (KindNarrationLine) AND subscribes with SubscribeAll —
// receiving its own output re-created per-execution state after teardown, so
// every idle-poll tick re-emitted the completion line (one LLM call each,
// forever; 180+ "Task completed successfully." rows on one prod task). The
// production test harness never loops Pub back into Sub, which is why the
// sweep tests missed it. These tests simulate the loopback explicitly.

// TestFeedbackLoop_OwnNarrationEventIgnored — a KindNarrationLine event (our
// own published output) must never (re)create state or emit anything.
func TestFeedbackLoop_OwnNarrationEventIgnored(t *testing.T) {
	torndown := make(chan string, 4)
	h := newTestHarness(t, func(n *Narrator) {
		n.onTeardown = func(id string) { torndown <- id }
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion", row.Kind)
	}
	select {
	case <-torndown:
	case <-time.After(2 * time.Second):
		t.Fatal("no teardown after completion")
	}

	// The loopback: our own published completion line arrives on the bus.
	h.Sub.push(testExecID, livepubsub.KindNarrationLine, livepubsub.NarrationLinePayload{Text: "Task completed successfully."})
	// It must NOT resurrect state — no second completion on later sweeps.
	h.expectNoLine(300 * time.Millisecond)
}

// TestFeedbackLoop_StragglerEventOnTerminalExecution — defence in depth: any
// late non-narration event (a replayed tool_call_finished, say) on an
// execution that is already terminal must not resurrect its story either.
func TestFeedbackLoop_StragglerEventOnTerminalExecution(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	if row := h.awaitLine(2 * time.Second); row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion", row.Kind)
	}

	// Straggler arrives after teardown; the execution is terminal in the DB.
	h.Sub.push(testExecID, livepubsub.KindToolCallStarted, livepubsub.ToolCallStartedPayload{CallID: "c9", StepID: "s1", Tool: "web_fetch"})
	h.expectNoLine(300 * time.Millisecond)
}
