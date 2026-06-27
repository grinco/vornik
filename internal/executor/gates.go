package executor

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// classifyGateEvalError maps an evaluateGateStep error to the outcome
// taxonomy. Producer vs gate responsibility depends on the error shape:
//   - "failed to parse gate input as JSON": producer returned malformed
//     JSON — the *producer's* output was unusable. Maps to parse_error.
//   - "no gate condition matched": producer output parsed fine but no
//     branch applied — the output was semantically unusable by this
//     consumer. Maps to downstream_rejected.
//   - anything else (bad gate expression, evaluator bug): the gate
//     itself broke. Maps to gate_failed.
func classifyGateEvalError(err error) (outcome, errorClass string) {
	if err == nil {
		return string(stepoutcome.OK), ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "failed to parse gate input as JSON"):
		return string(stepoutcome.ParseError), stepoutcome.ClassGateInvalidJSON
	case strings.Contains(msg, "no gate condition matched"):
		return string(stepoutcome.DownstreamRejected), stepoutcome.ClassGateEvalFailed
	default:
		return string(stepoutcome.GateFailed), stepoutcome.ClassGateEvalFailed
	}
}

// buildGatePromptSuffix generates response format instructions from a step's
// gate conditions. This tells the agent exactly what JSON structure it must
// produce so that gate evaluation can route the workflow.
func buildGatePromptSuffix(gates []registry.WorkflowGate) string {
	// Collect the unique JSON field paths and their expected values from
	// all gate conditions to build an example response object.
	//
	// Example gates:
	//   review.approved == true  → complete
	//   review.approved == false → implement
	//
	// Produces:
	//   {"review":{"approved": true or false}}
	var lines []string
	for _, g := range gates {
		lines = append(lines, fmt.Sprintf("  - %s (routes to: %s)", g.Condition, g.Target))
	}
	return fmt.Sprintf(`

IMPORTANT: You MUST respond with a pure JSON object (no markdown, no extra text).
The following conditions will be evaluated on your response to decide the next workflow step:
%s

Your entire response must be a valid JSON object containing the fields referenced above.
Do NOT wrap the JSON in markdown code fences. Do NOT include any text outside the JSON object.`, strings.Join(lines, "\n"))
}

// GateEvalTrace captures the per-gate outcome of one gate-step
// evaluation. The caller logs this so operators can see exactly which
// conditions the gate walked and what their comparands resolved to,
// without having to replay the LLM response. Zero value is a no-trace.
type GateEvalTrace struct {
	// RawPreview is a truncated view of the producer's raw output.
	RawPreview string
	// Entries holds per-gate diagnostic lines.
	Entries []GateEvalEntry
}

// GateEvalEntry is one attempted gate condition's evaluation result.
type GateEvalEntry struct {
	Condition string
	Target    string
	// Matched true means this gate fired and Target was selected.
	Matched bool
	// Observed is the value the left-hand side of the condition
	// resolved to (or the zero value plus Found=false).
	Observed any
	Found    bool
	// Wanted is the right-hand literal parsed from the condition.
	Wanted any
	// Err surfaces evaluator-internal failures (bad condition syntax).
	Err string
}

// evaluateGateStepTraced evaluates a gate step's conditions
// against the previous step's JSON result, returning the matched
// target plus a per-condition trace for diagnostics. Pure
// function; the caller decides whether to log the returned trace.
func evaluateGateStepTraced(step registry.WorkflowStep, lastResult json.RawMessage) (string, GateEvalTrace, error) {
	trace := GateEvalTrace{RawPreview: previewJSON(lastResult)}

	if len(step.Gates) == 0 {
		return "", trace, fmt.Errorf("gate step has no gates")
	}

	var payload any
	if len(lastResult) > 0 {
		// Use the same envelope-aware normalization the schema validator
		// uses, so a gate condition like "has_approvals == true" still
		// resolves correctly when the model emitted its JSON inside the
		// envelope's `message` field instead of at the top level (e.g.
		// the agent harness pass-3 extraction failed on multi-object
		// output — observed on risk-officer 2026-05-07,
		// exec_20260507164414_31fc381f97738812).
		obj, err := normalizedResultPayload(lastResult)
		if err != nil {
			return "", trace, fmt.Errorf("failed to parse gate input as JSON: %w — agent response was: %s", err, trace.RawPreview)
		}
		payload = obj
	}

	for _, gate := range step.Gates {
		entry := GateEvalEntry{Condition: gate.Condition, Target: gate.Target}
		// Best-effort pull the LHS so the trace shows what the gate
		// saw. We only care about simple `left == right` gates for
		// diagnostics; compound `&&` conditions resolve the whole
		// expression below but don't populate Observed/Wanted.
		if parts := strings.SplitN(gate.Condition, "==", 2); len(parts) == 2 && !strings.Contains(gate.Condition, "&&") {
			left := strings.TrimSpace(parts[0])
			right := strings.TrimSpace(parts[1])
			entry.Observed, entry.Found = lookupJSONPath(payload, left)
			if w, err := parseGateValue(right); err == nil {
				entry.Wanted = w
			}
		}

		matched, err := evaluateGateCondition(gate.Condition, payload)
		if err != nil {
			entry.Err = err.Error()
			trace.Entries = append(trace.Entries, entry)
			return "", trace, err
		}
		entry.Matched = matched
		trace.Entries = append(trace.Entries, entry)
		if matched {
			return gate.Target, trace, nil
		}
	}

	// Build a diagnostic showing expected conditions vs. what was received.
	var conditions []string
	for _, g := range step.Gates {
		conditions = append(conditions, g.Condition)
	}
	return "", trace, fmt.Errorf("no gate condition matched (expected one of: [%s], got: %s)",
		strings.Join(conditions, " | "), trace.RawPreview)
}

// previewJSON returns a short, safe preview of a raw JSON body for
// error messages and trace entries. Truncates at 300 chars with an
// ellipsis so the surrounding error line stays readable.
func previewJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func evaluateGateCondition(condition string, payload any) (bool, error) {
	// Support compound conditions joined by &&.
	if strings.Contains(condition, "&&") {
		for _, sub := range strings.Split(condition, "&&") {
			sub = strings.TrimSpace(sub)
			if sub == "" {
				// Empty subterm (trailing/leading/double && in the
				// gate condition) is a config error, NOT vacuous
				// truth. Pre-2026-05-06 we silently skipped these,
				// which meant `result.approved == true &&` evaluated
				// as true and the gate let work past that should
				// have been rejected. Fail the step loudly so the
				// operator notices the typo.
				return false, fmt.Errorf("gate condition %q has empty sub-term — check for trailing/double &&", condition)
			}
			matched, err := evaluateSingleCondition(sub, payload)
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		return true, nil
	}
	return evaluateSingleCondition(condition, payload)
}

func evaluateSingleCondition(condition string, payload any) (bool, error) {
	parts := strings.SplitN(condition, "==", 2)
	if len(parts) != 2 {
		return false, fmt.Errorf("unsupported gate condition %q", condition)
	}
	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])

	actual, found := lookupJSONPath(payload, left)
	if !found {
		return false, nil
	}

	expected, err := parseGateValue(right)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(actual, expected), nil
}

// lookupJSONPath resolves a dotted path against a decoded JSON payload.
// It prefers the nested-object interpretation (`a.b.c` → payload["a"]["b"]["c"])
// but falls back to treating the whole path as a single literal key if
// the nested walk misses. LLMs frequently produce flat keys like
// `{"review.approved": true}` when a prompt references `review.approved`
// as a path — accepting both shapes means the gate works regardless of
// how the model chose to structure its output, without forcing us to
// re-prompt the model or add schema validation.
func lookupJSONPath(payload any, path string) (any, bool) {
	object, ok := payload.(map[string]any)
	if !ok {
		return nil, false
	}
	// Flat-key fast path: if the producer emitted the whole dotted
	// name as a single top-level key, prefer it over walking nested
	// objects. This matches the most common LLM failure mode and
	// keeps the nested walk reserved for explicitly-nested payloads.
	if value, ok := object[path]; ok {
		return value, true
	}
	// Nested walk: a.b.c → object["a"]["b"]["c"].
	current := any(object)
	for _, part := range strings.Split(path, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := obj[part]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func parseGateValue(raw string) (any, error) {
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1], nil
	}
	var numeric float64
	if err := json.Unmarshal([]byte(raw), &numeric); err == nil {
		return numeric, nil
	}
	return raw, nil
}
