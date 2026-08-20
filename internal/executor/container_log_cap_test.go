package executor

import (
	"fmt"
	"strings"
	"testing"
)

// The line count and the header text were two independent literals: the runtime
// was asked for 50 lines while the header said "last 50 lines", so changing one
// silently made the other lie.
//
// 50 was also too few to diagnose with. Measured 2026-08-20 on the dev-pipeline
// report step: every failing rung emits a preflight + iteration pair per
// iteration, so a step with a real tool phase spends its whole 50-line budget on
// per-iteration bookkeeping and the tail no longer reaches the step-end
// decisions — whether the tool-free schema finalization fired, or the no-tool
// nudge. Those lines are the ones that distinguish causes, and they were being
// pushed out of the window by the log's own preamble.
//
// Raising the line count is close to free BECAUSE capContainerLog bounds bytes:
// the byte cap is the real control on what can reach a prompt, so more lines
// buys diagnostic depth without widening the blast radius the cap exists to
// contain.
func TestContainerLogSection(t *testing.T) {
	t.Run("header states the line count actually requested", func(t *testing.T) {
		got := containerLogSection("a\nb\n")
		want := fmt.Sprintf("last %d lines", containerLogTailLines)
		if !strings.Contains(got, want) {
			t.Errorf("header must name the configured tail (%q) so the text cannot drift\n"+
				"from the value passed to runtime.Logs; got:\n%s", want, got)
		}
	})

	t.Run("body is still byte-bounded", func(t *testing.T) {
		got := containerLogSection(strings.Repeat("z", 400_000))
		if len(got) > containerLogMaxBytes+512 {
			t.Errorf("section length %d exceeds the byte cap %d — raising the line count "+
				"must not remove the bound that keeps a log blob out of a prompt",
				len(got), containerLogMaxBytes)
		}
	})

	t.Run("tail is deep enough for a real tool phase", func(t *testing.T) {
		// A step that ran ~40 iterations emits ~80 preflight/iteration lines
		// before any step-end decision. The tail must outlast that preamble.
		if containerLogTailLines <= 80 {
			t.Errorf("containerLogTailLines = %d: a step with a real tool phase emits two "+
				"bookkeeping lines per iteration, so this cannot reach the step-end lines "+
				"that identify the failure", containerLogTailLines)
		}
	})
}

// A step error carries the tail of the container log so an operator can see what
// happened. It was appended by LINE count alone, with no byte bound — and a
// single line can be enormous.
//
// Measured 2026-08-19: the recovery-probe fixture's 1s timeout produced a
// runtime error whose log tail turned the lead's recovery context into a crash
// blob. Downstream, the lead generated ~1 MB responses over ~149s, which tripped
// the model-health circuit breaker and failed 24 of 27 attempts with
// MODEL_UNHEALTHY. Two separate measurements were invalidated before the cause
// was found.
//
// The error string is not just operator-facing: it is echoed into
// context.recovery.failure_reason and becomes part of the next agent's PROMPT. An
// unbounded blob there is a correctness problem, not a cosmetic one.
//
// Capping by bytes keeps the diagnostic value (the tail is where the failure is)
// while bounding what can reach a prompt. It is also what makes the line count
// safe to raise: see TestContainerLogSection.
func TestCapContainerLog(t *testing.T) {
	t.Run("short log passes through unchanged", func(t *testing.T) {
		in := "line one\nline two\n"
		if got := capContainerLog(in); got != in {
			t.Errorf("a small log must be untouched; got %q", got)
		}
	})

	t.Run("oversized log is truncated and says so", func(t *testing.T) {
		huge := strings.Repeat("x", 200_000)
		got := capContainerLog(huge)
		if len(got) >= len(huge) {
			t.Fatalf("not truncated: %d bytes in, %d out", len(huge), len(got))
		}
		if len(got) > containerLogMaxBytes+512 {
			t.Errorf("result %d bytes exceeds the cap %d plus a small notice",
				len(got), containerLogMaxBytes)
		}
		if !strings.Contains(got, "truncated") {
			t.Error("a truncated log must say it was truncated, or the reader trusts an " +
				"incomplete tail as complete")
		}
	})

	t.Run("keeps the TAIL, not the head", func(t *testing.T) {
		// The failure is at the end of a log. Keeping the head would discard
		// exactly the part worth reading.
		body := strings.Repeat("noise\n", 100_000) + "FINAL_FAILURE_MARKER"
		got := capContainerLog(body)
		if !strings.Contains(got, "FINAL_FAILURE_MARKER") {
			t.Error("truncation must retain the tail — the failure is at the end")
		}
	})

	t.Run("one enormous single line is still bounded", func(t *testing.T) {
		// The LINE bound is no protection here: one line, no newlines.
		got := capContainerLog(strings.Repeat("y", 500_000))
		if len(got) > containerLogMaxBytes+512 {
			t.Errorf("a single huge line must still be capped; got %d bytes", len(got))
		}
	})

	t.Run("empty stays empty", func(t *testing.T) {
		if got := capContainerLog(""); got != "" {
			t.Errorf("empty in, empty out; got %q", got)
		}
	})
}
