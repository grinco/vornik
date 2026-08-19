package executor

import (
	"strings"
	"testing"
)

// A step error carries the tail of the container log so an operator can see what
// happened. It is appended by LINE count (50), with no byte bound — and a single
// line can be enormous.
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
// while bounding what can reach a prompt.
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
		// The 50-LINE bound is no protection here: one line, no newlines.
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
