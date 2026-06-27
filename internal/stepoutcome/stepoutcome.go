// Package stepoutcome defines the taxonomy of per-step outcomes used by
// the executor to classify *output usability*, distinct from the container
// exit code that drives task_llm_usage.
//
// The problem it addresses: the executor treats container exit-code 0 +
// result.json.status != "FAILED" as step success. But a step whose output
// can't be parsed by the next step is not a success from the workflow's
// point of view — it's a quality failure of the producer model. The plain
// success/failed labels on agent_step_outcomes_total can't distinguish
// "LLM returned something" from "LLM returned something usable". These
// constants are the richer vocabulary the executor writes into the
// execution_step_outcomes table so dashboards can measure
// cost-per-usable-output per (role, model) rather than
// cost-per-LLM-roundtrip.
package stepoutcome

// Outcome is the string form stored in execution_step_outcomes.outcome.
// Kept as a typed string rather than an int enum so DB values remain
// human-readable in ad-hoc queries.
type Outcome string

const (
	// OK — step produced output and the downstream consumer accepted it.
	// For terminal steps with no consumer, the terminal-state sweep
	// finalizes pending_validation to OK when the execution completed.
	OK Outcome = "ok"

	// PendingValidation — step finished, but whether its output is
	// usable is unknown until the next step tries to consume it. Never
	// the final state: either the consumer upgrades it to OK / error
	// class, or the terminal sweep finalizes it on execution close.
	PendingValidation Outcome = "pending_validation"

	// ParseError — the consumer tried to parse the producer's output
	// (typically JSON) and couldn't. Producer blamed. Example: plan
	// step's lead agent returns malformed JSON; parsePlanSteps fails.
	ParseError Outcome = "parse_error"

	// SchemaViolation — output parsed syntactically but was missing
	// required fields or contained an empty required collection (e.g.
	// a lead plan with zero steps). Producer blamed.
	SchemaViolation Outcome = "schema_violation"

	// Refused — the model explicitly declined to produce the requested
	// output (e.g. "lead agent refused to plan: <reason>"). Producer
	// blamed. Distinct from parse errors because the model made a
	// deliberate choice rather than returning garbage.
	Refused Outcome = "refused"

	// IterationExhausted — a tool-calling loop ran out of iteration
	// budget with incomplete work. Attributed to the step itself (the
	// model couldn't finish within its budget), not the producer.
	IterationExhausted Outcome = "iteration_exhausted"

	// DegenerateLoop — detected repeated identical tool calls
	// (the agent container's tool audit can trip this at N=3 consecutive
	// identical (tool_name, arguments) calls). The step's own failure.
	DegenerateLoop Outcome = "degenerate_loop"

	// DownstreamRejected — producer output parsed cleanly but the
	// consumer semantically rejected it (e.g., a reviewer gate vetoed
	// the output). Use AttributedToStepID to point at the producer.
	DownstreamRejected Outcome = "downstream_rejected"

	// GateFailed — a gate step's own evaluation failed. Distinct from
	// the upstream ParseError of the producer whose output the gate
	// tried to read: gate_failed means the gate's own logic erred
	// (bad expression, missing field the gate itself defined).
	GateFailed Outcome = "gate_failed"

	// Timeout — step exceeded its configured timeout.
	Timeout Outcome = "timeout"

	// Cancelled — step stopped due to context cancellation (user cancel,
	// parent task shutdown, etc.).
	Cancelled Outcome = "cancelled"

	// Failed — generic terminal failure; reserved for cases where none
	// of the more specific labels apply (e.g. container start failure,
	// non-zero exit with no structured error class).
	Failed Outcome = "failed"

	// BudgetTripwire — agent loop bailed mid-step because the next LLM
	// call would have breached the project's remaining budget. The
	// step exits cleanly with whatever output it had at the bail-out
	// point, not as a container failure: the agent CHOSE to stop, the
	// runtime didn't kill it. Distinct from gate-side budget refusals
	// (which never start the step) because this fires after the step
	// has already begun spending and a per-call check has decided not
	// to spend more. The blame attaches to the step itself — there's
	// no producer to blame, the budget envelope is a system constraint.
	BudgetTripwire Outcome = "budget_tripwire"

	// Superseded — set on outcomes when an operator retries an
	// execution from an earlier step. The retry produces fresh
	// outcomes for the re-run steps; the original ones get this
	// label so dashboards can exclude them from quality stats
	// without losing the audit trail. Never produced by the agent
	// or executor directly — only by the retry-from-step API.
	Superseded Outcome = "superseded"

	// VerifierWarn — advisory verifier violation. Written as a
	// SEPARATE outcome row alongside the producer's primary row
	// (which keeps its normal verdict, typically `ok`). Surfaces
	// the warn-tier violations to the soak panel + post-mortem
	// so operators can see "this step passed but had N warnings"
	// rather than only finding them in journald. The companion
	// row's ErrorDetail carries the joined warning messages.
	VerifierWarn Outcome = "verifier_warn"
)

// Error class tags. Stored in execution_step_outcomes.error_class for
// quick machine-friendly filtering. Human-readable detail goes in the
// separate error_detail column. Keep these short — they're meant for
// dashboard grouping.
const (
	ClassContainerNonZeroExit = "container_non_zero_exit"
	ClassContainerFAILEDState = "container_failed_state"
	ClassParseInvalidJSON     = "parse_invalid_json"
	ClassParsePlanNoSteps     = "parse_plan_no_steps"
	ClassParsePlanRefused     = "parse_plan_refused"
	ClassGateInvalidJSON      = "gate_invalid_json"
	ClassGateEvalFailed       = "gate_eval_failed"
	ClassIterationCap         = "iteration_cap"
	ClassDegenerateLoop       = "degenerate_loop"
	ClassVerifyFailed         = "verify_claims_failed"
	// ClassMissingOutput — the agent declared an outputArtifact in
	// result.json but the file it named isn't on disk. Mirrors
	// VerifyFailed's role-claims-lie logic for the artifact side.
	// Crucial for the producer/consumer pipeline: when a researcher
	// claims to have written scan-<slug>.md but didn't, the writer
	// looks for that file, finds nothing, and loops on file_read —
	// catching it at the producer surfaces the real failure.
	ClassMissingOutput    = "missing_declared_output"
	ClassContextCancelled = "context_cancelled"
	ClassContextTimeout   = "context_timeout"
	// ClassHallucinated — the post-step claim-grounding detector
	// found a High-severity unsupported claim (URL never fetched,
	// task/project ID that doesn't exist, etc.). The step is
	// failed so the scheduler's existing retry path picks it up;
	// the JSONB signals column carries the per-claim detail.
	ClassHallucinated = "hallucinated_claim"
	// ClassBudgetTripwire — agent self-aborted because the next LLM
	// call would have breached the project's remaining budget for the
	// active period (daily or monthly). Detail field carries the
	// estimated next-call cost and remaining envelope.
	ClassBudgetTripwire = "budget_tripwire"
)

// IsTerminal reports whether an outcome value is final — i.e., not
// PendingValidation. Used by the terminal-state sweep to decide which
// rows still need finalization.
func (o Outcome) IsTerminal() bool {
	return o != PendingValidation && o != ""
}

// String returns the outcome's string form. Safe to call on zero values.
func (o Outcome) String() string { return string(o) }
