package config

import (
	"reflect"
	"strings"
)

// LookupByPath resolves a dotted yaml-tag path (e.g. "instinct.enabled",
// "instinct.consumers.application_feedback", "api.auth_enabled") against
// a *Config struct by walking the struct field tags. It returns the leaf
// field value and true, or nil/false when the path cannot be resolved.
//
// Only struct fields whose yaml tag matches the current segment are
// followed; unexported or non-struct fields are skipped at intermediate
// nodes. The returned value is the field's current value cast to any;
// the concrete type mirrors the Go field type (bool, string, int, etc.).
func LookupByPath(cfg *Config, dotted string) (any, bool) {
	if cfg == nil || dotted == "" {
		return nil, false
	}
	segments := strings.Split(dotted, ".")
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	return walkPath(v, segments)
}

// walkPath recursively resolves segments against a struct Value.
func walkPath(v reflect.Value, segments []string) (any, bool) {
	if len(segments) == 0 {
		return nil, false
	}
	if v.Kind() != reflect.Struct {
		return nil, false
	}
	seg := segments[0]
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("yaml")
		// yaml tags may have options like "omitempty"; use only the name.
		name := strings.Split(tag, ",")[0]

		// Traverse anonymous (embedded) struct fields and fields tagged
		// yaml:",inline" transparently: they don't consume a path segment,
		// so recurse into them using the current segments unchanged.
		if field.Anonymous || name == "" && strings.Contains(tag, "inline") {
			fv := v.Field(i)
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				if val, ok := walkPath(fv, segments); ok {
					return val, ok
				}
			}
			continue
		}

		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if name != seg {
			continue
		}
		fv := v.Field(i)
		// Dereference a pointer field.
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				return nil, false
			}
			fv = fv.Elem()
		}
		if len(segments) == 1 {
			// Leaf: return the value.
			if fv.CanInterface() {
				return fv.Interface(), true
			}
			return nil, false
		}
		// Intermediate: must be a struct.
		if fv.Kind() == reflect.Struct {
			return walkPath(fv, segments[1:])
		}
		return nil, false
	}
	return nil, false
}
