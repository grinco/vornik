package executor

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// enumViolation is one value the agent emitted that the schema it was handed
// does not admit.
type enumViolation struct {
	// Path addresses the offending value the way an operator reads it:
	// "testing.cases[0].id", not a node pointer.
	Path string
	// Value is what arrived. Always reproduced verbatim in the message — it is
	// the one thing a retry cannot guess.
	Value any
	// Allowed is the enum as declared, in declaration order.
	Allowed []any
	// Mode is the node's declared consequence. See registry.ViolationMode.
	Mode registry.ViolationMode
}

// validateEnums walks the schema the model was ACTUALLY HANDED against what it
// wrote, and returns every value outside a declared enum.
//
// Why the emitted schema and not the role's declaration: for a
// pinned_case_validation verifier the binding enum is the per-execution clone
// applyPinnedCaseEnum installs, and role.OutputSchema is shared by every
// execution of that role and deliberately never carries it. Walking the
// declaration would enforce the static enums and silently miss the only one
// D6 is about. See 2026-08-13-agent-quality-benchmark-design.md D6.1a.
//
// An unparseable result yields NO enum violations: "this is not the shape you
// promised" belongs to validateRequiredOutputKeys, and reporting it here would
// misattribute a shape failure to a contract failure.
func validateEnums(resultBytes []byte, schema *registry.OutputSchema) []enumViolation {
	if len(resultBytes) == 0 || schema == nil {
		return nil
	}
	parsed, err := normalizedResultPayload(resultBytes)
	if err != nil {
		return nil
	}
	var out []enumViolation
	walkEnumNode(schema, parsed, "", &out)
	return out
}

// walkEnumNode descends the schema and the decoded payload together. It only
// ever reads where BOTH have something: an absent field is the required-keys
// check's business, because an enum constrains a value, never its presence.
// That is what keeps dynamic-tool-budget-design.md §4's "absent -> 1.0x" leg
// reachable for an optional field like analysis.complexity.
func walkEnumNode(node *registry.OutputSchema, value any, path string, out *[]enumViolation) {
	if node == nil || value == nil {
		return
	}
	if len(node.Enum) > 0 && !enumAdmits(node.Enum, value) {
		*out = append(*out, enumViolation{
			Path:    path,
			Value:   value,
			Allowed: node.Enum,
			Mode:    node.ViolationMode(),
		})
	}
	switch typed := value.(type) {
	case map[string]any:
		names := make([]string, 0, len(node.Properties))
		for name := range node.Properties {
			names = append(names, name)
		}
		// Deterministic order: a failure message that reorders between runs is
		// a failure message nobody can diff.
		sort.Strings(names)
		for _, name := range names {
			child, ok := typed[name]
			if !ok {
				continue
			}
			walkEnumNode(node.Properties[name], child, joinEnumPath(path, name), out)
		}
	case []any:
		if node.Items == nil {
			return
		}
		for i, elem := range typed {
			walkEnumNode(node.Items, elem, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

func joinEnumPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// enumAdmits compares by rendered value rather than by Go type. The schema is
// decoded from YAML (an integer arrives as int) and the result from JSON
// (the same integer arrives as float64), so a type-equality check would reject
// values the provider itself accepted.
func enumAdmits(allowed []any, value any) bool {
	got := fmt.Sprintf("%v", value)
	for _, candidate := range allowed {
		if fmt.Sprintf("%v", candidate) == got {
			return true
		}
	}
	return false
}

// fatalEnumViolations returns only the violations that must fail the step,
// or nil when none do. A warn-mode node is counted and logged but never
// fails: see registry.OutputSchema.OnViolation for why analysis.complexity
// is one.
func fatalEnumViolations(violations []enumViolation) []enumViolation {
	var out []enumViolation
	for _, v := range violations {
		if v.Mode == registry.ViolationFail {
			out = append(out, v)
		}
	}
	return out
}

// enumAllowedPreview renders an enum for a failure message, capped. The cap is
// on the ALLOWED SET only — a 19-id pinned enum is normal and rendering it in
// full blows the message out, but truncating the offending value would remove
// the one thing the retry needs.
const enumPreviewLimit = 6

func enumAllowedPreview(allowed []any) string {
	shown := allowed
	suffix := ""
	if len(allowed) > enumPreviewLimit {
		shown = allowed[:enumPreviewLimit]
		suffix = fmt.Sprintf(" … and %d more", len(allowed)-enumPreviewLimit)
	}
	parts := make([]string, 0, len(shown))
	for _, v := range shown {
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	return strings.Join(parts, ", ") + suffix
}

// enumViolationMessage renders violations for the step failure, which is also
// the corrective-retry prompt. It names the path, the value received and the
// allowed set, because a retry told only "invalid enum" is a retry that
// guesses.
//
// REDACT THE VALUE. This message becomes agentError, which reaches the
// executions table, the dashboard, Telegram and downstream prompts — and the
// detection it comes from deliberately runs on rawResultBytes, the
// UNREDACTED form, so that a redaction splice cannot silently turn a real
// violation into a clean pass. That split is the 2026-08-20 "validate before
// redact" rule (longhorizon-flapping-fixes-plan), and its condition is that a
// raw-bytes read must emit structure, never payload content.
//
// An enum violation's value IS payload content, and it is content from exactly
// the fields that design names as ordinary redaction triggers — "case ids,
// commit SHAs, generated identifiers". So the value is scanned and redacted
// here, at the one point where it crosses from detection into something
// persisted. The path and the allowed set come from the schema, never from the
// payload, and need no scan.
func enumViolationMessage(violations []enumViolation, redact func(string) string) string {
	parts := make([]string, 0, len(violations))
	for _, v := range violations {
		value := fmt.Sprintf("%v", v.Value)
		if redact != nil {
			value = redact(value)
		}
		parts = append(parts, fmt.Sprintf("%s = %s (allowed: %s)",
			v.Path, value, enumAllowedPreview(v.Allowed)))
	}
	return "schema violation: value outside the declared set: " + strings.Join(parts, "; ")
}

// redactEnumValue returns the executor's payload-value redactor for enum
// diagnostics, or nil when no detector is wired (tests, and deployments that
// have not configured secret scanning — in which case nothing was being
// redacted anywhere and this changes nothing).
func (e *Executor) redactEnumValue() func(string) string {
	if e == nil || e.secretsDetector == nil {
		return nil
	}
	return func(value string) string {
		findings := e.secretsDetector.Scan([]byte(value))
		if len(findings) == 0 {
			return value
		}
		return string(secrets.Redact([]byte(value), findings))
	}
}

// recordEnumViolations counts every violation, fatal or not, and returns the
// message for the ones that must fail the step (empty when none do).
//
// Warn-mode violations are counted and logged but never returned: the point of
// the mode is that a downstream fallback already resolves the value, and
// failing the step would invert a decision the owning design made under
// incident pressure. They still count, because a control that cannot
// distinguish "examined and clean" from "never examined" reports the first and
// means the second.
func (e *Executor) recordEnumViolations(role string, violations []enumViolation) string {
	if len(violations) == 0 {
		return ""
	}
	for _, v := range violations {
		if e.metrics != nil && e.metrics.OutputEnumViolationTotal != nil {
			e.metrics.OutputEnumViolationTotal.WithLabelValues(role, v.Path).Inc()
		}
		if v.Mode == registry.ViolationWarn {
			e.logger.Warn().
				Str("role", role).
				Str("field", v.Path).
				Interface("value", v.Value).
				Str("allowed", enumAllowedPreview(v.Allowed)).
				Msg("output enum: value outside the declared set, not failing the step (onViolation: warn)")
		}
	}
	fatal := fatalEnumViolations(violations)
	if len(fatal) == 0 {
		return ""
	}
	return enumViolationMessage(fatal, e.redactEnumValue())
}

// checkOutputContract runs BOTH receipt-time output checks in one place, so
// the two sites that enforce them cannot drift apart, and returns the single
// message describing whatever failed.
//
// Precedence: a missing required key wins and the enum violation is appended
// to it. One message carries both defects, so a retry does not have to
// discover the second one on the next attempt.
//
// The enum half is skipped when there is no effective schema; the
// requiredOutputKeys half is NOT — nil EffectiveSchema suppresses the enum
// sub-check ONLY. Gating both on it would silently retire a check that
// predates this one on every recovery hop.
func (e *Executor) checkOutputContract(
	resultBytes []byte,
	roleName string,
	requiredKeys []string,
	schema *registry.OutputSchema,
) string {
	if len(resultBytes) == 0 {
		return ""
	}
	var parts []string
	if len(requiredKeys) > 0 {
		if missing := validateRequiredOutputKeys(resultBytes, requiredKeys); len(missing) > 0 {
			parts = append(parts, fmt.Sprintf("schema violation: role %q result.json is missing required keys: %v",
				roleName, missing))
		}
	}
	if msg := e.recordEnumViolations(roleName, validateEnums(resultBytes, schema)); msg != "" {
		parts = append(parts, msg)
	}
	return strings.Join(parts, "; ")
}
