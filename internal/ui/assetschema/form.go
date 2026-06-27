package assetschema

import (
	"strconv"
	"strings"
)

// FieldValue is one bound, validated, type-converted form value ready to
// become a YAML patch. Path is the dotted key; Value is the Go-typed
// value (int, float64, bool, string, or []string). Provided is false when
// the operator left the field blank (the patcher removes the key for
// optional fields).
type FieldValue struct {
	Path     string
	Kind     Kind
	Value    any
	Provided bool
}

// FieldError is a per-field validation failure surfaced back to the form.
type FieldError struct {
	Path    string
	Label   string
	Message string
}

// BindForm reads every editable (non-ReadOnly) field of the schema via the
// get lookup (e.g. (*http.Request).FormValue, keyed on the field Path —
// HTML name attributes may contain dots), validates it with the field's
// typed rules, and converts it to a Go-typed FieldValue. ReadOnly fields
// are skipped entirely (never bound, never patched). It returns the bound
// values and any per-field errors; when errors is non-empty the caller
// must NOT write (re-render the form with the errors instead).
func BindForm(s AssetSchema, get func(name string) string) (values []FieldValue, errs []FieldError) {
	for _, f := range s.Fields() {
		if f.ReadOnly {
			continue
		}
		// Normalise line endings before anything else. HTML <textarea> values
		// are submitted CRLF-encoded per the HTTP spec; writing those bytes
		// verbatim into the deployed YAML/markdown was the root cause of the
		// recurring config-drift-via-CRLF incident. Collapsing CRLF (and bare
		// CR) to LF here — the one seam every schema save routes through —
		// guarantees LF-only content on disk for all asset types and fields.
		raw := strings.TrimSpace(normalizeLineEndings(get(f.Path)))
		if err := f.ValidateValue(raw); err != nil {
			errs = append(errs, FieldError{Path: f.Path, Label: f.label(), Message: err.Error()})
			continue
		}
		values = append(values, FieldValue{
			Path:     f.Path,
			Kind:     f.Kind,
			Value:    convertValue(f.Kind, raw),
			Provided: raw != "",
		})
	}
	return values, errs
}

// normalizeLineEndings collapses Windows (CRLF) and bare Mac (CR) line
// endings to Unix LF. Applied to every inbound form value so UI-edited
// schema assets persist with LF-only line endings (config-drift-via-CRLF
// regression — see TestBindForm_NormalizesLineEndings).
func normalizeLineEndings(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// convertValue maps a validated raw string to the Go type the YAML patcher
// expects for the field's kind. Validation has already run, so parses here
// cannot fail (errors are ignored deliberately — a parse error would mean
// ValidateValue is out of sync, which the unit tests guard against).
func convertValue(kind Kind, raw string) any {
	if raw == "" {
		// Empty optional → zero value of the kind; Provided=false tells
		// the patcher to remove rather than write it.
		switch kind {
		case KindInt:
			return 0
		case KindFloat:
			return 0.0
		case KindBool:
			return false
		case KindStringList:
			return []string{}
		default:
			return ""
		}
	}
	switch kind {
	case KindInt:
		n, _ := strconv.Atoi(raw)
		return n
	case KindFloat:
		f, _ := strconv.ParseFloat(raw, 64)
		return f
	case KindBool:
		return parseBoolLiteral(raw)
	case KindStringList:
		return splitList(raw)
	default: // string, enum, duration → stored as a scalar string
		return raw
	}
}

// parseBoolLiteral maps the HTML-checkbox + common literals to a bool.
func parseBoolLiteral(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "on", "1", "yes":
		return true
	default:
		return false
	}
}

// splitList splits a textarea/CSV value into a trimmed, empty-dropped
// string slice. Accepts newline- or comma-separated entries.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// CurrentValues maps a decoded asset document (YAML unmarshalled into a
// nested map[string]any — the caller decodes to avoid coupling this
// package to a YAML library) to a path→display-string map for pre-filling
// the form on GET. Missing keys yield "" (the field shows blank / its
// default). String-lists are joined with newlines for textarea display.
func CurrentValues(s AssetSchema, doc map[string]any) map[string]string {
	out := make(map[string]string, len(s.Fields()))
	for _, f := range s.Fields() {
		raw, ok := lookupPath(doc, strings.Split(f.Path, "."))
		if !ok {
			out[f.Path] = ""
			continue
		}
		out[f.Path] = displayValue(f.Kind, raw)
	}
	return out
}

// lookupPath walks a dotted path into a nested map produced by YAML
// unmarshalling. YAML v3 decodes maps as map[string]any.
func lookupPath(doc map[string]any, segs []string) (any, bool) {
	cur := any(doc)
	for _, seg := range segs {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[seg]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// displayValue renders a decoded YAML value as the string shown in a form
// input. Lists join with newlines (for a textarea); scalars use their
// natural string form.
func displayValue(kind Kind, v any) string {
	if kind == KindStringList {
		items, ok := v.([]any)
		if !ok {
			return ""
		}
		parts := make([]string, 0, len(items))
		for _, it := range items {
			parts = append(parts, scalarString(it))
		}
		return strings.Join(parts, "\n")
	}
	return scalarString(v)
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
