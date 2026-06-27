package schemaregistry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func mkdir(t *testing.T, dir, name string) error {
	t.Helper()
	return os.Mkdir(filepath.Join(dir, name), 0o700)
}

func rm(t *testing.T, dir, name string) error {
	t.Helper()
	return os.Remove(filepath.Join(dir, name))
}

// strictSchema rejects additional properties and constrains a
// nested object — exercises the strict (extra-field) path and a
// numeric type constraint the executor relies on for CPC
// envelopes.
const strictSchema = `{
  "$id": "strict_doc.v1",
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "count"],
  "properties": {
    "name":  {"type": "string"},
    "count": {"type": "integer", "minimum": 0}
  }
}`

// TestValidate_ExtraFieldRejectedWhenStrict asserts that a
// schema with additionalProperties:false rejects an envelope
// carrying an undeclared field. This is the strict-mode path
// the resolve hook depends on for tool-schema conformance.
func TestValidate_ExtraFieldRejectedWhenStrict(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "strict_doc.v1.json", strictSchema)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	doc := map[string]any{"name": "x", "count": 1, "rogue": true}
	err = reg.Validate("strict_doc.v1", doc)
	if err == nil {
		t.Fatal("expected rejection for undeclared field under additionalProperties:false")
	}
	if !strings.Contains(err.Error(), "additionalProperties") &&
		!strings.Contains(err.Error(), "rogue") {
		t.Errorf("error should name the offending constraint/field, got %q", err.Error())
	}
}

// TestValidate_StrictHappyPathAccepts is the accept counterpart
// to the strict reject test: a document with exactly the
// declared fields passes.
func TestValidate_StrictHappyPathAccepts(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "strict_doc.v1.json", strictSchema)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Validate("strict_doc.v1", map[string]any{"name": "x", "count": 0}); err != nil {
		t.Errorf("conformant strict doc should pass, got %v", err)
	}
}

// TestValidate_NumericConstraintFires asserts a min-value
// constraint rejects an out-of-range value with an error that
// references the constraint.
func TestValidate_NumericConstraintFires(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "strict_doc.v1.json", strictSchema)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate("strict_doc.v1", map[string]any{"name": "x", "count": -5})
	if err == nil {
		t.Fatal("expected rejection for count below minimum")
	}
	if !strings.Contains(err.Error(), "minimum") && !strings.Contains(err.Error(), "count") {
		t.Errorf("error should reference the minimum/count constraint, got %q", err.Error())
	}
}

// TestValidate_NilDocAgainstObjectSchema asserts that a nil
// document validated against an object schema is rejected
// (nil is not an object). Guards the executor's "empty result"
// edge where a CPC returns no payload.
func TestValidate_NilDocAgainstObjectSchema(t *testing.T) {
	reg := loadOneSchema(t)
	if err := reg.Validate("spec_envelope.v1", nil); err == nil {
		t.Error("nil doc should fail an object schema with required fields")
	}
}

// TestValidate_EmptyDocMissesRequired asserts an empty map is
// rejected by a schema that declares required fields. Distinct
// from the nil case: this is a present-but-empty envelope.
func TestValidate_EmptyDocMissesRequired(t *testing.T) {
	reg := loadOneSchema(t)
	err := reg.Validate("spec_envelope.v1", map[string]any{})
	if err == nil {
		t.Error("empty doc should fail required-field constraints")
	}
}

// TestValidate_EmptySchemaAcceptsAnything asserts a schema body
// of `{}` (no constraints) accepts arbitrary documents. The
// empty schema is the JSON-Schema "always true" — useful as a
// registered-but-permissive placeholder.
func TestValidate_EmptySchemaAcceptsAnything(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "anything.json", `{}`)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reg.HasSchema("anything") {
		t.Fatalf("empty schema should still register, IDs=%v", reg.IDs())
	}
	for _, doc := range []any{
		map[string]any{"x": 1},
		[]any{1, 2, 3},
		"a string",
		42.0,
		nil,
	} {
		if err := reg.Validate("anything", doc); err != nil {
			t.Errorf("empty schema should accept %#v, got %v", doc, err)
		}
	}
}

// TestLoad_InvalidJSONReportsMultiError asserts a malformed
// schema file is surfaced as a load error that names the file,
// while valid sibling schemas still load. Covers scan's
// loadErrs aggregation path.
func TestLoad_InvalidJSONReportsMultiError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)
	mustWrite(t, dir, "broken.json", `{ this is not valid json `)

	reg, err := Load(dir)
	if err == nil {
		t.Fatal("expected a load error for the malformed file")
	}
	if !strings.Contains(err.Error(), "broken.json") {
		t.Errorf("error should name the failing file, got %q", err.Error())
	}
	// The valid sibling must still be registered despite the failure.
	if !reg.HasSchema("spec_envelope.v1") {
		t.Errorf("valid sibling schema should still load, IDs=%v", reg.IDs())
	}
	if reg.HasSchema("broken") {
		t.Error("malformed schema must not be registered")
	}
}

// TestLoad_NonJSONFilesIgnored asserts the loader only picks up
// *.json files and skips subdirectories — keeps stray README /
// nested config dirs from breaking the boot scan.
func TestLoad_NonJSONFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)
	mustWrite(t, dir, "README.md", "not a schema")
	mustWrite(t, dir, "notes.txt", "also not a schema")
	if err := mkdir(t, dir, "nested"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load should ignore non-json, got %v", err)
	}
	if reg.Count() != 1 {
		t.Errorf("count = %d, want 1 (only the .json file)", reg.Count())
	}
}

// TestLoad_EmptyDirIsClean asserts an existing-but-empty
// directory yields a clean empty registry with the dir
// remembered for a later Reload.
func TestLoad_EmptyDirIsClean(t *testing.T) {
	dir := t.TempDir()
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.Count() != 0 {
		t.Errorf("count = %d, want 0", reg.Count())
	}
	// dir was recorded, so Reload must not error (unlike New()).
	if err := reg.Reload(); err != nil {
		t.Errorf("Reload on a loaded-from-dir registry should not error, got %v", err)
	}
}

// TestIDs_ReturnsAllRegistered asserts IDs returns every
// registered id. The implementation does not guarantee order
// despite the doc comment, so we compare as a set (sorted copy)
// rather than asserting insertion order.
func TestIDs_ReturnsAllRegistered(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)
	mustWrite(t, dir, "assets.json", `{"type": "object"}`)
	mustWrite(t, dir, "report.json", `{"type": "object"}`)

	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := reg.IDs()
	if len(got) != 3 {
		t.Fatalf("IDs len = %d, want 3: %v", len(got), got)
	}
	sort.Strings(got)
	want := []string{"assets", "report", "spec_envelope.v1"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IDs (sorted) = %v, want %v", got, want)
			break
		}
	}
}

// TestReload_ReplacesNotMerges asserts Reload replaces the
// schema set with the current on-disk contents — a schema whose
// file was removed disappears after Reload. Guards against a
// stale-merge regression in scan's swap.
func TestReload_ReplacesNotMerges(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)
	mustWrite(t, dir, "assets.json", `{"type": "object"}`)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.Count() != 2 {
		t.Fatalf("precondition: count = %d, want 2", reg.Count())
	}
	if err := rm(t, dir, "assets.json"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if reg.HasSchema("assets") {
		t.Error("Reload should have dropped the removed schema (replace, not merge)")
	}
	if !reg.HasSchema("spec_envelope.v1") {
		t.Error("Reload should keep the still-present schema")
	}
}

// TestLoad_ExplicitIDOverridesFilename asserts that a `$id`
// inside the body wins over the filename slug, so a renamed
// file keeps its canonical schema id.
func TestLoad_ExplicitIDOverridesFilename(t *testing.T) {
	dir := t.TempDir()
	// filename slug would be "renamed-file" but $id declares canonical.
	mustWrite(t, dir, "renamed-file.json", `{"$id": "canonical.v2", "type": "object"}`)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reg.HasSchema("canonical.v2") {
		t.Errorf("expected $id to win, IDs=%v", reg.IDs())
	}
	if reg.HasSchema("renamed-file") {
		t.Error("filename slug should not be used when $id is present")
	}
}
