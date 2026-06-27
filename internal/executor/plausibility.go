package executor

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// PlausibilityViolation is one rule's verdict on a result.json. The
// caller maps this to either a step failure (when WarnOnly is false)
// or a log line (when true). Includes both the rule name and the
// human-readable detail so the operator's dashboard can show "rule X
// fired because field Y was empty" without re-evaluating.
type PlausibilityViolation struct {
	// RuleName is the operator-supplied identifier from the role
	// yaml's plausibilityRules[i].name. Falls back to "rule[i]"
	// when the operator didn't name the rule.
	RuleName string
	// Detail is the specific reason the rule fired, e.g.
	// "field 'feedback' is empty under condition approved=false".
	Detail string
	// WarnOnly carries the rule's WarnOnly flag through to the
	// caller so it can decide whether to gate or log.
	WarnOnly bool
}

// EvaluatePlausibility runs each rule against the parsed result.json.
// Rules whose When clause doesn't match are no-ops; rules that match
// return a violation when any of their Require fields are absent or
// empty. Returns every violation (no early-exit) so the operator
// sees the full picture in a single log line / dashboard entry.
//
// resultBytes is expected to already have been parsed as a JSON
// object by the caller; we re-parse here rather than thread the
// parsed map through to keep the helper self-contained. Cheap —
// result.json is typically <10KB.
//
// An empty rules slice or unparseable resultBytes returns no
// violations: rules that can't apply (because there's no JSON to
// inspect) shouldn't poison the step. The earlier
// validateRequiredOutputKeys path already classifies "result.json
// can't be parsed" failures as INVALID_OUTPUT.
func EvaluatePlausibility(resultBytes []byte, rules []registry.PlausibilityRule) []PlausibilityViolation {
	if len(rules) == 0 || len(resultBytes) == 0 {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		return nil
	}

	var out []PlausibilityViolation
	for i, rule := range rules {
		if !ruleMatches(parsed, rule.When) {
			continue
		}
		for _, field := range rule.Require {
			if isMissingOrEmpty(parsed, field) {
				out = append(out, PlausibilityViolation{
					RuleName: ruleName(rule, i),
					Detail:   formatViolationDetail(rule, field),
					WarnOnly: rule.WarnOnly,
				})
			}
		}
	}
	return out
}

// ruleMatches reports whether every entry in `when` is present in
// `parsed` and equals the expected value. Empty `when` is "match
// always" — operators use that to express "no matter what, these
// fields must be filled in" (a stricter form of requiredOutputKeys
// that adds the non-empty check).
//
// Path lookup uses lookupJSONPath (flat-key fast path then nested
// walk), so a `when:` clause keyed on `writing.written` matches
// whether the model emitted `{"writing": {"written": true}}` or
// `{"writing.written": true}`. Pre-2026-05-08, the lookup was a
// naked map indexing; nested-path rules silently never matched, and
// the schema-derived plausibility rules introduced with the
// outputSchema migration (writing.path, review.feedback, etc.) were
// inert. Caught by the result-replay corpus on first run.
func ruleMatches(parsed map[string]any, when map[string]any) bool {
	if len(when) == 0 {
		return true
	}
	for k, want := range when {
		got, ok := lookupJSONPath(parsed, k)
		if !ok {
			return false
		}
		if !valueEquals(got, want) {
			return false
		}
	}
	return true
}

// valueEquals normalises across the YAML/JSON unmarshal type drift
// before comparing: YAML decodes "false" → bool, "0" → int, "0.0" →
// float64; JSON decodes the same as bool / float64 / float64. A
// reflect.DeepEqual on raw values would refuse YAML int 0 == JSON
// float64 0. Explicit type-pair handling keeps the rule predicates
// behaving as the operator wrote them.
func valueEquals(got, want any) bool {
	if reflect.DeepEqual(got, want) {
		return true
	}
	// Numeric coercion: YAML int -> JSON float64.
	if gn, gok := toFloat64(got); gok {
		if wn, wok := toFloat64(want); wok {
			return gn == wn
		}
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// isMissingOrEmpty reports whether `field` in `parsed` is absent or
// holds an "empty" value. Empty means: nil, "", []any{}, map[]any{}{},
// or numeric 0. Numeric zero is treated as empty deliberately: an
// LLM that returns "feedback": 0 is broken, regardless of whether
// the schema would technically accept a number there.
//
// Path lookup uses lookupJSONPath (flat-key fast path then nested
// walk), matching the gate evaluator's semantics so a rule keyed on
// `writing.path` accepts both `{"writing": {"path": "..."}}` and the
// flat-key form an LLM sometimes emits. Pre-2026-05-08 a naked map
// index meant nested-path requires never resolved — the
// outputSchema-migration's nested rules were silently inert. Replay
// corpus caught it.
func isMissingOrEmpty(parsed map[string]any, field string) bool {
	val, ok := lookupJSONPath(parsed, field)
	if !ok {
		return true
	}
	return isEmptyValue(val)
}

func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case float64:
		return x == 0
	case float32:
		return x == 0
	case int:
		return x == 0
	case int32:
		return x == 0
	case int64:
		return x == 0
	case bool:
		// Bools are NOT treated as empty when false — that's a
		// legitimate value, not absence. Plausibility rules express
		// "feedback must be non-empty when approved=false" via the
		// When clause, not by treating false as missing.
		return false
	}
	return false
}

// formatViolationDetail builds a human-readable explanation of why a
// rule fired. The operator sees this in the failed-task UI and the
// dashboard, so naming the field and the condition that triggered
// the requirement is the priority.
func formatViolationDetail(rule registry.PlausibilityRule, missingField string) string {
	if len(rule.When) == 0 {
		return fmt.Sprintf("required field %q is missing or empty", missingField)
	}
	conds := make([]string, 0, len(rule.When))
	for k, v := range rule.When {
		conds = append(conds, fmt.Sprintf("%s=%v", k, v))
	}
	return fmt.Sprintf("under condition %s, field %q is missing or empty",
		strings.Join(conds, " AND "), missingField)
}

// ruleName falls back to a positional label so logs and the
// dashboard always have something to display, even when the
// operator didn't name the rule.
func ruleName(rule registry.PlausibilityRule, idx int) string {
	if rule.Name != "" {
		return rule.Name
	}
	return fmt.Sprintf("rule[%d]", idx)
}
