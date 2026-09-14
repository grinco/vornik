package narrator

import (
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// THE COMPLETION LINE CANNOT BE COALESCED AWAY.
//
// `emitLine` drops a line silently when it lands within `min_line_interval`
// of the previous one — right for progress lines, which are interchangeable
// and followed by another. Wrong for the COMPLETION line, because `sweepIdle`
// tears the execution's state down on the very next statement:
//
//	n.emitLine(ctx, execID, st, triggerCompletion, ...)
//	n.teardown(execID)
//
// So a completion that arrives too soon after the last step line is dropped
// and then made unrecoverable — the story ends on "Finished step N" and reads
// as though the task never finished. That is the same failure the
// attempt-failed line exists to prevent (headmatch task ...9417765, §sweepIdle).
//
// FOUND AS A TEST FLAKE, which is what it looks like from outside:
// TestSweepIdle_TerminalCompletion timed out three times over 2026-09-09..13,
// always on a loaded host, always passing alone. The wait floor had been raised
// 15s → 60s twice, and the note left at narrationWaitFloor said that a third
// occurrence meant the deadline was the wrong instrument. It was: under load
// the step line's 20 ms debounce pushes it close enough to the sweep that the
// two land inside the 15 ms interval, the completion is dropped, the state is
// destroyed, and the test waits forever for a line that will never come. The
// flake was reporting a real defect, at the rate the race happened to occur.
//
// This test drives the race deterministically through the nowFn seam instead
// of waiting for a loaded machine to produce it.
func TestEmitLine_CompletionSurvivesTheMinLineInterval(t *testing.T) {
	var (
		mu    sync.Mutex
		clock = time.Now()
	)
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d)
	}
	h := newTestHarness(t, func(n *Narrator) {
		n.nowFn = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		}
		// Long enough that no passage of time can satisfy it: the ONLY way the
		// completion line gets out is the exemption under test.
		n.MinLineInterval = time.Hour
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	step := h.awaitLine(2 * time.Second)
	if step.Kind == persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("expected the step line first, got %+v", step)
	}

	// Terminal execution. Advancing one second clears the 30 ms idle threshold
	// the sweep waits for and stays FAR inside the one-hour coalescing window
	// the step line just opened — which is exactly the production race, with
	// the ratio between the two intervals exaggerated so it cannot be timing.

	h.Executions.set(testExecID, "proj-1", "task-1", persistence.ExecutionStatusCompleted)
	advance(time.Second)

	row := h.awaitLine(2 * time.Second)
	if row.Kind != persistence.ExecutionNarrationKindCompletion {
		t.Fatalf("Kind = %q, want completion — a completion dropped by min_line_interval "+
			"is dropped forever, because sweepIdle tears the state down on the next line", row.Kind)
	}
}

// The coalescer still works for the lines it is FOR. Without this the fix
// could be "exempt everything", which removes the rate limit the setting
// exists to provide.
func TestEmitLine_ProgressLinesAreStillCoalesced(t *testing.T) {
	fixed := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	h := newTestHarness(t, func(n *Narrator) {
		n.nowFn = func() time.Time { return fixed } // time never advances
		n.MinLineInterval = time.Hour
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})
	h.awaitLine(2 * time.Second)

	// A second progress event inside the window must be swallowed.
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s2", Role: "worker"})
	h.expectNoLine(300 * time.Millisecond)
}
