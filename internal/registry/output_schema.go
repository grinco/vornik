package registry

import (
	"fmt"
	"sort"
	"strings"
)

// OutputSchema is the structured declaration of a role's result.json
// shape. It replaces the prose `Output on success: { ... }` examples
// that historically lived inside `systemPrompt`, and it subsumes the
// `requiredOutputKeys` + `plausibilityRules` fields by deriving both
// at config-load time.
//
// The shape is a small bespoke dialect of JSON Schema — large enough
// to express what vornik roles actually need (typed required paths,
// non-empty strings, conditional non-empty checks), small enough that
// operators don't have to learn the full spec or run a heavyweight
// validator. See https://docs.vornik.io for the
// motivating regressions and the full delivery plan.
//
// Phase 1 (this struct): derive the existing RequiredOutputKeys +
// PlausibilityRules slices from a populated OutputSchema so consumers
// (executor's validateRequiredOutputKeys, EvaluatePlausibility) need
// no changes. Phase 2 will add a renderSchemaForPrompt helper and
// switch consumers to read OutputSchema directly.
type OutputSchema struct {
	// Version is an operator-managed integer that operators bump when
	// the schema's contract changes in a way consumers need to know
	// about (a sub-field renamed, a previously-optional field becomes
	// required, plausibility tightens). Surfaced in the rendered
	// prompt prose, in the JSON Schema output (as `x-vornik-version`,
	// non-standard but harmless to providers), and in task.json's
	// config.responseSchema — so an agent runtime that caches across
	// role-config changes can detect a schema rev and discard stale
	// per-role state.
	//
	// Defaults to 0 (unset). vornik doesn't enforce monotonicity or
	// reject mismatches at runtime today; the field is metadata for
	// the migration trail. Item 13 of
	// https://docs.vornik.io may layer enforcement
	// on top once schemas start actually evolving and the
	// operator-decided semantics for "incompatible version" are clear.
	Version int `yaml:"version,omitempty"`
	// Type is the JSON type for this schema node:
	//   "object", "array", "string", "number", "bool", "any" (or empty).
	// Validator entries the schema generates carry this type so
	// validateRequiredOutputKeys can assert it. Empty means "any
	// non-null value", matching the legacy bare-key behaviour.
	Type string `yaml:"type,omitempty"`
	// Required is the list of property names that must exist on this
	// object. Each entry maps to one (or more, for nested objects)
	// derived RequiredOutputKeys path:type strings — see
	// DeriveRequiredOutputKeys.
	Required []string `yaml:"required,omitempty"`
	// Properties declares the nested schema for each property. Only
	// meaningful when Type=="object". Names not in Required may still
	// appear here — they're optional but typed when present.
	Properties map[string]*OutputSchema `yaml:"properties,omitempty"`
	// Items is the schema each array element must satisfy. Only
	// meaningful when Type=="array".
	Items *OutputSchema `yaml:"items,omitempty"`
	// MinLength enforces a minimum on string values. Today only
	// MinLength=1 is honoured (translated into an implicit
	// plausibility rule that rejects "" — the validator's type
	// check accepts empty strings). Phase 2 may add full numeric
	// bounds.
	MinLength int `yaml:"minLength,omitempty"`
	// Enum constrains a string/number to a fixed value set. Phase 2
	// will wire this into the validator. Today the field is parsed
	// but unused — declaring it now keeps role YAMLs forward-
	// compatible.
	Enum []any `yaml:"enum,omitempty"`
	// Plausibility is the conditional-non-empty block from the
	// existing PlausibilityRules feature, hoisted under the schema
	// so role YAMLs declare shape constraints in one place.
	Plausibility []PlausibilityRule `yaml:"plausibility,omitempty"`
}

// DeriveRequiredOutputKeys walks the schema and returns the
// "path[:type]" strings that the executor's validator currently
// consumes via SwarmRole.RequiredOutputKeys. Top-level required
// names produce one entry each; required names whose schema is an
// object with its own `required:` recurse with a dotted path so
// `writing.written:bool` lands alongside `writing:object`.
//
// Output is sorted for determinism — derivation order would
// otherwise depend on map iteration in Properties traversal.
func (s *OutputSchema) DeriveRequiredOutputKeys() []string {
	if s == nil {
		return nil
	}
	var out []string
	s.walkRequired("", &out)
	sort.Strings(out)
	return out
}

func (s *OutputSchema) walkRequired(prefix string, out *[]string) {
	if s == nil {
		return
	}
	for _, name := range s.Required {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		child := s.Properties[name]
		if child != nil && child.Type != "" {
			*out = append(*out, path+":"+child.Type)
		} else {
			// No type info — emit the path-only form, which the
			// validator interprets as "any non-null value".
			*out = append(*out, path)
		}
		if child != nil && child.Type == "object" && len(child.Required) > 0 {
			child.walkRequired(path, out)
		}
	}
}

// DerivePlausibilityRules returns the schema's explicit `plausibility:`
// block plus any implicit rules introduced by `minLength: 1` (which
// the validator's type check accepts as "" without). Implicit rules
// carry deterministic generated names (`min_length_<dotted_path>`) so
// failures point at the schema clause that triggered them.
//
// Returns nil (not an empty slice) when the schema produces no rules,
// matching the legacy PlausibilityRules zero-value so consumers'
// `len(rules) > 0` guards behave identically with or without
// outputSchema set.
func (s *OutputSchema) DerivePlausibilityRules() []PlausibilityRule {
	if s == nil {
		return nil
	}
	var rules []PlausibilityRule
	if len(s.Plausibility) > 0 {
		rules = append(rules, s.Plausibility...)
	}
	s.walkMinLength("", &rules)
	if len(rules) == 0 {
		return nil
	}
	return rules
}

// RenderForPrompt produces a deterministic prose description of the
// schema suitable for appending to an agent's prompt. The agent reads
// this in place of a hand-written `Output on success: { ... }` block;
// the schema becomes the single source of truth for both validation
// and the model's instructions.
//
// Output is line-based: a header naming the required keys, then per-
// key sub-clauses for typed properties + nested objects + plausibility
// rules. Sorted property names + sorted plausibility-rule order so
// two equivalent schemas produce byte-identical render output (any
// drift becomes a real diff in test snapshots / replay corpora —
// item 12 of https://docs.vornik.io).
//
// Example render for the writer schema:
//
//	Respond with ONLY a JSON object. Required top-level keys (with types):
//	  - message (string, non-empty)
//	  - produced_files (array)
//	  - writing (object)
//	    - writing.written (bool, required)
//	Conditional requirements:
//	  - when writing.written=true: writing.path, produced_files must be non-empty
//	  - when writing.written=false: writing.reason must be non-empty
//
// Renders nothing when the schema is nil or empty (no required keys
// + no plausibility rules) — caller should not append the result.
func (s *OutputSchema) RenderForPrompt() string {
	if s == nil {
		return ""
	}
	derived := s.DeriveRequiredOutputKeys()
	rules := s.DerivePlausibilityRules()
	if len(derived) == 0 && len(rules) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Respond with ONLY a JSON object. No prose, no markdown fences, no preamble.\n")
	if s.Version > 0 {
		// Surface the schema version in the prompt so an LLM that's
		// somehow seen a stale system prompt can spot the mismatch
		// against an evolved spec. Operators reading the prompt also
		// benefit — the version pins which row in the migration
		// trail they're looking at.
		fmt.Fprintf(&b, "(schema v%d)\n", s.Version)
	}

	// Render a JSON skeleton showing the EXACT nested structure the
	// model must emit. Pre-fix this section listed required keys
	// using dot-notation ("research.written") which trained some
	// models to emit `{"research.written": true}` as a flat key —
	// validateRequiredOutputKeys' flat-key fast path then passed
	// the nested check but the parent `research:object` check
	// failed because no `research` key existed. Reproduced
	// 2026-05-08 on dev-swarm + assistant-swarm researcher roles.
	// A JSON skeleton makes the nested structure unambiguous and
	// matches the format models are trained on (millions of JSON
	// examples in pretraining vs. vornik's bespoke dot-path
	// notation).
	if len(s.Required) > 0 || len(s.Properties) > 0 {
		b.WriteString("Your response must match this JSON shape:\n")
		renderJSONSkeleton(&b, s, "")
		b.WriteString("\n")
	}

	if len(rules) > 0 {
		b.WriteString("Conditional requirements (each must hold when its `when` matches):\n")
		for _, rule := range rules {
			b.WriteString("  - ")
			b.WriteString(formatPlausibilityRuleForPrompt(rule))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderJSONSkeleton writes a JSON-shaped illustration of the schema's
// expected output structure. Each key is on its own line with a
// "<type, required|optional>" placeholder for the value, so the
// model sees both the structural nesting (required to produce valid
// shape) AND the type expectations (required for validation).
//
// Output for the researcher schema:
//
//	{
//	  "research": {            // (object, required)
//	    "written": <bool>,     // required
//	    "sources": <array>,    // optional
//	    "summary": <string>    // optional
//	  },
//	  "produced_files": <array>  // (required)
//	}
//
// Required vs optional is computed by checking if the key is in the
// parent's Required slice. Sorted property iteration so the output
// is deterministic across runs (Go map iteration is randomised) and
// the prompt cache lands consistently.
func renderJSONSkeleton(b *strings.Builder, s *OutputSchema, indent string) {
	if s == nil {
		return
	}
	b.WriteString("{\n")
	requiredSet := make(map[string]struct{}, len(s.Required))
	for _, r := range s.Required {
		requiredSet[r] = struct{}{}
	}
	// Stable ordering: required keys first (in their declared order
	// so operator intent shows through), then optional keys
	// alphabetically.
	allKeys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		allKeys = append(allKeys, k)
	}
	sort.Strings(allKeys)
	requiredKeys := make([]string, 0, len(s.Required))
	requiredKeys = append(requiredKeys, s.Required...)
	optionalKeys := make([]string, 0, len(allKeys))
	for _, k := range allKeys {
		if _, isRequired := requiredSet[k]; !isRequired {
			optionalKeys = append(optionalKeys, k)
		}
	}
	keys := append(requiredKeys, optionalKeys...)

	childIndent := indent + "  "
	lastIdx := len(keys) - 1
	for i, name := range keys {
		child := s.Properties[name]
		_, isRequired := requiredSet[name]

		b.WriteString(childIndent)
		fmt.Fprintf(b, "%q: ", name)
		writeJSONSkeletonValue(b, child, childIndent, isRequired)
		if i < lastIdx {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(indent)
	b.WriteString("}")
}

// writeJSONSkeletonValue writes one value placeholder, recursing for
// nested object schemas. Non-object types render as `<type>` so the
// model sees JSON shape + type hint.
func writeJSONSkeletonValue(b *strings.Builder, s *OutputSchema, indent string, isRequired bool) {
	if s == nil {
		// Unknown — show any-value with a required marker.
		b.WriteString("<any>")
		if isRequired {
			b.WriteString(" /* required */")
		}
		return
	}
	if s.Type == "object" && len(s.Properties) > 0 {
		renderJSONSkeleton(b, s, indent)
		if isRequired {
			b.WriteString(" /* required */")
		}
		return
	}
	if s.Type == "array" && s.Items != nil {
		b.WriteString("[")
		writeJSONSkeletonValue(b, s.Items, indent, false)
		b.WriteString("]")
		if isRequired {
			b.WriteString(" /* required */")
		}
		return
	}
	typeLabel := s.Type
	if typeLabel == "" {
		typeLabel = "any"
	}
	if s.Type == "string" && s.MinLength == 1 {
		typeLabel = "string, non-empty"
	}
	if len(s.Enum) > 0 {
		enums := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			enums = append(enums, fmt.Sprintf("%v", v))
		}
		typeLabel = "one of: " + strings.Join(enums, ", ")
	}
	fmt.Fprintf(b, "<%s>", typeLabel)
	if isRequired {
		b.WriteString(" /* required */")
	}
}

// formatPlausibilityRuleForPrompt renders one rule as a single line.
// Empty `when` becomes "always"; non-empty becomes "when k=v[ AND k=v]"
// to read naturally inside the prose block.
func formatPlausibilityRuleForPrompt(rule PlausibilityRule) string {
	var when string
	if len(rule.When) == 0 {
		when = "always"
	} else {
		// Sort when-clause keys for deterministic rendering — Go map
		// iteration is randomised and a flapping render would land in
		// the prompt cache differently each time.
		keys := make([]string, 0, len(rule.When))
		for k := range rule.When {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		conds := make([]string, 0, len(keys))
		for _, k := range keys {
			conds = append(conds, fmt.Sprintf("%s=%v", k, rule.When[k]))
		}
		when = "when " + strings.Join(conds, " AND ")
	}
	require := strings.Join(rule.Require, ", ")
	if rule.Name != "" {
		return fmt.Sprintf("%s: %s must be present and non-empty (rule %q)", when, require, rule.Name)
	}
	return fmt.Sprintf("%s: %s must be present and non-empty", when, require)
}

// ToJSONSchema converts the vornik-bespoke OutputSchema dialect into
// a standard JSON Schema map, suitable for handing to an LLM gateway
// that accepts a `response_format: { type: "json_schema", json_schema:
// ... }` directive (OpenAI, Bedrock Converse) or a tool-call schema
// (Anthropic, others).
//
// Mapping rules — each chosen so the conversion is lossless for the
// dialect's expressive surface and so the result lands in the
// JSON-Schema subset all the major providers actually accept (no
// draft-2019 conditionals, no $defs, no anchors):
//
//   - `type: bool` → `"boolean"` (JSON Schema's name).
//   - `type: number` → `"number"`. Other types pass through verbatim.
//   - `required` / `properties` map 1:1.
//   - `items` recurses through the same converter.
//   - `minLength` and `enum` pass through verbatim.
//   - `additionalProperties: false` is added on every object so the
//     provider rejects unknown keys — without this, a model that
//     emits extra fields trips no validation despite the schema's
//     intent. Operators who want loose objects can override at the
//     dialect layer (item 13 backlog: schema-level escape hatch).
//
// The dialect-only `plausibility:` block is intentionally NOT
// converted: JSON Schema's conditional support (draft 2019-09's
// if/then/else) isn't uniformly supported across providers, and our
// existing EvaluatePlausibility runs post-receipt anyway. Provider
// schema enforces shape; plausibility enforces content semantics. The
// two tiers are complementary.
//
// Returns nil for nil receiver and for schemas that produce no
// usable shape (no type set + no required + no properties), so
// callers can use `if s := ...; s != nil` to gate provider-side
// enforcement on a meaningful schema.
//
// Item 7 of https://docs.vornik.io
func (s *OutputSchema) ToJSONSchema() map[string]any {
	if s == nil {
		return nil
	}
	out := convertSchemaNode(s)
	if len(out) == 0 {
		return nil
	}
	// Surface the operator-managed version under x-vornik-version
	// (the JSON-Schema `x-` extension prefix is allowed by the spec
	// and providers ignore unknown fields — safe to attach without
	// confusing tool-use schema validators). Only emit when set;
	// version 0 is the unset sentinel.
	if s.Version > 0 {
		out["x-vornik-version"] = s.Version
	}
	return out
}

func convertSchemaNode(s *OutputSchema) map[string]any {
	if s == nil {
		return nil
	}
	out := map[string]any{}
	switch s.Type {
	case "":
		// No type info — emit nothing for the type field; provider
		// treats the node as accept-anything. Distinct from the
		// nil-receiver case above which signals "no schema at all".
	case "bool":
		out["type"] = "boolean"
	default:
		out["type"] = s.Type
	}
	if len(s.Required) > 0 {
		// Copy so the caller can't mutate our slice and have it
		// reflect back into the schema-derived JSON.
		req := make([]string, len(s.Required))
		copy(req, s.Required)
		out["required"] = req
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		// Sort property names for deterministic output — providers
		// don't care, but cache hits + diff hygiene do.
		names := make([]string, 0, len(s.Properties))
		for name := range s.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := s.Properties[name]
			if child == nil {
				continue
			}
			props[name] = convertSchemaNode(child)
		}
		out["properties"] = props
		// Object types get additionalProperties: false so providers
		// reject extra keys. The dialect's plausibility layer can't
		// catch "model emitted an unexpected key" — the provider can.
		if s.Type == "object" {
			out["additionalProperties"] = false
		}
	}
	if s.Items != nil {
		out["items"] = convertSchemaNode(s.Items)
	}
	if s.MinLength > 0 {
		out["minLength"] = s.MinLength
	}
	if len(s.Enum) > 0 {
		enum := make([]any, len(s.Enum))
		copy(enum, s.Enum)
		out["enum"] = enum
	}
	return out
}

// ToolSpec describes a synthetic tool the agent runtime can register
// with the LLM gateway so a model that's been instructed to "call the
// emit_result tool" produces an output the provider has already
// validated against the schema. The strongest portable enforcement
// available — works on every provider that supports tool use
// (Anthropic, OpenAI, Bedrock Converse, Vertex / Gemini), since
// providers all validate tool-call args against the declared
// parameters JSON Schema before returning the call to the client.
//
// Daemon emits this in task.json under config.resultEmissionTool when
// the role has an outputSchema; the agent runtime decides whether to
// register it. Item 9 of https://docs.vornik.io
type ToolSpec struct {
	// Name is the function-call identifier the LLM emits. Stable per
	// role so multi-shot conversations and replay harnesses can match
	// calls back to a role without juggling synthetic IDs.
	Name string `json:"name"`
	// Description is the one-line LLM-facing prose that tells the
	// model when to use this tool. Strong, imperative phrasing
	// because tool-use models pay a lot of attention to the
	// description for routing.
	Description string `json:"description"`
	// Parameters is the JSON Schema for the tool's input. The schema
	// IS the result.json — when the model calls emit_result(...),
	// the args are exactly what the daemon's validator would receive
	// as result.json. Stripping the level of indirection.
	Parameters map[string]any `json:"parameters"`
}

// ToToolSpec returns a ToolSpec built from this schema, suitable for
// surfacing in task.json so the agent runtime can register it with
// the LLM gateway. Returns nil for nil receiver and for schemas that
// don't produce a usable JSON Schema body (no required + no
// properties + no type), so callers can gate registration on `if t :=
// ...; t != nil`.
//
// Naming rule: emit_<role>_result. Stable across runs (no random
// suffix) so replay harnesses + audit trails see consistent call
// names. Role name is provided by the caller because the schema
// doesn't carry the role identity.
//
// Item 9 of https://docs.vornik.io
func (s *OutputSchema) ToToolSpec(roleName string) *ToolSpec {
	if s == nil {
		return nil
	}
	params := s.ToJSONSchema()
	if params == nil {
		return nil
	}
	name := "emit_" + roleName + "_result"
	desc := "Emit the role's structured result. Call this tool exactly once with the final answer; the args ARE the result.json the daemon validates. Do NOT also produce a free-form text response."
	return &ToolSpec{
		Name:        name,
		Description: desc,
		Parameters:  params,
	}
}

// DeclaresPath reports whether the schema declares a (possibly
// nested) property at the given dotted path. Used by the
// workflow-gate compatibility check (item 11 of
// https://docs.vornik.io): a workflow step that
// gates on `testing.passed == true` must use a role whose schema
// declares `testing.passed` somewhere in its properties tree.
//
// "Declares" is broader than "requires" — a schema with `passed:
// {type: bool}` under `testing.properties` produces true here even
// when `passed` isn't in the `required:` list. The model can still
// emit the field; the gate condition just won't match if it's
// absent. Strict-required mismatches are a separate (and more
// brittle) check we may layer on later.
//
// Returns true for the empty path (a degenerate "self") to keep
// callers from needing a guard for that case.
func (s *OutputSchema) DeclaresPath(path string) bool {
	if s == nil {
		return false
	}
	if path == "" {
		return true
	}
	cur := s
	for _, segment := range strings.Split(path, ".") {
		if cur == nil {
			return false
		}
		next, ok := cur.Properties[segment]
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

func (s *OutputSchema) walkMinLength(prefix string, rules *[]PlausibilityRule) {
	if s == nil || len(s.Properties) == 0 {
		return
	}
	// Sort property names for deterministic rule ordering — a
	// future delivery comparing two derivation outputs (e.g. the
	// replay corpus, item 12 of the design doc) would otherwise
	// see rule-order churn for a no-op schema change.
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := s.Properties[name]
		if child == nil {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if child.Type == "string" && child.MinLength == 1 {
			*rules = append(*rules, PlausibilityRule{
				Name:    "min_length_" + strings.ReplaceAll(path, ".", "_"),
				Require: []string{path},
			})
		}
		if child.Type == "object" {
			child.walkMinLength(path, rules)
		}
	}
}
