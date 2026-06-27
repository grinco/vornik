package schemaregistry

import (
	"os"
	"path/filepath"
	"testing"
)

const specEnvelopeV1 = `{
  "$id": "spec_envelope.v1",
  "type": "object",
  "required": ["schema", "status", "data"],
  "properties": {
    "schema":  {"type": "string", "const": "spec_envelope.v1"},
    "status":  {"type": "string"},
    "summary": {"type": "string"},
    "data": {
      "type": "object",
      "required": ["kpis"],
      "properties": {
        "kpis": {"type": "array", "items": {"type": "string"}}
      }
    }
  }
}`

// TestLoad_ReadsValidSchemas asserts the loader picks up every
// *.json file under the dir and exposes them via Get/HasSchema.
func TestLoad_ReadsValidSchemas(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)

	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.Count() != 1 {
		t.Errorf("count = %d, want 1", reg.Count())
	}
	if !reg.HasSchema("spec_envelope.v1") {
		t.Error("HasSchema should be true for the $id we declared")
	}
	if reg.Get("spec_envelope.v1") == nil {
		t.Error("Get should return the compiled schema")
	}
}

// TestLoad_MissingDirIsClean covers the secure default —
// deployments without configs/schemas/ get an empty registry
// and the resolve hook falls through to envelope-shape check.
func TestLoad_MissingDirIsClean(t *testing.T) {
	reg, err := Load("/nonexistent/path/schemas")
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if reg.Count() != 0 {
		t.Errorf("count = %d, want 0", reg.Count())
	}
}

// TestLoad_FilenameDefaultID asserts a schema without an
// explicit $id picks up its filename as the id. Lets authors
// drop in a quick prototype without the boilerplate.
func TestLoad_FilenameDefaultID(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "assets.json", `{"type": "object"}`)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reg.HasSchema("assets") {
		t.Errorf("expected id from filename, got IDs=%v", reg.IDs())
	}
}

// TestValidate_HappyPath covers the conformant envelope path.
func TestValidate_HappyPath(t *testing.T) {
	reg := loadOneSchema(t)
	envelope := map[string]any{
		"schema": "spec_envelope.v1",
		"status": "ok",
		"data":   map[string]any{"kpis": []any{"click_rate", "conv_rate"}},
	}
	if err := reg.Validate("spec_envelope.v1", envelope); err != nil {
		t.Errorf("happy envelope should pass, got %v", err)
	}
}

// TestValidate_MissingRequiredField asserts the schema's
// required-field constraints fire. The data.kpis field is
// required by the example schema; omitting it must error.
func TestValidate_MissingRequiredField(t *testing.T) {
	reg := loadOneSchema(t)
	envelope := map[string]any{
		"schema": "spec_envelope.v1",
		"status": "ok",
		"data":   map[string]any{}, // missing kpis
	}
	err := reg.Validate("spec_envelope.v1", envelope)
	if err == nil {
		t.Error("expected validation error for missing required field, got nil")
	}
}

// TestValidate_WrongType asserts type constraints fire.
// kpis must be array<string>; passing an int array fails.
func TestValidate_WrongType(t *testing.T) {
	reg := loadOneSchema(t)
	envelope := map[string]any{
		"schema": "spec_envelope.v1",
		"status": "ok",
		"data":   map[string]any{"kpis": []any{1, 2}},
	}
	if err := reg.Validate("spec_envelope.v1", envelope); err == nil {
		t.Error("expected type-mismatch error, got nil")
	}
}

// TestValidate_UnknownSchemaPassesThrough asserts the "no
// schema registered" path returns nil — the resolve hook
// falls back to envelope-shape validation. Critical for
// deployments that haven't authored schema files yet.
func TestValidate_UnknownSchemaPassesThrough(t *testing.T) {
	reg := loadOneSchema(t)
	envelope := map[string]any{"schema": "anything"}
	if err := reg.Validate("unregistered_envelope.v1", envelope); err != nil {
		t.Errorf("unknown schema should pass through, got %v", err)
	}
}

// TestReload_PicksUpChanges asserts that adding a new schema
// file and calling Reload makes it visible to subsequent
// Validate calls.
func TestReload_PicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.HasSchema("assets") {
		t.Fatal("assets should not exist yet")
	}
	mustWrite(t, dir, "assets.json", `{"type": "object"}`)
	if err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !reg.HasSchema("assets") {
		t.Error("Reload should have picked up assets.json")
	}
}

// TestReload_WithoutDirErrors covers the case where the
// registry was constructed with New() (no dir on file).
// Reload must surface an error rather than silently no-op.
func TestReload_WithoutDirErrors(t *testing.T) {
	reg := New()
	if err := reg.Reload(); err == nil {
		t.Error("expected error from Reload on a registry without a directory")
	}
}

// TestNilRegistry_IsSafe asserts every public method is safe
// to call on a nil receiver — the executor's hot path
// short-circuits this way when the registry isn't wired.
func TestNilRegistry_IsSafe(t *testing.T) {
	var reg *Registry
	if reg.Get("anything") != nil {
		t.Error("nil.Get should return nil")
	}
	if reg.HasSchema("anything") {
		t.Error("nil.HasSchema should be false")
	}
	if reg.Count() != 0 {
		t.Error("nil.Count should be 0")
	}
	if reg.IDs() != nil {
		t.Error("nil.IDs should be nil")
	}
	if err := reg.Validate("anything", map[string]any{}); err != nil {
		t.Errorf("nil.Validate should be nil, got %v", err)
	}
}

// --- helpers ---

func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func loadOneSchema(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, dir, "spec_envelope.v1.json", specEnvelopeV1)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return reg
}
