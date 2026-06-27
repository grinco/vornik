// Package assetschema is the curated field-schema spine for the UI
// asset-management feature (projects, swarms, workflows). It is the
// single source of truth that the form renderer, inline documentation,
// defaults, and typed pre-validation all read from.
//
// A build-time drift-guard test (per asset type) asserts that every
// yaml-tagged registry struct field has a schema entry, and that every
// schema Path resolves to a real struct field — so a new parameter can
// never silently become YAML-only, which is the root cause the feature
// exists to fix. See https://docs.vornik.io
package assetschema

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind classifies a field's value type for rendering + validation.
type Kind string

const (
	KindString     Kind = "string"
	KindInt        Kind = "int"
	KindFloat      Kind = "float"
	KindBool       Kind = "bool"
	KindEnum       Kind = "enum"
	KindDuration   Kind = "duration"   // Go duration string, e.g. "30s", "5m"
	KindStringList Kind = "stringlist" // a YAML sequence of scalars
	// KindSubobject marks a nested block (e.g. budget:) or a repeating
	// collection item. Subobjects do not carry a directly-editable scalar
	// value themselves; their leaf fields are separate Field entries with
	// dotted Paths (budget.daily_cap_usd) or live in a collection's
	// item-schema.
	KindSubobject Kind = "subobject"
)

// Backing is where a field's value physically lives on disk. Most fields
// are frontmatter (the project .yaml, or a SWARM/WORKFLOW.md frontmatter
// block). Long prose (a role's systemPrompt, a swarm's rolePrelude) lives
// in the markdown body and routes through the body editor instead of the
// node patcher.
type Backing string

const (
	BackingFrontmatter Backing = "frontmatter"
	BackingBody        Backing = "body"
)

// Field describes one editable parameter.
type Field struct {
	// Path is the dotted YAML key the patcher targets, e.g.
	// "budget.on_exceed". For body-backed fields it is a logical
	// identifier (e.g. "rolePrelude") the body editor understands.
	Path string
	// Label / Help are operator-facing (curated for quality).
	Label string
	Help  string
	Kind  Kind
	// Enum lists allowed values when Kind == KindEnum (rendered as a
	// dropdown; validated for membership).
	Enum []string
	// Default is the value shown as a hint and pre-filled on create
	// (string form regardless of Kind).
	Default string
	// Required fails pre-validation when the submitted value is empty.
	Required bool
	// Advanced fields render inside a collapsed section by default.
	Advanced bool
	// Multiline renders a free-text scalar as a textarea (prose like a
	// swarm's rolePrelude that lives in the frontmatter but reads as a
	// paragraph). Body-backed fields are always multi-line regardless.
	Multiline bool
	// ReadOnly fields are displayed but never bound or patched —
	// identity keys (e.g. projectId) that would rename/orphan the
	// file if edited. The renderer shows them disabled.
	ReadOnly bool
	// Backing defaults to BackingFrontmatter when empty.
	Backing Backing
}

// backing returns the effective backing store (frontmatter by default).
func (f Field) backing() Backing {
	if f.Backing == "" {
		return BackingFrontmatter
	}
	return f.Backing
}

// IsBody reports whether the field is stored in the markdown body.
func (f Field) IsBody() bool { return f.backing() == BackingBody }

// Section groups related fields under a heading. An Advanced section
// collapses by default in the renderer.
type Section struct {
	Title    string
	Help     string
	Advanced bool
	Fields   []Field
}

// AssetSchema is the full editable surface for one asset type.
type AssetSchema struct {
	// Asset is the asset-type key: "project" | "swarm" | "workflow".
	Asset    string
	Sections []Section
	// Collections are repeating sub-objects (Swarm roles[], Workflow
	// steps{}) edited as lists of cards. Empty for flat assets and for
	// a collection's own ItemSchema (collections don't nest).
	Collections []Collection
}

// Fields flattens every field across all sections, in declaration order.
func (s AssetSchema) Fields() []Field {
	var out []Field
	for _, sec := range s.Sections {
		out = append(out, sec.Fields...)
	}
	return out
}

// FieldByPath returns the field with the given dotted path.
func (s AssetSchema) FieldByPath(path string) (Field, bool) {
	for _, sec := range s.Sections {
		for _, f := range sec.Fields {
			if f.Path == path {
				return f, true
			}
		}
	}
	return Field{}, false
}

// Paths returns every field path (used by the drift-guard test).
func (s AssetSchema) Paths() []string {
	fs := s.Fields()
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return out
}

// ValidateValue checks a raw form value (as submitted by the browser)
// against the field's kind, enum, and required-ness. It returns a
// nil error when the value is acceptable, or a precise operator-facing
// error naming the field. Empty non-required values always pass (the
// field is simply omitted / left at its default).
//
// Booleans accept the HTML-checkbox convention: "" / "false" / "off" /
// "0" → false, "true" / "on" / "1" → true.
func (f Field) ValidateValue(raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		if f.Required {
			return fmt.Errorf("%s is required", f.label())
		}
		return nil
	}
	switch f.Kind {
	case KindInt:
		if _, err := strconv.Atoi(v); err != nil {
			return fmt.Errorf("%s must be an integer (got %q)", f.label(), v)
		}
	case KindFloat:
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("%s must be a number (got %q)", f.label(), v)
		}
	case KindBool:
		if !isBoolLiteral(v) {
			return fmt.Errorf("%s must be true or false (got %q)", f.label(), v)
		}
	case KindDuration:
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s must be a duration like 30s or 5m (got %q)", f.label(), v)
		}
	case KindEnum:
		if !containsStr(f.Enum, v) {
			return fmt.Errorf("%s must be one of [%s] (got %q)", f.label(), strings.Join(f.Enum, ", "), v)
		}
	case KindString, KindStringList:
		// Free-form scalar / list: nothing further to validate here.
	case KindSubobject:
		// Subobjects carry no directly-editable scalar.
	default:
		return fmt.Errorf("%s has unknown field kind %q", f.label(), f.Kind)
	}
	return nil
}

// label returns a human label for error messages, falling back to the
// path when no Label was set.
func (f Field) label() string {
	if f.Label != "" {
		return f.Label
	}
	return f.Path
}

func isBoolLiteral(v string) bool {
	switch strings.ToLower(v) {
	case "true", "false", "on", "off", "0", "1", "yes", "no":
		return true
	}
	return false
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
