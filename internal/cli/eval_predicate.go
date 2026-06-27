package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// EvalExpectation is the predicate the operator declares for a case.
// Exactly one of the fields should be set; multiple set means
// every set predicate must pass (AND-combined). Empty means
// "passes when the task reaches a terminal status without checking
// the result body" — useful for smoke tests where existence of a
// successful run is enough.
//
// The shape is deliberately minimal. Operators express their checks
// by picking the simplest predicate that catches the regression
// they care about — fancier matchers (LLM-judge, multi-field
// predicates, structural diffs) can layer on later without
// breaking the existing format.
type EvalExpectation struct {
	// Outcome asserts the task's terminal status. Accepted
	// values: "COMPLETED", "FAILED", "CANCELLED". Empty skips
	// the status check (other predicates still apply).
	Outcome string `json:"outcome,omitempty" yaml:"outcome,omitempty"`
	// Equals is a JSON-shaped expected value. The matcher
	// requires the result.json payload to deep-equal this
	// structure. Use for tightly-specified outputs (a workflow
	// that should always emit {"approved": true, "score": 100}
	// for a known-good input).
	Equals json.RawMessage `json:"equals,omitempty" yaml:"equals,omitempty"`
	// Contains is a JSON object whose every key/value must be
	// present in the result.json (subset match, top-level only).
	// Use when the workflow emits more fields than the test
	// cares about — assert just the relevant ones.
	Contains json.RawMessage `json:"contains,omitempty" yaml:"contains,omitempty"`
	// Regex is matched against the JSON-encoded result.json. Use
	// when the test target is text inside a "message" field or
	// a value whose exact form varies (timestamps, generated
	// IDs). Matched against the marshalled result, NOT a
	// specific field — this is the lowest-fidelity matcher and
	// the right tool when nothing else fits.
	Regex string `json:"regex,omitempty" yaml:"regex,omitempty"`
}

// EvalEvidence is the data plane the predicate evaluator inspects.
// Bundled into a struct so tests can construct one directly without
// minting a fake task + execution lifecycle.
type EvalEvidence struct {
	// TerminalStatus is the task's final status: "COMPLETED",
	// "FAILED", "CANCELLED". Empty means "didn't reach terminal
	// in the polling window" — an automatic fail.
	TerminalStatus string
	// Result is the raw JSON the executor persisted for the
	// successful execution. May be empty for FAILED tasks; the
	// predicate handles that.
	Result json.RawMessage
}

// EvalVerdict is what the predicate returns. Passed=true means the
// case is green; Passed=false carries a Reason string the CLI
// prints back to the operator.
type EvalVerdict struct {
	Passed bool
	Reason string
}

// EvaluateExpectation runs every set predicate in order and returns
// the first failure (or success when all pass). Order matters for
// the user-facing message: "outcome != COMPLETED" is more useful
// than "regex didn't match" when the task didn't even succeed.
func EvaluateExpectation(expect EvalExpectation, evidence EvalEvidence) EvalVerdict {
	if isEmptyExpectation(expect) {
		// Empty predicate degrades to "task reached terminal
		// status without erroring." That's a smoke-test
		// signal; we still want the case to fail when the
		// task crashed or got cancelled.
		if evidence.TerminalStatus == "COMPLETED" {
			return EvalVerdict{Passed: true}
		}
		return EvalVerdict{
			Passed: false,
			Reason: fmt.Sprintf("no expectation set; task ended in %s, expected COMPLETED", emptyStatusOrName(evidence.TerminalStatus)),
		}
	}

	if expect.Outcome != "" {
		if !strings.EqualFold(expect.Outcome, evidence.TerminalStatus) {
			return EvalVerdict{
				Passed: false,
				Reason: fmt.Sprintf("expected outcome=%s, got %s", expect.Outcome, emptyStatusOrName(evidence.TerminalStatus)),
			}
		}
	}

	if len(expect.Equals) > 0 {
		ok, err := evalEquals(expect.Equals, evidence.Result)
		if err != nil {
			return EvalVerdict{Passed: false, Reason: "equals: " + err.Error()}
		}
		if !ok {
			return EvalVerdict{
				Passed: false,
				Reason: fmt.Sprintf("equals: result.json does not deep-equal expected (got %s)", truncateForLog(string(evidence.Result), 200)),
			}
		}
	}

	if len(expect.Contains) > 0 {
		missing, err := evalContains(expect.Contains, evidence.Result)
		if err != nil {
			return EvalVerdict{Passed: false, Reason: "contains: " + err.Error()}
		}
		if missing != "" {
			return EvalVerdict{
				Passed: false,
				Reason: fmt.Sprintf("contains: %s", missing),
			}
		}
	}

	if expect.Regex != "" {
		re, err := regexp.Compile(expect.Regex)
		if err != nil {
			return EvalVerdict{Passed: false, Reason: "regex: " + err.Error()}
		}
		if !re.Match(evidence.Result) {
			return EvalVerdict{
				Passed: false,
				Reason: fmt.Sprintf("regex %q did not match result.json", expect.Regex),
			}
		}
	}

	return EvalVerdict{Passed: true}
}

func isEmptyExpectation(e EvalExpectation) bool {
	return e.Outcome == "" && len(e.Equals) == 0 && len(e.Contains) == 0 && e.Regex == ""
}

func emptyStatusOrName(s string) string {
	if s == "" {
		return "<no terminal status>"
	}
	return s
}

// evalEquals deep-compares the expected JSON against the actual.
// Both are unmarshalled so map-key order, whitespace, and trailing
// newlines don't false-fail.
func evalEquals(expected, actual json.RawMessage) (bool, error) {
	if len(actual) == 0 {
		return false, nil
	}
	var e, a any
	if err := json.Unmarshal(expected, &e); err != nil {
		return false, fmt.Errorf("expected payload is not valid JSON: %w", err)
	}
	if err := json.Unmarshal(actual, &a); err != nil {
		// Actual is not valid JSON → predicate fails but we
		// don't surface that as an evaluator error: the
		// caller sees it as a normal pass/fail with the
		// truncated raw body in the reason.
		return false, nil
	}
	return reflect.DeepEqual(e, a), nil
}

// evalContains checks that every top-level key in expected is
// present in actual with a deep-equal value. Returns "" when the
// subset matches; a human-readable string naming the first
// mismatch otherwise. Nested values still deep-equal — Contains is
// a top-level KEY filter, not a fully recursive partial-match.
// Operators who need nested partials should compose multiple
// Contains predicates by flattening their result schema.
func evalContains(expected, actual json.RawMessage) (string, error) {
	if len(actual) == 0 {
		return "result.json is empty; nothing to check", nil
	}
	var e, a map[string]any
	if err := json.Unmarshal(expected, &e); err != nil {
		return "", fmt.Errorf("expected payload is not a JSON object: %w", err)
	}
	if err := json.Unmarshal(actual, &a); err != nil {
		return "result.json is not a JSON object", nil
	}
	for k, want := range e {
		got, ok := a[k]
		if !ok {
			return fmt.Sprintf("key %q is missing from result.json", k), nil
		}
		if !reflect.DeepEqual(want, got) {
			return fmt.Sprintf("key %q value mismatch: want %v, got %v", k, want, got), nil
		}
	}
	return "", nil
}

// truncateForLog clips long strings so the failure reason stays
// readable in a CLI table cell. The truncation marker keeps the
// reader aware that the payload was longer.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}
