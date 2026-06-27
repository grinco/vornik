package executor

import (
	"encoding/json"
	"regexp"
	"strings"
)

// outputRefRegex matches ${outputs.<step-id>.<field-path>}
// references inside string values. The dot-separated field
// path supports nested JSON access: ${outputs.spec.data.kpis}
// drills into the spec step's result.json under data.kpis.
//
// Inter-project orchestration Phase D — referenced by both
// call_project payload interpolation and spawn_project params
// interpolation.
var outputRefRegex = regexp.MustCompile(`\$\{outputs\.([A-Za-z0-9_\-]+)\.([A-Za-z0-9_\.\-]+)\}`)

// interpolateOutputs walks the YAML-decoded value tree and
// replaces every ${outputs.<step>.<field>} string with the
// resolved value from stepResults. Returns a new tree;
// the input is not mutated (workflow YAML is shared across
// executions).
//
// Resolution rules:
//   - String value matches the regex exactly → substitute the
//     raw JSON value (could be a string, number, list, object).
//     A payload field `brief: ${outputs.x.brief}` ends up with
//     whatever shape x.brief carries.
//   - String value contains the regex AS A SUBSTRING (e.g.
//     `"campaign for ${outputs.x.product}"`) → substitute the
//     scalar string form; non-string values JSON-marshal so
//     the surrounding template stays intact.
//   - Reference points at a missing step or field → empty
//     string. The step author writes ${outputs.X} expecting it
//     to be there; an absent reference falling back to ""
//     surfaces in payloads as obviously-broken values rather
//     than failing the step.
//
// Recursive: descends into nested maps + slices.
func interpolateOutputs(in any, stepResults map[string]json.RawMessage) any {
	if in == nil {
		return nil
	}
	switch v := in.(type) {
	case string:
		return interpolateStringRefs(v, stepResults)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = interpolateOutputs(val, stepResults)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = interpolateOutputs(val, stepResults)
		}
		return out
	default:
		return v
	}
}

// interpolateStringRefs resolves all ${outputs.x.y} occurrences
// in s. Exact-match strings substitute the raw value (preserves
// the JSON type); embedded references substitute scalar form.
func interpolateStringRefs(s string, stepResults map[string]json.RawMessage) any {
	matches := outputRefRegex.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	// Exact-match case: the entire string IS the reference. The
	// caller likely wants the raw shape (list/map/number); a
	// stringified version would lose type fidelity.
	if len(matches) == 1 {
		start, end := matches[0][0], matches[0][1]
		if start == 0 && end == len(s) {
			step := s[matches[0][2]:matches[0][3]]
			field := s[matches[0][4]:matches[0][5]]
			val := resolveStepField(stepResults, step, field)
			if val == nil {
				return ""
			}
			return val
		}
	}
	// Embedded case: substitute each match as its scalar string
	// form. Preserves the surrounding template text.
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		step := s[m[2]:m[3]]
		field := s[m[4]:m[5]]
		val := resolveStepField(stepResults, step, field)
		b.WriteString(scalarStringForm(val))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// resolveStepField looks up stepResults[step], parses it as
// JSON, then drills through the dot-separated field path.
// Returns nil when the step is absent, the body fails to
// parse, or the path can't be resolved.
func resolveStepField(stepResults map[string]json.RawMessage, step, fieldPath string) any {
	raw, ok := stepResults[step]
	if !ok || len(raw) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	for _, seg := range strings.Split(fieldPath, ".") {
		obj, isMap := doc.(map[string]any)
		if !isMap {
			return nil
		}
		v, present := obj[seg]
		if !present {
			return nil
		}
		doc = v
	}
	return doc
}

// scalarStringForm renders a resolved value as a string for
// embedded interpolation. Strings pass through, numbers/bools
// via fmt-friendly conversion, lists/maps JSON-marshal.
func scalarStringForm(v any) string {
	if v == nil {
		return ""
	}
	switch tv := v.(type) {
	case string:
		return tv
	case bool:
		if tv {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode to float64 by default. Use
		// FormatFloat with -1 prec so integers don't render
		// as "5.000000".
		b, _ := json.Marshal(tv)
		return string(b)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
