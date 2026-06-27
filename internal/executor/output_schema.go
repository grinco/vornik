package executor

import (
	"encoding/json"
	"fmt"
	"strings"
)

// validateRequiredOutputKeys parses resultBytes as a JSON object and
// returns the subset of required entries that are absent or fail
// type assertion. A non-object body (array, string, null) fails
// every check — the role promised a structured output and produced
// something that can't carry the required fields.
//
// Each entry uses the syntax "path[:type]" where:
//
//   - path is a dot-separated traversal: "review.approved" walks
//     {"review":{"approved":...}}. Bare names ("plan") are top-level
//     keys. Array indexes are not supported — paths address objects.
//
//   - type, when present, is one of: "string", "bool", "number",
//     "array", "object". The path's value must match. Absent type
//     matches any non-null value.
//
// Backward compat: existing role yamls with bare key names ("plan",
// "research") still work — those become path-only entries with no
// type assertion, identical to the previous shallow check.
//
// Empty resultBytes or empty required list returns no missing keys:
// callers are responsible for not invoking the check when the
// contract doesn't apply.
func validateRequiredOutputKeys(resultBytes []byte, required []string) []string {
	if len(resultBytes) == 0 || len(required) == 0 {
		return nil
	}
	parsed, err := normalizedResultPayload(resultBytes)
	if err != nil {
		// Not a JSON object — every required key is missing.
		out := make([]string, len(required))
		copy(out, required)
		return out
	}
	var missing []string
	for _, entry := range required {
		path, typeName := parseSchemaEntry(entry)
		val, ok := walkPath(parsed, path)
		if !ok {
			missing = append(missing, entry)
			continue
		}
		if typeName != "" && !valueMatchesType(val, typeName) {
			missing = append(missing, fmt.Sprintf("%s (wrong type: got %s)", entry, jsonTypeName(val)))
		}
	}
	return missing
}

// normalizedResultPayload parses an agent result.json envelope and
// returns a map that has the model's structured output keys lifted to
// the top level. Three layers of recovery:
//
//  1. The envelope parses cleanly. Return it as-is — but if it has a
//     `message` field that is itself a JSON-encoded string, hoist that
//     string's keys onto the envelope (envelope keys win on collision).
//     This is the safety net for the agent harness's pass-3
//     extraction bug, which fails to merge when the model leaks
//     multi-object output (e.g. `<think>{reasoning}</think>{final}` —
//     observed on glm-5 risk-officer 2026-05-07,
//     exec_20260507164414_31fc381f97738812 — first/last brace spans
//     multiple objects, jq rejects, no merge, validation fails).
//
//  2. The envelope itself doesn't parse but contains a balanced
//     trailing JSON object — extract that and use it. Covers the case
//     where the agent harness wrote raw model output without any
//     enveloping (rare but observed historically).
//
//  3. Nothing parses anywhere — return an error so the validator
//     reports every key missing, matching the original strict
//     behaviour.
//
// The function deliberately does NOT mutate resultBytes; downstream
// audit / secret-scan / message-handover code keeps reading the raw
// envelope.
func normalizedResultPayload(resultBytes []byte) (map[string]any, error) {
	var envelope map[string]any
	if err := json.Unmarshal(resultBytes, &envelope); err != nil {
		// Layer 2: raw bytes aren't a valid envelope but might contain
		// a trailing JSON object (model wrote prose + JSON without the
		// agent harness wrapping it).
		if extracted := extractLastJSONObject(resultBytes); extracted != nil {
			var inner map[string]any
			if err2 := json.Unmarshal(extracted, &inner); err2 == nil {
				return inner, nil
			}
		}
		return nil, err
	}
	// Layer 1: hoist keys from envelope.message when it's a JSON-encoded
	// string. Envelope keys take precedence — never overwrite something
	// the agent harness already populated.
	if msg, ok := envelope["message"].(string); ok && msg != "" {
		if inner := extractLastJSONObject([]byte(msg)); inner != nil {
			var innerObj map[string]any
			if err := json.Unmarshal(inner, &innerObj); err == nil {
				for k, v := range innerObj {
					if _, exists := envelope[k]; !exists {
						envelope[k] = v
					}
				}
			}
		}
	}
	return envelope, nil
}

// extractLastJSONObject scans b for top-level balanced JSON objects
// and returns the last one's bytes, or nil if no balanced object is
// found. Respects "..." string literals (with backslash escapes) so
// braces inside strings don't confuse the depth counter.
//
// "Last" is the right heuristic for LLM output: models emit reasoning
// prose first and the final structured answer last (`<think>{scratch}
// </think>{answer}`), so the trailing object is the one operators
// actually want.
func extractLastJSONObject(b []byte) []byte {
	var lastStart, lastEnd = -1, -1
	depth := 0
	startIdx := -1
	inString := false
	escape := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				startIdx = i
			}
			depth++
		case '}':
			if depth == 0 {
				// Stray closer outside any object — ignore.
				continue
			}
			depth--
			if depth == 0 && startIdx >= 0 {
				lastStart = startIdx
				lastEnd = i + 1
				startIdx = -1
			}
		}
	}
	if lastStart < 0 || lastEnd <= lastStart {
		return nil
	}
	return b[lastStart:lastEnd]
}

// ExtractLastJSONObject is the exported form of extractLastJSONObject, for
// out-of-package consumers that need the same "model emits reasoning prose
// first, final structured object last" extraction — e.g. forge.post_review
// rendering a reviewer's structured output to clean markdown instead of
// posting the raw envelope. Returns nil when no balanced object is found.
func ExtractLastJSONObject(b []byte) []byte { return extractLastJSONObject(b) }

// parseSchemaEntry splits "path:type" into its parts. A bare entry
// without a colon returns the whole string as the path and an empty
// type. Trailing/leading whitespace is trimmed so operators can use
// list-form yaml without accidental space damage.
func parseSchemaEntry(entry string) (path, typeName string) {
	entry = strings.TrimSpace(entry)
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		return strings.TrimSpace(entry[:idx]), strings.TrimSpace(entry[idx+1:])
	}
	return entry, ""
}

// walkPath traverses obj along the dot-separated segments of path and
// returns the value at the end. Routes through lookupJSONPath so it
// uses the same semantics every other path-lookup site in the
// executor uses (flat-key fast path then nested walk) — without this,
// a model emitting `{"writing.written": true}` as a flat top-level
// key would fail validation here while the gate evaluator and
// plausibility checker both accept it. The inconsistency was the same
// bug class fixed for plausibility in commit 265355b.
func walkPath(obj map[string]any, path string) (any, bool) {
	return lookupJSONPath(obj, path)
}

// valueMatchesType returns whether v fits the named JSON type.
// "number" matches both float64 and int (Go's encoding/json defaults
// to float64; a json.Number could be either) — operators write
// schemas in JSON terms, and JSON makes no int/float distinction.
func valueMatchesType(v any, typeName string) bool {
	switch typeName {
	case "string":
		_, ok := v.(string)
		return ok
	case "bool", "boolean":
		_, ok := v.(bool)
		return ok
	case "number", "int", "integer", "float":
		switch v.(type) {
		case float64, float32, int, int32, int64:
			return true
		}
		return false
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "any", "":
		return v != nil
	default:
		// Unknown type — be permissive rather than fail an entire
		// step on a typo in the role yaml. Doctor surfaces these.
		return true
	}
}

// jsonTypeName reports the JSON-side type name of an unmarshalled
// value. Used to make the missing-keys diagnostic readable.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64, float32, int, int32, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}
