package narrator

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

const testExecID = "exec-1"

// seedRunningExecution seeds the harness's fakeExecutions with
// testExecID as a RUNNING execution owned by proj-1/task-1 — the
// fixed identity every test in this package narrates against.
func seedRunningExecution(h *testHarness) {
	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusRunning)
}

// TestTrigger_StepStarted_EmitsAfterDebounce — a step-start candidate
// held for Debounce, then emitted as a "step" kind line, template
// text (no chat.Provider wired ⇒ Client nil ⇒ always fallback).
func TestTrigger_StepStarted_EmitsAfterDebounce(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{
		StepID: "s1", Role: "researcher",
	})

	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindStep {
		t.Errorf("Kind = %q, want step", row.Kind)
	}
	if row.Text == "" {
		t.Error("Text must not be empty")
	}
	if !row.Degraded {
		t.Error("no chat.Provider wired ⇒ line should be flagged Degraded")
	}
}

// TestTrigger_FastStartComplete_Collapses pins design §5.2: a step
// that completes before the debounce window elapses produces ONLY
// the completion line — the start candidate is dropped, never both.
func TestTrigger_FastStartComplete_Collapses(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	// Well inside the 20ms debounce window.
	time.Sleep(3 * time.Millisecond)
	h.Sub.push(testExecID, livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "s1", Outcome: "ok"})

	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindStep {
		t.Fatalf("Kind = %q, want step", row.Kind)
	}
	// Give the (cancelled) debounce timer a chance to fire and prove
	// it was actually suppressed, not just slower than the completion.
	h.expectNoLine(60 * time.Millisecond)
}

// TestTrigger_ToolBurst_OneHeartbeat — several long-running tool
// calls close together yield exactly one heartbeat line (min_line_
// interval coalescing), not one per call (design §5.2).
func TestTrigger_ToolBurst_OneHeartbeat(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	for i, callID := range []string{"c1", "c2", "c3"} {
		h.Sub.push(testExecID, livepubsub.KindToolCallStarted, livepubsub.ToolCallStartedPayload{
			StepID: "s1", CallID: callID, Tool: "web_search",
		})
		_ = i
	}
	// All three exceed LongToolThresh (20ms) and never finish, so
	// three heartbeats are armed close together; MinLineInterval
	// (15ms) coalesces the burst into one visible line.
	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindTool {
		t.Fatalf("Kind = %q, want tool", row.Kind)
	}
	h.expectNoLine(80 * time.Millisecond)
}

// TestTrigger_ToolCallFinished_CancelsHeartbeat — a tool call that
// finishes before the long-tool threshold never produces a heartbeat.
func TestTrigger_ToolCallFinished_CancelsHeartbeat(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindToolCallStarted, livepubsub.ToolCallStartedPayload{
		StepID: "s1", CallID: "c1", Tool: "read_file",
	})
	h.Sub.push(testExecID, livepubsub.KindToolCallFinished, livepubsub.ToolCallFinishedPayload{CallID: "c1"})

	h.expectNoLine(60 * time.Millisecond)
}

// TestTrigger_MinLineInterval_DropsRapidStepCompletions asserts at
// most one line lands per MinLineInterval even for legitimately
// distinct steps completing back to back.
func TestTrigger_MinLineInterval_DropsRapidStepCompletions(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.Sub.push(testExecID, livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "s1", Outcome: "ok"})
	first := h.awaitLine(2 * time.Second)
	if first.Kind != persistence.ExecutionNarrationKindStep {
		t.Fatalf("Kind = %q, want step", first.Kind)
	}

	// Immediately start+complete a second step — within MinLineInterval
	// (15ms) of the first line, so it must be dropped.
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s2", Role: "worker"})
	h.Sub.push(testExecID, livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "s2", Outcome: "ok"})
	h.expectNoLine(10 * time.Millisecond)
}

// TestTrigger_StepIdxIncrementsPerDistinctStep — the running step
// counter increments once per NEW step_id, not per event.
func TestTrigger_StepIdxIncrementsPerDistinctStep(t *testing.T) {
	h := newTestHarness(t)
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	first := h.awaitLine(2 * time.Second)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s2", Role: "worker"})
	time.Sleep(20 * time.Millisecond) // clear min_line_interval from the first line
	second := h.awaitLine(2 * time.Second)

	if first.Text == second.Text {
		// Not a strict requirement content-wise, but StepIdx (baked
		// into the template) must differ — a same-text collision
		// here would mean the counter didn't advance.
		t.Errorf("expected different step-index text between s1 and s2 lines, got identical: %q", first.Text)
	}
}

// TestTrigger_UnresolvableExecution_DropsEvent — when ExecutionLookup
// can't resolve an execution_id (e.g. a race with a not-yet-committed
// row), the event is dropped rather than crashing or persisting a
// row with blank required fields.
func TestTrigger_UnresolvableExecution_DropsEvent(t *testing.T) {
	h := newTestHarness(t)
	// Deliberately do NOT seed "exec-ghost" in h.Executions.

	h.Sub.push("exec-ghost", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.expectNoLine(60 * time.Millisecond)
}
