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

import "sort"

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

	// PromptTokenBudget — agent loop stopped because the projected
	// cumulative prompt-token replay for the step would exceed the
	// configured per-step ceiling. The step exits cleanly with a
	// tool-free finalization answer, so workflow control flow can
	// continue while dashboards still distinguish the quality signal
	// from a fully unconstrained OK.
	PromptTokenBudget Outcome = "prompt_token_budget"

	// Superseded — set on outcomes when an operator retries an
	// execution from an earlier step. The retry produces fresh
	// outcomes for the re-run steps; the original ones get this
	// label so dashboards can exclude them from quality stats
	// without losing the audit trail. Never produced by the agent
	// or executor directly — only by the retry-from-step API.
	Superseded Outcome = "superseded"

	// ParallelJoin — observability signal emitted by the parent on the
	// proceed-true resume of a declarative `parallel` fan-out step. It is
	// a direct terminal outcome (not the two-phase pending_validation path)
	// keyed on the parallel step's id; ErrorDetail carries the JSON
	// {policy, succeeded, total}. Emitted ONLY on proceed-true — a
	// proceed-false join (quorum/best_effort not met) is observed via the
	// task's last_error and the child tasks' own FAILED outcomes, never a
	// parallel_join row (deliberate non-emission). See
	// https://docs.vornik.io §6.
	ParallelJoin Outcome = "parallel_join"

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
	// ClassUnclassified — the refiner recognised nothing. THE RESIDUAL, and
	// the name says so: it was called container_non_zero_exit until
	// 2026-08-26, which named the mechanism by which the step arrived here
	// rather than any cause, and read like a diagnosis while carrying none.
	//
	// It is a sentinel for ABSENCE and is load-bearing as one:
	// timeoutOutcomeAndClass compares against it to mean "nothing recognised".
	// Its only writer is the default arm of refineAgentFailureOutcome.
	//
	// Measured before the wave-2 arms landed it was 3,027 of 5,791 classified
	// step failures — 52.3% — of which only 12.4% was genuinely unrecognisable.
	// A classifier whose modal output is "unknown" is the finding, not the
	// baseline: treat the size of this bucket as a metric.
	ClassUnclassified         = "unclassified"
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
	// ClassPromptTokenBudget — agent self-finalized because the next
	// request would have exceeded the configured cumulative prompt-token
	// ceiling for the step.
	ClassPromptTokenBudget = "prompt_token_budget"
	// ClassContextOverflow — the agent could not fit the conversation into
	// the model's window, after exhausting its in-container overflow
	// rescues. Distinct from ClassContextTimeout (wall clock ran out) and
	// from ClassPromptTokenBudget (a self-imposed ceiling, not the model's).
	// 14 of the 2026-08-16 long-horizon arm's 73 failures, concentrated in
	// the analyst role.
	ClassContextOverflow = "context_overflow"
	// ClassPlausibilityViolation — a gate-mode plausibility rule fired on
	// an otherwise schema-valid result.json. Kept apart from the generic
	// schema classes because the fix differs: the shape was right and the
	// content was not. The single largest failure cause in the 2026-08-16
	// long-horizon arm (32 of 73).
	ClassPlausibilityViolation = "plausibility_violation"
	// ClassEmptyDelegation — a step that pins delegated_workflow finished a
	// fresh pass with zero delegatedTasks, so no subtasks were scheduled.
	// Paired with SchemaViolation (empty required collection). Without the
	// guard the step would silently advance and a downstream consumer would
	// fail on an empty diff. Incident task_20260709102613_79c570a868fefedb.
	ClassEmptyDelegation = "empty_delegation"

	// --- wave-2 classes: what the residual bucket was actually made of ---
	//
	// Each was measured over the whole bucket on the production database 2026-08-26. They
	// are not speculative categories; they are the shapes that were already
	// there with nowhere to go.

	// ClassLLMCallFailed — the agent's call to the model provider failed:
	// transport, gateway, or an upstream error. 1,374 rows, 45.4% of the
	// residual and its single largest slice, 175 of them in the trailing 30
	// days. The fleet's most common failure was an upstream provider error
	// with no step class, hidden inside a container-shaped name.
	//
	// Deliberately ONE class, not one per provider: the provider and status
	// live in error_detail, and splitting the class per provider would mirror
	// a registry we do not own.
	ClassLLMCallFailed = "llm_call_failed"
	// ClassMissingPrerequisite — the agent stopped because an input it needed
	// was absent (typically a file an upstream step was meant to produce).
	// 183 rows. Distinct from missing_declared_output, which catches the
	// PRODUCER lying; this is the consumer finding nothing.
	ClassMissingPrerequisite = "missing_prerequisite"
	// ClassContainerKilled — the container was killed by a signal, most often
	// the OOM killer. 47 rows.
	//
	// PRECEDENCE: these rows are a strict SUBSET of ClassContainerWaitFailed's
	// (the literal reads "podman wait failed: signal: killed"), so this must be
	// matched FIRST or every kill disappears into the generic wait bucket.
	// Pinned by test, not by comment.
	ClassContainerKilled = "container_killed"
	// ClassContainerWaitFailed — the runtime failed while waiting for the
	// container to finish. 107 rows once the killed subset above is taken out
	// of the 154 that match the literal.
	ClassContainerWaitFailed = "container_wait_failed"
	// ClassContainerStartFailed — the container never started: image, mount or
	// runtime configuration. 61 rows. Sub-second and deterministic, which is
	// the signature of a fallback rung that has never once worked.
	ClassContainerStartFailed = "container_start_failed"

	// ClassModelUnhealthy is a circuit-open fast-reject: the (route, model)
	// breaker is OPEN, so the call never reached the model — and never started
	// a container, which is why these rows carry no container_exit_code and
	// complete in ~4ms.
	//
	// Named because it was 85.6% of the unclassified population (387 of 452
	// measured 2026-09-02). A typed, daemon-generated condition with a stable
	// prefix has no business in the catch-all: it made the doctor's
	// unclassified-share check largely a measure of one nameable thing, and
	// left an operator with no surface saying a breaker had been open for three
	// days.
	//
	// NOT a terminal failure class, deliberately. An open circuit is permanent
	// only WHILE open, and MODEL_UNHEALTHY is already a model-fallback trigger —
	// the fallback hop is what carries the traffic. See
	// https://docs.vornik.io
	ClassModelUnhealthy = "model_unhealthy"
)

// IsTerminal reports whether an outcome value is final — i.e., not
// PendingValidation. Used by the terminal-state sweep to decide which
// rows still need finalization.
func (o Outcome) IsTerminal() bool {
	return o != PendingValidation && o != ""
}

// String returns the outcome's string form. Safe to call on zero values.
func (o Outcome) String() string { return string(o) }

// errorClasses is the closed set of error-class values this package declares.
//
// Kept beside the constants so the two cannot drift: anything consuming the
// vocabulary at runtime (workflow `retry.on` validation) reads this rather
// than keeping its own copy. internal/playbook/vocabulary_test.go derives the
// same set independently via go/ast and fails the build when a declared
// constant lacks a playbook entry, so completeness is enforced by test rather
// than by remembering to update a list.
var errorClasses = []string{
	ClassUnclassified,
	ClassContainerFAILEDState,
	ClassParseInvalidJSON,
	ClassParsePlanNoSteps,
	ClassParsePlanRefused,
	ClassGateInvalidJSON,
	ClassGateEvalFailed,
	ClassIterationCap,
	ClassDegenerateLoop,
	ClassVerifyFailed,
	ClassMissingOutput,
	ClassContextCancelled,
	ClassContextTimeout,
	ClassHallucinated,
	ClassBudgetTripwire,
	ClassPromptTokenBudget,
	ClassContextOverflow,
	ClassPlausibilityViolation,
	ClassEmptyDelegation,
	ClassLLMCallFailed,
	ClassMissingPrerequisite,
	ClassContainerKilled,
	ClassContainerWaitFailed,
	ClassContainerStartFailed,
}

// ErrorClasses returns every declared error class, sorted, for operator-facing
// messages. Returns a copy so a caller cannot mutate the vocabulary.
func ErrorClasses() []string {
	out := make([]string, len(errorClasses))
	copy(out, errorClasses)
	sort.Strings(out)
	return out
}

// IsErrorClass reports whether s is a declared error class.
func IsErrorClass(s string) bool {
	for _, c := range errorClasses {
		if c == s {
			return true
		}
	}
	return false
}
