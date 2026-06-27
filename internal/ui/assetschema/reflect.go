package assetschema

import (
	"reflect"
	"strings"
)

// LeafPaths reflects over a struct value and returns every editable
// YAML leaf path in dotted form (e.g. "budget.daily_cap_usd"), recursing
// into nested struct blocks. It is the basis of the drift-guard contract:
// the set of paths a schema must cover (or explicitly defer).
//
// Rules:
//   - Only fields with a yaml tag are considered; yaml:"-" is skipped
//     (e.g. Project.Brief, loaded from a companion file, not editable here).
//   - A nested struct block recurses with a dotted prefix.
//   - Scalars, []string and other slices/maps are LEAVES (a list field is
//     edited as a unit; opaque slices like []map[string]any are a single
//     editable surface).
//   - Pointers are followed to their element type.
//   - time.Time and types implementing encoding.TextUnmarshaler are leaves,
//     not recursed (they serialize as scalars).
func LeafPaths(v any) []string {
	var out []string
	collectLeafPaths(reflect.TypeOf(v), "", &out, map[reflect.Type]bool{})
	return out
}

// collectLeafPaths walks a struct type emitting dotted yaml leaf paths.
// seen holds the struct types on the current recursion PATH (not a global
// visited set): a type re-entered while already on the path is a
// recursive type (e.g. registry.OutputSchema, a JSON-schema shape that
// nests itself) — we stop and treat that field as a single leaf rather
// than recursing forever. Path-based tracking (mark on enter, unmark on
// exit) means two sibling fields of the same struct type both expand;
// only an ancestor of the same type terminates the recursion.
func collectLeafPaths(t reflect.Type, prefix string, out *[]string, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		key := yamlKey(f)
		if key == "" || key == "-" {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !isScalarStruct(ft) && !seen[ft] {
			collectLeafPaths(ft, path, out, seen)
			continue
		}
		// Scalar, list, map, or a recursive struct already on the path:
		// edited as a single unit.
		*out = append(*out, path)
	}
}

// yamlKey extracts the bare key from a yaml struct tag, dropping options
// like ",omitempty" / ",inline".
func yamlKey(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return ""
	}
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		tag = tag[:comma]
	}
	return strings.TrimSpace(tag)
}

// isScalarStruct reports structs that serialize as a scalar and should be
// treated as leaves rather than recursed (e.g. time.Time). We detect this
// structurally by name to avoid importing every such type.
func isScalarStruct(t reflect.Type) bool {
	switch t.String() {
	case "time.Time", "time.Duration":
		return true
	}
	return false
}
