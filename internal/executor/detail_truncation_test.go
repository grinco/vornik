package executor

import (
	"strings"
	"testing"
)

// A step's error_detail carries two things whose useful parts sit at OPPOSITE
// ends: the daemon's own message first ("schema violation: role %q result.json is
// missing required keys: [...]"), then the container-log section appended after
// it, whose interesting lines are at its end because that is where the step
// decided what to do.
//
// Two truncations used to fight over it:
//
//	capContainerLog     keeps the TAIL — "the failure is at the end. Keeping the
//	                    head would discard exactly the part worth reading."
//	truncateStr(…,2000) keeps the HEAD — s[:max] + "..."
//
// The second ran last, so the stored detail was the first 2000 bytes: the
// daemon's message, then the agent's startup and earliest iterations, and
// nothing else. Measured 2026-08-20: every failing report rung in bench arms 4-6
// stored exactly that shape, which is why raising containerLogTailLines from 50
// to 400 changed nothing observable — the deeper tail was fetched, appended, and
// then discarded by the head-truncation. Step-end lines (schema finalization, the
// no-tool nudge, the iteration cap) are beyond 2000 bytes BY CONSTRUCTION and
// could never be stored.
//
// So a single-ended truncation cannot serve this field. Keep both ends.
func TestTruncateDetailPreservingEnds(t *testing.T) {
	t.Run("short input is untouched", func(t *testing.T) {
		in := "schema violation: role \"analyst\" result.json is missing required keys"
		if got := truncateDetailPreservingEnds(in, 2000); got != in {
			t.Errorf("short detail must pass through unchanged; got %q", got)
		}
	})

	t.Run("keeps the daemon's message AND the log's end", func(t *testing.T) {
		head := "schema violation: output contract for step \"report\" not met"
		tail := "[vornik-agent] schema finalization: tool phase ended, re-asking tool-free"
		in := head + "\n" + strings.Repeat("[vornik-agent] iteration=n filler line\n", 400) + tail

		got := truncateDetailPreservingEnds(in, 2000)
		if !strings.Contains(got, head) {
			t.Error("the daemon's message was dropped — it is the first thing a reader needs " +
				"and it sits at the head")
		}
		if !strings.Contains(got, tail) {
			t.Error("the END of the container log was dropped — that is where the step-end " +
				"decisions are, and dropping it is the defect this function exists to fix")
		}
		if len(got) > 2000+128 {
			t.Errorf("length %d exceeds the cap plus a small elision notice; the bound is what "+
				"keeps a log blob out of a downstream prompt", len(got))
		}
		if !strings.Contains(got, "elided") {
			t.Error("a truncated detail must say something was removed, or the reader trusts " +
				"a partial log as complete")
		}
	})

	t.Run("degenerate caps do not panic or corrupt", func(t *testing.T) {
		for _, limit := range []int{0, 1, 8, 64} {
			got := truncateDetailPreservingEnds(strings.Repeat("x", 5000), limit)
			if len(got) > limit+128 {
				t.Errorf("limit=%d produced %d bytes", limit, len(got))
			}
		}
	})

	t.Run("no newlines is still bounded", func(t *testing.T) {
		got := truncateDetailPreservingEnds(strings.Repeat("y", 100000), 2000)
		if len(got) > 2000+128 {
			t.Errorf("a single enormous line must still be capped; got %d", len(got))
		}
	})
}

// The step-end lines are the whole point, so assert the realistic case end to
// end: a real-shaped detail must retain a marker placed at the very end.
func TestTruncateDetailPreservingEnds_retainsStepEndEvidence(t *testing.T) {
	marker := "[vornik-agent] no schema finalization: response_format='json_schema' emit_tool='' tool_phase=1"
	detail := "schema violation: role \"analyst\" result.json is missing required keys: [analysis:object]" +
		containerLogSection(strings.Repeat("[vornik-agent] preflight task_id=… iteration=n\n", 300)+marker)

	got := truncateDetailPreservingEnds(detail, 2000)
	if !strings.Contains(got, "no schema finalization") {
		t.Errorf("the diagnostic that explains the failure was truncated away.\n"+
			"This is the exact evidence chain that was unavailable all of 2026-08-20:\n"+
			"stored detail (%d bytes):\n%s", len(got), got)
	}
}
