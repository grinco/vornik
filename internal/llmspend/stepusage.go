package llmspend

// Step usage identity.
//
// A workflow step's spend reaches the ledger from TWO writers that must land on
// the SAME row: the agent container streams cumulative usage after every LLM
// iteration (so a force-killed step still shows its cost), and the executor
// writes the final figures from result.json when the step finalizes. Both use a
// deterministic id and Postgres' ON CONFLICT (id) DO UPDATE, so the finalize
// write — which runs last — wins.
//
// THE BUG THIS FILE EXISTS TO FIX (2026-08-16). The id used to be
// `tu_<task>_<step>_<role>`, which does not name the EXECUTION. A task that
// retries runs the same step, in the same role, under a NEW execution — and its
// upsert therefore overwrote the previous execution's row instead of adding one.
// The earlier execution's spend was not merely misattributed, it was ERASED, and
// the row's execution_id was rewritten to the retry's.
//
// Measured on a preserved benchmark ledger: 1,171 of 4,726 (execution, step)
// pairs had no usage row at all, and 1,158 of those — 98.9% — had a row for the
// same (task, step, role) under a DIFFERENT execution. That is this collision,
// not a recording gap.
//
// It undercounts exactly the work that costs the most (retried work), in the
// ledger that feeds budget enforcement, the spend dashboard and per-key
// attribution. So it is a production billing defect, not a benchmark artifact.
//
// Including the execution makes each execution's spend its own row, which is
// what "cumulative for this attempt" already meant everywhere else: the stream
// reports cumulative totals WITHIN one execution, and a retry is a different
// execution with its own totals.

// StepUsageID returns the deterministic ledger id for one step's LLM spend.
//
// Both writers MUST derive the id here rather than formatting it themselves.
// The agent container sends its own `usage_id`, but the daemon re-derives from
// the request's validated task/execution/step/role instead of trusting it —
// otherwise fixing one writer would split the collision into two rows per step
// and double-count, and the container image would have to be redeployed in
// lockstep with the daemon.
//
// The legacy shape is kept when there is no execution to name. The dispatcher
// path has no execution row, and a run that cannot identify its execution is
// better recorded under a colliding id than dropped: a merged row understates
// which attempt spent the money, while no row understates the bill.
func StepUsageID(taskID, executionID, stepID, role string) string {
	if executionID == "" {
		return "tu_" + taskID + "_" + stepID + "_" + role
	}
	return "tu_" + taskID + "_" + executionID + "_" + stepID + "_" + role
}
