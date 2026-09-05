package config

import (
	"reflect"
	"strings"
)

// WalkLeaves visits every yaml-tagged leaf of a config value, depth first, in
// declaration order, with its dotted key. One walk for the two consumers that
// used to have their own idea of "every key": cmd/docs-gen's configuration
// reference (which additionally filters to doc-tagged fields) and the
// resolved-config dump (which wants them all). The rule is the same for both,
// so a key cannot be documented and undumpable or the reverse: an exported
// field with a yaml tag is a key; one without is not (deny-by-default — an
// untagged field is internal).
//
// A leaf is a field whose type is not a struct, or a struct the walk treats
// as a value (time.Duration-like, or one with a doc tag — see IsLeafStruct).
// Pointers are followed when non-nil; a nil pointer to a struct is walked as
// the zero value so its keys still enumerate (a section the operator never
// wrote still has keys, all unset).
func WalkLeaves(v reflect.Value, fn func(key string, f reflect.StructField, v reflect.Value)) {
	walkLeaves(v, "", fn)
}

func walkLeaves(v reflect.Value, prefix string, fn func(string, reflect.StructField, reflect.Value)) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v = reflect.Zero(v.Type().Elem())
			break
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		fv := v.Field(i)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !IsLeafStruct(ft) && f.Tag.Get("doc") == "" {
			walkLeaves(fv, key, fn)
			continue
		}
		fn(key, f, fv)
	}
}

// IsLeafStruct reports whether a struct type is treated as a single value
// rather than a section: types with no yaml-tagged exported fields (e.g. a
// wrapper around a duration) have nothing to recurse into.
func IsLeafStruct(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if name := strings.Split(f.Tag.Get("yaml"), ",")[0]; name != "" && name != "-" {
			return false
		}
	}
	return true
}
