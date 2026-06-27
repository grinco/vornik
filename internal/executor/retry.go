package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// shapeRetryMaxAttempts caps how many corrective re-runs we'll do on a
// single agent step. 1 means: original attempt + 1 retry. Bumping this
// without thought turns a 14-cases-a-week issue into a denial-of-service
// when a model gets stuck producing the wrong shape.
const shapeRetryMaxAttempts = 1

// shapeRetryLoopCap is the per-execution cap on shape retries before
// the watchdog escalates to terminal INVALID_OUTPUT_LOOP. Counted across
// all daemon restarts (the count comes from execution_step_outcomes in
// the DB, not in-memory). 3 means: a single role's primary attempt +
// shape_retry + model_fallback all failing schema validation will
// terminate the task on the next call instead of looping. Reproduced
// 2026-05-07: dev-swarm lead spent 40+ minutes in a shape-retry loop
// across one daemon restart (exec_20260507215311_ef10f2e6c9ff685f) —
// the cap caps that wall-clock cost regardless of how many restarts
// the operator does.
const shapeRetryLoopCap = 3

// shapeRetryHint is the suffix appended to the step prompt on retry
// when the validation failure was a JSON-shape problem. It names the
// failure and gives explicit instructions to dispense with preamble /
// markdown / prose. Models that emit "Here's the JSON: {...}" or wrap
// the JSON in ```json fences are the target.
const shapeRetryHint = "\n\n[CORRECTION: your previous attempt failed schema validation: %s. " +
	"Respond ONLY with the required JSON object as a top-level value. " +
	"No prose before or after. No markdown code fences. " +
	"No 'Here is the result:' preamble. " +
	"The output must parse with `jq .` and contain every required key.]"

// plausibilityRetryHint is the corrective suffix used when the prior
// output's *shape* was fine but a plausibility rule fired (e.g.
// {approved:true, feedback:""}). Different message from shapeRetryHint
// because the model already produced parseable JSON — the problem is
// that the field VALUES contradicted each other or were unhelpfully
// empty. Pointing at JSON formatting (as shapeRetryHint does) would
// confuse the model; here we tell it the issue is logical
// completeness, not syntax.
const plausibilityRetryHint = "\n\n[CORRECTION: your previous attempt failed plausibility validation: %s. " +
	"Your JSON shape was correct but the field values were inconsistent or empty where they shouldn't be. " +
	"Re-answer with values that match each other (e.g. if approved:false, populate feedback with the specific issue) " +
	"and don't leave required-content fields blank. Same JSON structure as before; just fix the content.]"

// priorAttemptAnchor is appended to the corrective hint when the prior
// attempt produced a substantive message. The agent sees its own prior
// reasoning re-fed and is told to TRANSFORM it into the required JSON
// rather than start from scratch — without this the model frequently
// converges to a "safe" minimal answer (empty arrays, default values)
// that throws away the substantive analysis it had just produced.
//
// Captured behaviour from task_20260504204356_1806f248aafc7f39: the
// risk-officer wrote 2KB of prose approving 3 strategist proposals,
// failed shape validation (no JSON envelope), and on the corrective
// re-run emitted {approved:[],rejected:[]} — losing all 3 approvals.
// The shape-only hint asked it to fix syntax; the model fixed syntax
// by abandoning content. Re-injecting the prior message anchors the
// re-run on the work already done.
const priorAttemptAnchor = "\n\nYour previous attempt's content (re-format it into the JSON shape above; do NOT abandon prior decisions or substantive findings):\n<<<PRIOR_ATTEMPT\n%s\n>>>PRIOR_ATTEMPT"

// priorAttemptMaxChars caps how much of the prior message is fed back
// into the corrective hint. Long enough to preserve a typical risk-
// officer reasoning chain; short enough to keep the next-attempt
// prompt under the gateway's request size envelope.
const priorAttemptMaxChars = 4000

// extractPriorMessage pulls the prior attempt's stripped `message`
// field out of the agent's result.json bytes. Returns "" if the
// bytes don't parse as JSON or the message is missing/short — the
// corrective hint should only re-anchor when there's substantive
// content to re-format. Matches the same extraction the workflow
// loop does for downstream-step handover, so the model gets the
// post-strip text it would have seen in a successful next step.
func extractPriorMessage(result []byte) string {
	if len(result) == 0 {
		return ""
	}
	var r struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	msg := strings.TrimSpace(stripReasoning(r.Message))
	if len(msg) < 50 {
		// Below this threshold the message is either empty,
		// "FAILED", or a one-line error string — re-feeding it
		// adds noise without anchoring on real content.
		return ""
	}
	if len(msg) > priorAttemptMaxChars {
		msg = msg[:priorAttemptMaxChars] + "\n…(truncated)"
	}
	return msg
}

// infraRetryMaxAttempts is the cap on how many times we'll re-run a
// step that hit an infrastructure failure pattern (curl can't reach
// the chat proxy, DNS lookup failed, gateway 5xx, container OOM
// killed before result.json was written, etc). Counts the original
// attempt PLUS retries — so 6 means: try, retry 1 … retry 5.
//
// The cap exists because some "infra" patterns are actually
// permanent misconfiguration (wrong endpoint, auth missing, model
// IDs the gateway doesn't know about). Without a cap we'd spin
// indefinitely on those and burn the daemon's process clock.
//
// 6 attempts + the timing below give a ~50s sleep budget. That
// covers the daemon-startup race condition where the scheduler
// dispatches a queued task before the chat proxy has finished
// binding port 8080. The pre-2026.5.5 budget (4 attempts × 8s cap
// ≈ 10s) wasn't enough — operators reported "curl exit 7, connect
// refused after 0ms" failures the first tick after every
// `systemctl restart vornik`. Permanent misconfigurations still
// fail within ~50s wall clock.
const infraRetryMaxAttempts = 6

// infraRetryTimeoutAttempts is the (lower) cap that applies specifically to
// TIMEOUT-class infra failures — curl exit 28, i/o timeout, context deadline.
// Unlike connect-refused / DNS / gateway-5xx failures (which fail in
// milliseconds and recover within seconds, so the full infraRetryMaxAttempts
// budget is cheap and worthwhile), a timeout failure burns the ENTIRE per-call
// VORNIK_LLM_TIMEOUT (~120s) before it returns. A model that times out once is
// likely to keep timing out — it is structurally too slow for the step's
// budget — and spending all 6 attempts on it costs ~13min of wall clock before
// the persistent-timeout model fallback (isPersistentTimeoutFailure) can engage.
//
// 2 keeps one confirming retry (a single 120s timeout could be a one-off
// upstream hiccup) while capping the wasted budget at ~4min, so the fallback to
// a faster independent model fires ~3× sooner. Set during the 2026-06-24
// zai.glm-5 latency-tail investigation: glm-5's Bedrock tail occasionally
// exceeds 120s uncorrelated with request size, and the reviewer (many
// sequential glm-5 calls) is the canary.
const infraRetryTimeoutAttempts = 2

// infraRetryBaseDelay is the first sleep before the infra retry's
// second attempt. Subsequent retries double the wait — exponential
// backoff bounded by infraRetryMaxDelay so we don't schedule an
// over-budget sleep on a step that has 90 seconds of wall-clock
// budget left.
//
// Sleep schedule with the 2026.5.5 numbers below:
//
//	attempt 1:   fire immediately
//	attempt 2:   +1.5s
//	attempt 3:   +3s
//	attempt 4:   +6s
//	attempt 5:   +12s
//	attempt 6:   +20s (capped)
//	total wall: ~42.5s
//
// The cap was raised 8s → 20s in 2026.5.5 because chat-proxy
// warmup on busy hosts (heavy compose stack, IBGateway warming up
// in parallel) can take longer than 8s post-restart.
// vars (not consts) so tests can zero the backoff and exercise the
// 6-attempt exhaustion path without ~42s of real sleeping. Production
// keeps the values below.
var (
	infraRetryBaseDelay = 1500 * time.Millisecond
	infraRetryMaxDelay  = 20 * time.Second
)

// executeAgentStepWithFallback wraps executeAgentStepWithShapeRetry
// with a per-role model-fallback layer. When the role declares a
// `modelFallback:` model and the primary attempt (including shape
// retry) failed with a "model-shaped" error class, the executor
// re-runs the step on the fallback model exactly once. The retry
// uses a distinct step ID (stepID_fallback) so the audit log shows
// the fallback as its own attempt.
//
// "Model-shaped" failures are the classes where a different model
// has a real chance of producing a usable result with the same
// inputs:
//   - shape failures (schema, plausibility, plan parse) — the
//     primary couldn't produce valid output;
//   - tool-iteration limits — the primary's reasoning chain didn't
//     converge before the cap;
//   - LLM provider errors that survived the infra-retry layer
//     (gateway exhausted retries, persistent 5xx).
//   - PERSISTENT timeouts: a timeout that exhausted the entire
//     infra-retry budget (isPersistentTimeoutFailure). A single
//     transient timeout is absorbed by the infra-retry layer without
//     a model swap, but one that survives all 6 attempts means the
//     primary model is too slow for the per-call budget (e.g.
//     zai.glm-* at 247-564s vs a 120s VORNIK_LLM_TIMEOUT) — a faster,
//     independent fallback is worth one shot. (2026-06-24: this is the
//     class that previously looped 6× and failed without ever trying
//     the configured fallback.)
//
// Failures outside that set (container crashes, content verification
// rejections, mtime-floor violations, and SINGLE transient timeouts
// the infra layer already absorbs) are not retried on a different
// model — the issue isn't model choice, and a fallback would just
// burn money.
func (e *Executor) executeAgentStepWithFallback(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	stepID string,
	step registry.WorkflowStep,
	timeout time.Duration,
	opts *agentInputOpts,
	roleConfig *registry.SwarmRole,
) (string, []byte, error) {
	cid, result, err := e.executeAgentStepWithShapeRetry(ctx, task, execution, plan, stepID, step, timeout, opts)
	if err == nil {
		return cid, result, nil
	}
	if roleConfig == nil || strings.TrimSpace(roleConfig.ModelFallback) == "" {
		return cid, result, err
	}
	if !isModelShapedFailure(err) && !isPersistentTimeoutFailure(err) {
		return cid, result, err
	}

	// Clone the role config and swap in the fallback model. The shape
	// retry layer reads the model from step + role at agent-input
	// build time, so editing the role copy here flows through.
	fallbackRole := *roleConfig
	fallbackRole.Model = roleConfig.ModelFallback
	fallbackRole.ModelFallback = "" // prevent recursive fallback if the
	// primary's fallback also defines one — one fallback is the
	// budget; if the secondary fails too the step terminates.

	// Inject the swapped role into the plan's swarm so executeAgentStep
	// picks it up via the same role lookup it always does. Cloning the
	// swarm avoids stomping on a parallel sibling step's view of the
	// role catalogue. Roles is a slice; replace the matching entry.
	fallbackSwarm := *plan.swarm
	clonedRoles := make([]registry.SwarmRole, len(plan.swarm.Roles))
	copy(clonedRoles, plan.swarm.Roles)
	for i := range clonedRoles {
		if clonedRoles[i].Name == roleConfig.Name {
			clonedRoles[i] = fallbackRole
			break
		}
	}
	fallbackSwarm.Roles = clonedRoles
	fallbackPlan := *plan
	fallbackPlan.swarm = &fallbackSwarm

	fallbackStepID := stepID + "_model_fallback"
	if e.metrics != nil {
		e.metrics.RecordModelFallback(step.Role, roleConfig.Model, roleConfig.ModelFallback)
	}
	e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Str("retry_step", fallbackStepID).
		Str("role", step.Role).
		Str("primary_model", roleConfig.Model).
		Str("fallback_model", roleConfig.ModelFallback).
		Str("error", truncateForPrompt(err.Error(), 200)).
		Msg("model fallback: re-running step with secondary model")

	cid2, result2, err2 := e.executeAgentStepWithShapeRetry(ctx, task, execution, &fallbackPlan, fallbackStepID, step, timeout, opts)
	if err2 == nil {
		e.logger.Info().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Str("primary_model", roleConfig.Model).
			Str("fallback_model", roleConfig.ModelFallback).
			Msg("model fallback: step recovered on secondary model")
		return cid2, result2, nil
	}
	// Both models failed. Surface the fallback's error since it's
	// the most-recent state; thread the primary error in the message
	// so operators see the full chain.
	return cid2, result2, fmt.Errorf("%w (primary model %q also failed: %s)",
		err2, roleConfig.Model, truncateForPrompt(err.Error(), 200))
}

// isModelShapedFailure returns true when the error class is one
// where a different model is plausibly worth trying. Currently:
// shape failures (schema, plausibility, plan parse), tool-iteration
// limits, and the persistent LLM/PROVIDER_ERROR class.
func isModelShapedFailure(err error) bool {
	if err == nil {
		return false
	}
	if classifyShapeFailure(err) != shapeFailureNone {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "Tool iteration limit") || strings.Contains(msg, "tool iteration cap") {
		return true
	}
	if strings.Contains(msg, "PROVIDER_ERROR") {
		return true
	}
	return false
}

// isPersistentTimeoutFailure reports whether err is an infra-retry-EXHAUSTED
// failure whose underlying cause was a timeout — i.e. the model/endpoint never
// produced a response within the per-call budget across every infra-retry
// attempt. This is distinct from a single transient timeout (which the
// infra-retry layer absorbs without a model swap): a timeout that survives all
// attempts means the primary model is structurally too slow for the step's
// VORNIK_LLM_TIMEOUT (the 2026-06-24 zai.glm-5 case — observed 247-564s on
// Bedrock vs a 120s budget — which looped 6× and failed without ever trying
// the configured fallback). A faster, independent fallback model is then worth
// exactly one shot, so this is a model-fallback trigger.
//
// Gated on the "infra retry exhausted" wrapper (added by
// executeAgentStepWithInfraRetry) so a one-off timeout that recovered, or a
// timeout on a non-infra path, does NOT spuriously swap models.
func isPersistentTimeoutFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "infra retry exhausted") {
		return false
	}
	lowered := strings.ToLower(msg)
	// curl exit 28 ("(28)"), Go context deadlines, and generic gateway
	// "timed out"/"timeout" all denote the model not responding in time.
	return containsAny(lowered, "timed out", "timeout", "deadline", "(28)", "no bytes received")
}

// isTimeoutInfraFailure reports whether a SINGLE infra failure (one attempt's
// error, before any "infra retry exhausted" wrapper) is timeout-class. The
// caller has already confirmed isInfraFailure(err); this narrows that to the
// subset that costs a full per-call timeout, so the infra-retry loop can cap
// timeout-class failures at infraRetryTimeoutAttempts instead of the full
// infraRetryMaxAttempts. Same timeout vocabulary as isPersistentTimeoutFailure
// but WITHOUT the exhausted-wrapper gate, since here we classify per attempt.
func isTimeoutInfraFailure(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(err.Error())
	return containsAny(lowered, "timed out", "timeout", "deadline", "(28)", "no bytes received")
}

// executeAgentStepWithShapeRetry runs an agent step with two layers
// of self-healing wrapped around the inner executeAgentStep:
//
//  1. Infra retry (this layer): the agent's call to the chat proxy /
//     bedrock gateway / vertex / etc fails with a transient network
//     pattern (connect refused, DNS, gateway 5xx, EOF mid-stream,
//     SIGKILL of the container). Up to infraRetryMaxAttempts attempts
//     with exponential backoff, no prompt change between attempts —
//     the inputs are the same so we just re-run the call and let the
//     transient unwind.
//
//  2. Shape retry (innermost): if all infra attempts succeeded but
//     the result is malformed JSON or missing required keys, ONE
//     corrective re-run with a prompt suffix telling the model to
//     return only valid JSON.
//
// Non-shape, non-infra failures (content verification, agent reported
// FAILED with a content-class reason) are NOT retried — those
// reflect logic problems that won't be fixed by re-prompting the
// same model with the same inputs. Returning the original error
// preserves the existing classification path so task-level retry
// (with a different model or after operator intervention) can decide.
//
// Each retry uses a distinct step ID (stepID_infra_retryN /
// stepID_shape_retry) so the per-step audit and outcome rows stay
// unambiguous — operators see every attempt in the UI.
func (e *Executor) executeAgentStepWithShapeRetry(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	stepID string,
	step registry.WorkflowStep,
	timeout time.Duration,
	opts *agentInputOpts,
) (string, []byte, error) {
	cid, result, err := e.executeAgentStepWithInfraRetry(ctx, task, execution, plan, stepID, step, timeout, opts)
	if err == nil {
		return cid, result, nil
	}
	kind := classifyShapeFailure(err)
	if kind == shapeFailureNone {
		return cid, result, err
	}
	if shapeRetryMaxAttempts <= 0 {
		return cid, result, err
	}

	// Loop guard: count prior shape-failure outcomes for THIS execution
	// across all step IDs (original + _shape_retry + _model_fallback).
	// Persists across daemon restarts because step_outcomes is in the
	// DB. Once the cap is hit, escalate to terminal
	// INVALID_OUTPUT_LOOP instead of firing another shape retry — the
	// model is structurally unable to follow the schema and burning
	// more attempts produces no signal beyond the wall-clock cost.
	if e.priorShapeFailureCount(ctx, execution.ID) >= shapeRetryLoopCap {
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("step", stepID).
			Str("role", step.Role).
			Int("cap", shapeRetryLoopCap).
			Str("error", truncateForPrompt(err.Error(), 200)).
			Msg("shape retry loop cap hit — escalating to terminal INVALID_OUTPUT_LOOP")
		return cid, result, fmt.Errorf("shape retry loop cap hit (%d prior shape failures): %w", shapeRetryLoopCap, err)
	}

	// Resolve role+model for metrics. Lookup is best-effort; an
	// unknown role here means the failure happened before role
	// resolution, in which case skipping metrics is correct (no
	// model to attribute the shape retry to).
	primaryModel := ""
	var roleForHint *registry.SwarmRole
	if roleConfig, lookupErr := findSwarmRole(plan.swarm, step.Role); lookupErr == nil {
		primaryModel = e.effectiveRoleModelForTask(task, roleConfig)
		roleForHint = roleConfig
	}
	metricKind := shapeFailureMetricKind(err, kind)
	if e.metrics != nil {
		e.metrics.RecordShapeRetry(step.Role, primaryModel, metricKind)
		// Item 10: per-role outcome counter for rescue-rate
		// visibility. The "attempted" tick fires whenever the
		// executor decides to run a retry; recovered / failed
		// land on the result of the retry call below.
		e.metrics.RecordShapeRetryOutcome(step.Role, "attempted")
	}

	// Build corrective opts. Don't mutate the caller's struct; clone.
	// Pick the hint flavor that matches the failure kind so the
	// model gets a useful nudge — telling a model with valid JSON
	// to "respond only with valid JSON" mis-frames the problem
	// (and frequently causes a regression on the second attempt).
	// Item 10 enriched this with structured "Missing keys:" and
	// "Required schema:" clauses so the model sees exactly what
	// failed without having to parse the truncated error string.
	retryOpts := *opts
	hint := buildShapeRetryHint(err, kind, result, opts.ShapeRetryHint, roleForHint)
	retryOpts.StepPrompt = retryOpts.StepPrompt + hint
	priorMsg := extractPriorMessage(result)

	retryStepID := stepID + "_shape_retry"
	e.logger.Warn().
		Str("execution_id", execution.ID).
		Str("step", stepID).
		Str("retry_step", retryStepID).
		Str("role", step.Role).
		Bool("prior_message_anchored", priorMsg != "").
		Int("prior_message_chars", len(priorMsg)).
		Str("error", truncateForPrompt(err.Error(), 200)).
		Msg("shape retry: re-running step with corrective prompt")

	cid2, result2, err2 := e.executeAgentStepWithInfraRetry(ctx, task, execution, plan, retryStepID, step, timeout, &retryOpts)
	if err2 == nil {
		// Shape retry recovered — count it as salvaged so the
		// per-model recovered/total ratio reflects which models
		// respond well to corrective prompting.
		if e.metrics != nil {
			e.metrics.RecordShapeRetryRecovered(step.Role, primaryModel, metricKind)
			e.metrics.RecordShapeRetryOutcome(step.Role, "recovered")
		}
		return cid2, result2, nil
	}
	if e.metrics != nil {
		e.metrics.RecordShapeRetryOutcome(step.Role, "failed")
	}
	// Both attempts failed. Surface the second error — it reflects the
	// most-recent state and includes whatever new failure mode the
	// retry produced. Keep the original message threaded for clarity.
	return cid2, result2, fmt.Errorf("%w (original error before retry: %s)", err2, truncateForPrompt(err.Error(), 200))
}

// shapeFailureMetricKind maps a (err, classifyShapeFailure result) pair
// to the low-cardinality "kind" label used by ShapeRetryTotal /
// ShapeRetryRecoveredTotal. Pulls from the same string predicates as
// classifyStepOutcome so a single failure produces consistent labels
// across both the step-outcome and shape-retry metrics.
func shapeFailureMetricKind(err error, kind shapeFailureKind) string {
	if err == nil {
		return "shape_failure"
	}
	msg := err.Error()
	switch {
	case kind == shapeFailurePlausibility:
		return "plausibility"
	case strings.Contains(msg, "schema violation:") || strings.Contains(msg, "is missing required keys"):
		return "schema_violation"
	case strings.Contains(msg, "could not parse plan from") || strings.Contains(msg, "invalid JSON") || strings.Contains(msg, "invalid character"):
		return "parse_error"
	default:
		return "shape_failure"
	}
}

// executeAgentStepWithInfraRetry retries the inner executeAgentStep
// on transient infrastructure failures (connection refused, DNS,
// gateway 5xx, container OOM-killed before writing result.json).
// Same prompt and inputs each attempt — we're not trying to coax
// different content out of the model, just waiting out a transient
// network condition or backend hiccup.
//
// Backoff: infraRetryBaseDelay doubles per attempt, capped at
// infraRetryMaxDelay. Honours ctx cancellation between attempts so
// a graceful shutdown doesn't sleep for 8 seconds before unwinding.
func (e *Executor) executeAgentStepWithInfraRetry(
	ctx context.Context,
	task *persistence.Task,
	execution *persistence.Execution,
	plan *executionPlan,
	stepID string,
	step registry.WorkflowStep,
	timeout time.Duration,
	opts *agentInputOpts,
) (string, []byte, error) {
	delay := infraRetryBaseDelay
	var lastCID string
	var lastResult []byte
	var lastErr error
	timeoutFailures := 0
	attemptsMade := 0

	for attempt := 1; attempt <= infraRetryMaxAttempts; attempt++ {
		attemptsMade = attempt
		stepIDForAttempt := stepID
		if attempt > 1 {
			// Per-attempt step ID so the audit log and step-outcome
			// rows distinguish the retries from the original. The
			// UI's timeline picks them up as separate segments.
			stepIDForAttempt = fmt.Sprintf("%s_infra_retry%d", stepID, attempt-1)
		}

		cid, result, err := e.executeAgentStep(ctx, task, execution, plan, stepIDForAttempt, step, timeout, opts)
		if err == nil {
			if attempt > 1 {
				e.logger.Info().
					Str("execution_id", execution.ID).
					Str("step", stepID).
					Int("attempt", attempt).
					Msg("infra retry: step recovered after transient failure")
			}
			return cid, result, nil
		}
		lastCID, lastResult, lastErr = cid, result, err

		if !isInfraFailure(err) {
			// Non-infra error — kick it back to the caller (shape
			// retry layer or the workflow loop) without burning more
			// retry budget on a problem that won't fix itself.
			return cid, result, err
		}
		// Timeout-class failures each cost a full per-call timeout (~120s)
		// and rarely recover on retry, so cap them well below the general
		// infra-retry budget. Hitting the cap exhausts the loop early so the
		// persistent-timeout model fallback (isPersistentTimeoutFailure) can
		// engage on a faster model instead of burning ~13min on a slow one.
		if isTimeoutInfraFailure(err) {
			timeoutFailures++
			if timeoutFailures >= infraRetryTimeoutAttempts {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Str("step", stepIDForAttempt).
					Int("attempt", attempt).
					Int("timeout_failures", timeoutFailures).
					Int("timeout_budget", infraRetryTimeoutAttempts).
					Msg("infra retry: timeout budget reached, exhausting early so model fallback can engage")
				break
			}
		}
		if attempt == infraRetryMaxAttempts {
			break
		}

		// Backoff with ctx-aware sleep. Cap delay so an exponential
		// run-up doesn't blow past the step's remaining timeout.
		if delay > infraRetryMaxDelay {
			delay = infraRetryMaxDelay
		}
		e.logger.Warn().
			Str("execution_id", execution.ID).
			Str("step", stepIDForAttempt).
			Int("attempt", attempt).
			Int("max", infraRetryMaxAttempts).
			Dur("backoff", delay).
			Str("error", truncateForPrompt(err.Error(), 200)).
			Msg("infra retry: transient failure detected, retrying after backoff")

		select {
		case <-ctx.Done():
			return cid, result, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	// All attempts exhausted on the infra retry budget. Return the
	// last error so the caller sees the most recent failure detail.
	return lastCID, lastResult, fmt.Errorf("infra retry exhausted after %d attempts: %w", attemptsMade, lastErr)
}

// infraFailureMarkers are substrings that identify a transient
// infrastructure failure in agent error messages. The list is
// intentionally narrow: each entry corresponds to a real failure
// mode observed in the daemon's failure-class dashboard.
//
// Operators adding new entries should keep them ANCHORED enough not
// to false-positive on legitimate content. "EOF", "connection",
// "timeout" alone are too broad — those words appear in agent prose
// when a model writes about a story involving a connection. The
// curl exit codes and the bracketed tags below are reliably
// machine-emitted.
var infraFailureMarkers = []string{
	// curl exit codes from the agent's chat-proxy call. The agent
	// surfaces these as "curl failed (exit N): curl: (N) ..." in
	// the result.json error path.
	"curl: (6)",  // CURLE_COULDNT_RESOLVE_HOST — DNS
	"curl: (7)",  // CURLE_COULDNT_CONNECT — connection refused
	"curl: (28)", // CURLE_OPERATION_TIMEDOUT
	"curl: (35)", // CURLE_SSL_CONNECT_ERROR
	"curl: (52)", // CURLE_GOT_NOTHING — empty reply / EOF
	"curl: (56)", // CURLE_RECV_ERROR — failure receiving network data
	// Gateway 5xx — bedrock-access-gateway / vertex / claude-sub
	// errors that sometimes recover within seconds.
	"gateway error 502",
	"gateway error 503",
	"gateway error 504",
	"PROVIDER_ERROR",
	// Connection-level errors that show up in agent logs without
	// the curl: prefix (some chat clients call native HTTP, not
	// curl, but report the same kernel-level failure modes).
	"connection refused",
	"Connection refused",
	"no such host",
	"i/o timeout",
}

// isInfraFailure returns true when err looks like a transient
// infrastructure failure that's worth retrying with the same inputs.
//
// Only matches against agent-emitted error patterns (curl exit
// codes, gateway 5xx, connection refused) — i.e. failures that
// happen WHILE the agent container is running and surface via
// "agent reported FAILED status: ..." in the executor error
// path. The existing retryableError wrapper that container.go
// puts on container-start / wait failures is intentionally NOT
// matched here: those are handled by the task-level retry loop in
// runExecution, which uses task.attempts as its budget. Layering
// both a step-level infra retry AND a task-level retry on the
// same error class would double-count attempts.
func isInfraFailure(err error) bool {
	if err == nil {
		return false
	}
	// Don't catch container-start / wait transients — those are
	// task-level retries via markRetryable + shouldRetry.
	var transient retryableError
	if errors.As(err, &transient) {
		return false
	}
	// Match against agent-emitted error messages.
	msg := err.Error()
	for _, marker := range infraFailureMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// shapeFailureKind tags the flavor of validation failure so the
// retry layer can pick the right corrective hint. Plausibility
// failures need a different prompt than JSON-shape failures —
// telling a model that already produced valid JSON to "respond
// only with valid JSON" misleads it about the actual problem.
type shapeFailureKind int

const (
	shapeFailureNone         shapeFailureKind = iota
	shapeFailureJSON                          // bad JSON / missing required keys / unparseable plan
	shapeFailurePlausibility                  // shape OK but field values inconsistent or empty
)

// classifyShapeFailure returns the kind of shape failure (if any)
// represented by err. Returns shapeFailureNone for non-shape errors
// — caller should fall through to the model-fallback / terminal-
// failure path. The marker strings come from the existing failure
// sites in container.go / plan_step.go; if those messages change,
// this matcher needs to follow.
func classifyShapeFailure(err error) shapeFailureKind {
	if err == nil {
		return shapeFailureNone
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "plausibility violation"):
		return shapeFailurePlausibility
	case strings.Contains(msg, "schema violation: role"):
		return shapeFailureJSON
	case strings.Contains(msg, "is missing required keys"):
		return shapeFailureJSON
	case strings.Contains(msg, "could not parse plan from lead output"):
		return shapeFailureJSON
	case strings.Contains(msg, "result.json") &&
		(strings.Contains(msg, "parse") || strings.Contains(msg, "unmarshal")):
		return shapeFailureJSON
	}
	return shapeFailureNone
}

// isShapeFailure preserves the boolean predicate for callers that
// only need the "should we retry at all" signal.
func isShapeFailure(err error) bool {
	return classifyShapeFailure(err) != shapeFailureNone
}

// truncateForPrompt clips a string to the given byte budget so the
// corrective prompt can't accidentally balloon the next request body.
// Adds an ellipsis when truncation occurs so the model sees that the
// message was cut off.
func truncateForPrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// missingKeysRegex matches the missing-keys clause emitted by
// container.go ("schema violation: role %q result.json is missing
// required keys: [a b c]"). The pre-compiled regex keeps the parser
// cheap on the retry hot-path. Captures the bracket contents into
// group 1.
var missingKeysRegex = regexp.MustCompile(`is missing required keys:\s*\[([^\]]*)\]`)

// extractMissingKeysFromError pulls the missing-keys slice back out
// of a schema-violation error message. Mirrors the format emitted by
// container.go's validateRequiredOutputKeys failure site so the
// corrective hint can name the missing keys structurally rather than
// burying them in a truncated prose blob.
//
// Returns nil when err is nil, the error doesn't match the expected
// format, or the captured key list is empty. Callers should fall
// back to including the raw error text in the hint when this
// returns nil — there's still useful signal in the error string,
// just less structured.
func extractMissingKeysFromError(err error) []string {
	if err == nil {
		return nil
	}
	m := missingKeysRegex.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return nil
	}
	inner := strings.TrimSpace(m[1])
	if inner == "" {
		return nil
	}
	// container.go emits the slice via fmt's %v which produces
	// "[a b c]" (space-separated). Some future drift might emit
	// commas; tolerate both so the parser doesn't silently fail
	// the next time the formatter changes.
	parts := strings.FieldsFunc(inner, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildShapeRetryHint composes the corrective-prompt suffix appended
// to the step prompt on a shape-retry attempt. Item 10 of
// https://docs.vornik.io generalised this from
// the lead-specific path to every agent step, AND enriched the hint
// with two structured clauses the pre-fix prose-only template
// lacked:
//
//  1. "Missing keys: [...]" — names the exact keys the validator
//     flagged. Pre-fix the keys were only embedded in the
//     truncated error string and models frequently failed to
//     extract them from the prose.
//  2. "Required schema:" + role.OutputSchema.RenderForPrompt() —
//     re-states the canonical shape so the model doesn't have to
//     recall it from an original prompt that may have been
//     truncated/compacted by intermediate tool calls.
//
// kind picks the base template (shape vs plausibility — the former
// tells the model to fix JSON syntax, the latter tells it the JSON
// was fine and to fix the values). priorResult, when non-empty,
// drives the prior-attempt anchor so the retry can re-format
// substantive prior content rather than abandoning it. roleHint is
// the per-role addendum from swarm YAML.
//
// Returns the hint string ready to concatenate after the step
// prompt. Caller is responsible for the append.
func buildShapeRetryHint(err error, kind shapeFailureKind, priorResult []byte, roleHint string, role *registry.SwarmRole) string {
	template := shapeRetryHint
	if kind == shapeFailurePlausibility {
		template = plausibilityRetryHint
	}
	hint := fmt.Sprintf(template, truncateForPrompt(err.Error(), 400))

	// Structured missing-keys + rendered-schema clauses only apply
	// to JSON-shape failures. Plausibility failures had valid
	// shape — adding a "Missing keys:" clause there would mis-frame
	// the problem and frequently cause a regression on retry (the
	// model "fixes" missing keys that weren't missing, drops the
	// values that were actually wrong).
	if kind == shapeFailureJSON {
		if missing := extractMissingKeysFromError(err); len(missing) > 0 {
			hint = hint + "\n\nMissing keys: [" + strings.Join(missing, ", ") + "]"
		}
		if role != nil && role.OutputSchema != nil {
			if rendered := role.OutputSchema.RenderForPrompt(); rendered != "" {
				hint = hint + "\n\nRequired schema:\n" + rendered
			}
		}
	}

	if priorMsg := extractPriorMessage(priorResult); priorMsg != "" {
		hint = hint + fmt.Sprintf(priorAttemptAnchor, priorMsg)
	}

	// Role-specific addendum from swarm YAML — captures
	// empirical knowledge ("preserve approvals") as data
	// rather than baking it into the generic anchor template.
	if roleHint != "" {
		hint = hint + "\n\n[ROLE GUIDANCE: " + roleHint + "]"
	}
	return hint
}

// priorShapeFailureCount queries the persisted step_outcomes table
// for shape-related failures (schema_violation, parse_error) on a
// given execution. Used by the shape-retry loop guard so a single
// execution that's stuck producing the wrong shape — across daemon
// restarts and across role/step boundaries — caps at
// shapeRetryLoopCap before escalating.
//
// Best-effort: returns 0 on any DB error. The caller's existing
// shape retry path is preserved if the count can't be loaded; the
// loop guard simply doesn't fire. This keeps the watchdog from
// blocking executions when the outcomes table is unreachable.
func (e *Executor) priorShapeFailureCount(ctx context.Context, executionID string) int {
	if e == nil || e.outcomeRepo == nil || executionID == "" {
		return 0
	}
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := e.outcomeRepo.List(queryCtx, persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &executionID,
		PageSize:    50, // bound the scan; an exec with 50+ shape failures is itself the bug
	})
	if err != nil {
		return 0
	}
	count := 0
	for _, row := range rows {
		if row == nil {
			continue
		}
		switch row.Outcome {
		case "schema_violation", "parse_error":
			count++
		}
	}
	return count
}
