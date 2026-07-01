package config

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSetYAMLKey_CreatedSignal asserts the `created` return distinguishes
// appending a brand-new key from updating an existing one — the signal the
// feature-doctor / UI YAML-editor use to warn on a likely typo'd/unknown
// key (a silent append produces a dead config entry that still parses).
func TestSetYAMLKey_CreatedSignal(t *testing.T) {
	base := []byte("instinct:\n  model: old\n  consumers:\n    application_feedback: false\n")

	// Updating an existing leaf → created=false.
	if _, created, err := SetYAMLKey(base, "instinct.model", "new"); err != nil || created {
		t.Errorf("updating existing key: created=%v err=%v; want created=false", created, err)
	}
	if _, created, err := SetYAMLKey(base, "instinct.consumers.application_feedback", true); err != nil || created {
		t.Errorf("updating existing nested key: created=%v err=%v; want created=false", created, err)
	}

	// Appending a new leaf under an existing mapping → created=true.
	if _, created, err := SetYAMLKey(base, "instinct.consumers.memory_hygiene", true); err != nil || !created {
		t.Errorf("new leaf under existing block: created=%v err=%v; want created=true", created, err)
	}

	// Creating a key whose entire parent path is missing → created=true.
	if _, created, err := SetYAMLKey(base, "blackbox.replay_safe_tools_enabled", true); err != nil || !created {
		t.Errorf("new key with missing parent path: created=%v err=%v; want created=true", created, err)
	}

	// An unsupported value type creates nothing and errors.
	if _, created, err := SetYAMLKey(base, "instinct.brand_new", 3.14); err == nil || created {
		t.Errorf("unsupported type: created=%v err=%v; want created=false + error", created, err)
	}
}

func TestSetKeyPreservesComments(t *testing.T) {
	in, _ := os.ReadFile("testdata/commented.yaml")
	out, _, err := SetYAMLKey(in, "instinct.consumers.application_feedback", true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "# consumers block") || !strings.Contains(s, "# inline comment") {
		t.Fatal("comments must be preserved")
	}
	if !strings.Contains(s, "application_feedback: true") {
		t.Fatal("target key not updated")
	}
	if strings.Count(s, "application_feedback") != 1 {
		t.Fatal("must update in place, not duplicate")
	}
}

// TestSetYAMLKey_CreatesMissingLeaf: a missing leaf is appended to its
// existing parent mapping (writer-AND-creator).
func TestSetYAMLKey_CreatesMissingLeaf(t *testing.T) {
	in := []byte("instinct:\n  enabled: true\n")
	out, _, err := SetYAMLKey(in, "instinct.application_feedback", true)
	if err != nil {
		t.Fatalf("expected missing leaf to be created, got error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "application_feedback: true") {
		t.Fatalf("missing leaf not created, got:\n%s", s)
	}
	if !strings.Contains(s, "enabled: true") {
		t.Fatal("existing sibling key lost")
	}
}

// TestSetYAMLKey_CreatesMissingIntermediate: missing intermediate mappings are
// created along the path, then the leaf set.
func TestSetYAMLKey_CreatesMissingIntermediate(t *testing.T) {
	in := []byte("instinct:\n  enabled: true\n")
	out, _, err := SetYAMLKey(in, "instinct.consumers.application_feedback", true)
	if err != nil {
		t.Fatalf("expected intermediate map to be created, got error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "consumers:") || !strings.Contains(s, "application_feedback: true") {
		t.Fatalf("intermediate map/leaf not created, got:\n%s", s)
	}
}

// TestSetYAMLKey_AppendPreservesComments: creating a new leaf in a commented
// block leaves existing comments intact.
func TestSetYAMLKey_AppendPreservesComments(t *testing.T) {
	in, _ := os.ReadFile("testdata/commented.yaml")
	out, _, err := SetYAMLKey(in, "instinct.consumers.memory_hygiene", true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "# consumers block") || !strings.Contains(s, "# inline comment") {
		t.Fatal("comments must be preserved when appending a new key")
	}
	if !strings.Contains(s, "memory_hygiene: true") {
		t.Fatalf("new key not appended, got:\n%s", s)
	}
}

// TestSetYAMLKey_StringValue checks that a string value is set correctly.
func TestSetYAMLKey_StringValue(t *testing.T) {
	in := []byte("instinct:\n  model: old\n")
	out, _, err := SetYAMLKey(in, "instinct.model", "new-model")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "model: new-model") {
		t.Fatalf("string value not set, got:\n%s", s)
	}
}

// TestSetYAMLKey_IntValue checks that an integer value is set correctly.
func TestSetYAMLKey_IntValue(t *testing.T) {
	in := []byte("instinct:\n  limit: 5\n")
	out, _, err := SetYAMLKey(in, "instinct.limit", 42)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "limit: 42") {
		t.Fatalf("int value not set, got:\n%s", s)
	}
}

// TestSetYAMLKey_NestedPath verifies a deeply nested key update round-trips cleanly.
func TestSetYAMLKey_NestedPath(t *testing.T) {
	in, _ := os.ReadFile("testdata/commented.yaml")
	out, _, err := SetYAMLKey(in, "instinct.consumers.failure_playbooks", false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "failure_playbooks: false") {
		t.Fatalf("nested key not updated, got:\n%s", s)
	}
	// top comment should still be present
	if !strings.Contains(s, "# top comment") {
		t.Fatal("top comment lost")
	}
}

// TestSetYAMLKey_InvalidYAML ensures unmarshal errors are surfaced.
func TestSetYAMLKey_InvalidYAML(t *testing.T) {
	_, _, err := SetYAMLKey([]byte(":\t:bad yaml\n"), "key", true)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

// TestSetYAMLKey_UnsupportedType ensures unsupported value types return an error.
func TestSetYAMLKey_UnsupportedType(t *testing.T) {
	in := []byte("instinct:\n  enabled: true\n")
	_, _, err := SetYAMLKey(in, "instinct.enabled", 3.14)
	if err == nil {
		t.Fatal("expected error for unsupported type float64")
	}
}

// TestSetYAMLKey_NonMappingIntermediate ensures an error is returned when
// descending into a non-mapping node (e.g. a scalar used as a path segment).
func TestSetYAMLKey_NonMappingIntermediate(t *testing.T) {
	// "instinct" is a scalar here, not a mapping — cannot descend into it.
	in := []byte("instinct: scalar\n")
	_, _, err := SetYAMLKey(in, "instinct.enabled", true)
	if err == nil {
		t.Fatal("expected error when intermediate is not a mapping")
	}
}

// TestSetYAMLKey_StringSliceReplacesScalarLeaf — the gen-config bootstrap sets
// api.api_keys (a list) from a []string. The leaf, which the example ships as a
// placeholder list, must be rewritten as a sequence of quoted strings, parse
// back to the same slice, and preserve surrounding comments/keys.
func TestSetYAMLKey_StringSliceReplacesScalarLeaf(t *testing.T) {
	in := []byte("api:\n  # keep this comment\n  auth_enabled: true\n  api_keys:\n    - \"replace-me\"\n")
	out, created, err := SetYAMLKey(in, "api.api_keys", []string{"sk-vornik-abc.def", "sk-vornik-xyz.ghi"})
	if err != nil {
		t.Fatalf("SetYAMLKey []string: %v", err)
	}
	if created {
		t.Errorf("created=true, want false (api_keys already existed)")
	}
	if !strings.Contains(string(out), "# keep this comment") {
		t.Errorf("comment not preserved:\n%s", out)
	}
	var parsed struct {
		API struct {
			AuthEnabled bool     `yaml:"auth_enabled"`
			APIKeys     []string `yaml:"api_keys"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !parsed.API.AuthEnabled {
		t.Errorf("auth_enabled flipped to false")
	}
	if len(parsed.API.APIKeys) != 2 || parsed.API.APIKeys[0] != "sk-vornik-abc.def" {
		t.Errorf("api_keys = %v, want the two injected keys", parsed.API.APIKeys)
	}
}

// TestSetYAMLKey_StringSliceCreatesMissingLeaf — a []string leaf that doesn't
// yet exist is appended as a sequence.
func TestSetYAMLKey_StringSliceCreatesMissingLeaf(t *testing.T) {
	in := []byte("api:\n  auth_enabled: true\n")
	out, created, err := SetYAMLKey(in, "api.api_keys", []string{"sk-vornik-abc.def"})
	if err != nil {
		t.Fatalf("SetYAMLKey: %v", err)
	}
	if !created {
		t.Errorf("created=false, want true (api_keys was absent)")
	}
	var parsed struct {
		API struct {
			APIKeys []string `yaml:"api_keys"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(parsed.API.APIKeys) != 1 || parsed.API.APIKeys[0] != "sk-vornik-abc.def" {
		t.Errorf("api_keys = %v, want one injected key", parsed.API.APIKeys)
	}
}

// TestGetYAMLString_ScalarPresent is the happy path: a scalar at a nested
// dotted key is returned verbatim. This is the read-side counterpart to
// SetYAMLKey and the primary primitive migrate-ce uses to pull CE's
// database.* coordinates out of config.yaml before the preflight connect.
func TestGetYAMLString_ScalarPresent(t *testing.T) {
	in := []byte("database:\n  host: 127.0.0.1\n  port: 5432\n  name: vornik\n  user: vornik\n")
	if got := GetYAMLString(in, "database.host"); got != "127.0.0.1" {
		t.Errorf("database.host = %q, want %q", got, "127.0.0.1")
	}
	if got := GetYAMLString(in, "database.port"); got != "5432" {
		t.Errorf("database.port = %q, want %q", got, "5432")
	}
	if got := GetYAMLString(in, "database.name"); got != "vornik" {
		t.Errorf("database.name = %q, want %q", got, "vornik")
	}
}

// TestGetYAMLString_AbsentKey returns "" for a missing key (the signal
// migrate-ce's orDefault logic relies on to apply a fallback).
func TestGetYAMLString_AbsentKey(t *testing.T) {
	in := []byte("database:\n  host: 127.0.0.1\n")
	if got := GetYAMLString(in, "database.port"); got != "" {
		t.Errorf("missing database.port = %q, want empty", got)
	}
	if got := GetYAMLString(in, "server.port"); got != "" {
		t.Errorf("missing top-level server.port = %q, want empty", got)
	}
}

// TestGetYAMLString_NonScalarReturnsEmpty: descending into a mapping or
// sequence must not return a garbage value — only scalars are readable.
func TestGetYAMLString_NonScalarReturnsEmpty(t *testing.T) {
	in := []byte("database:\n  host: 127.0.0.1\n")
	if got := GetYAMLString(in, "database"); got != "" {
		t.Errorf("database (a mapping) = %q, want empty", got)
	}
}

// TestGetYAMLString_InvalidYAML returns "" rather than panicking —
// migrate-ce tolerates a malformed config.yaml by falling back to defaults.
func TestGetYAMLString_InvalidYAML(t *testing.T) {
	if got := GetYAMLString([]byte(":\t:bad\n"), "database.host"); got != "" {
		t.Errorf("invalid yaml = %q, want empty", got)
	}
}

// TestGetYAMLString_RoundTripWithSet: a value written by SetYAMLKey is
// readable back by GetYAMLString — the two helpers share the dotted-key
// convention and must agree.
func TestGetYAMLString_RoundTripWithSet(t *testing.T) {
	in := []byte("database:\n  host: old\n")
	out, _, err := SetYAMLKey(in, "database.host", "db.example.com")
	if err != nil {
		t.Fatalf("SetYAMLKey: %v", err)
	}
	if got := GetYAMLString(out, "database.host"); got != "db.example.com" {
		t.Errorf("after Set, Get = %q, want %q", got, "db.example.com")
	}
}
