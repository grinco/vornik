package llmspend

import "testing"

// Regression, 2026-08-16: retried executions erased each other's spend.
//
// The id was `tu_<task>_<step>_<role>`, so a retry of the same step in the same
// role upserted OVER the first execution's row. On a preserved benchmark ledger
// that erased 1,158 of 4,726 (execution, step) rows — 24.5% of all step spend,
// concentrated in retried work, which is the most expensive work there is.
//
// Pre-fix this test fails on the first assertion: both executions produce the
// same id.
func TestStepUsageID_RetryDoesNotOverwriteEarlierExecution(t *testing.T) {
	first := StepUsageID("task_1", "exec_a", "step_3", "coder")
	retry := StepUsageID("task_1", "exec_b", "step_3", "coder")

	if first == retry {
		t.Fatalf("a retry must not collide with the execution it retried; both are %q", first)
	}
}

// The two writers of a step's spend — the agent's per-iteration stream and the
// executor's finalize path — must land on ONE row, or every step is billed
// twice. That is the whole reason the id is deterministic.
func TestStepUsageID_BothWritersCollideWithinOneExecution(t *testing.T) {
	streamed := StepUsageID("task_1", "exec_a", "step_3", "coder")
	finalized := StepUsageID("task_1", "exec_a", "step_3", "coder")

	if streamed != finalized {
		t.Fatalf("stream and finalize must upsert the same row: %q vs %q", streamed, finalized)
	}
}

// The dispatcher path has no execution row. Recording under the legacy shape
// beats dropping the row: a merged row understates which attempt spent the
// money, a missing row understates the bill.
func TestStepUsageID_NoExecutionKeepsLegacyShape(t *testing.T) {
	got := StepUsageID("task_1", "", "step_3", "coder")
	want := "tu_task_1_step_3_coder"

	if got != want {
		t.Fatalf("StepUsageID without an execution = %q, want the legacy %q", got, want)
	}
}

// Each component has to participate in the identity, or steps/roles that differ
// only in the omitted field would share a row.
func TestStepUsageID_EveryComponentSeparatesRows(t *testing.T) {
	base := StepUsageID("task_1", "exec_a", "step_3", "coder")
	cases := map[string]string{
		"task":      StepUsageID("task_2", "exec_a", "step_3", "coder"),
		"execution": StepUsageID("task_1", "exec_b", "step_3", "coder"),
		"step":      StepUsageID("task_1", "exec_a", "step_4", "coder"),
		"role":      StepUsageID("task_1", "exec_a", "step_3", "tester"),
	}
	for field, got := range cases {
		if got == base {
			t.Errorf("changing %s must change the id; both are %q", field, got)
		}
	}
}
