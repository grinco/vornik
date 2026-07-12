package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNormalizedToolCredentials(t *testing.T) {
	tests := []struct {
		name string
		in   []ToolCredentialConfig
		want []ToolCredentialConfig
	}{
		{
			name: "empty is nil",
			in:   nil,
			want: nil,
		},
		{
			name: "valid entry keeps fields, trims, defaults label",
			in: []ToolCredentialConfig{
				{Tool: "  mcp__pagedrop__pagedrop_publish ", CredentialField: " password ", ArtifactField: " url ", Label: ""},
			},
			want: []ToolCredentialConfig{
				{Tool: "mcp__pagedrop__pagedrop_publish", CredentialField: "password", ArtifactField: "url", Label: "credential"},
			},
		},
		{
			name: "custom label preserved",
			in: []ToolCredentialConfig{
				{Tool: "t", CredentialField: "f", Label: "viewing password"},
			},
			want: []ToolCredentialConfig{
				{Tool: "t", CredentialField: "f", Label: "viewing password"},
			},
		},
		{
			name: "entry missing tool or field is dropped",
			in: []ToolCredentialConfig{
				{Tool: "", CredentialField: "password"},
				{Tool: "t", CredentialField: ""},
				{Tool: "keep", CredentialField: "pw", Label: "x"},
			},
			want: []ToolCredentialConfig{
				{Tool: "keep", CredentialField: "pw", Label: "x"},
			},
		},
		{
			name: "all-invalid collapses to nil",
			in: []ToolCredentialConfig{
				{Tool: "", CredentialField: ""},
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SecretsConfig{ToolCredentials: tc.in}.NormalizedToolCredentials()
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Backward-compat: a secrets block with the legacy string-list
// trusted_output_tools and no tool_credentials key must load cleanly, with
// ToolCredentials nil.
func TestToolCredentials_BackwardCompatAbsent(t *testing.T) {
	const y = `
allowlist: []
trusted_output_tools:
  - "mcp__pagedrop__pagedrop_publish"
`
	var sc SecretsConfig
	if err := yaml.Unmarshal([]byte(y), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sc.TrustedOutputTools) != 1 {
		t.Errorf("trusted_output_tools = %v, want 1 entry", sc.TrustedOutputTools)
	}
	if sc.ToolCredentials != nil {
		t.Errorf("ToolCredentials = %v, want nil when key absent", sc.ToolCredentials)
	}
	if sc.NormalizedToolCredentials() != nil {
		t.Errorf("NormalizedToolCredentials() = non-nil, want nil")
	}
}

// A pattern-only entry (text-output tool like PageDrop) is valid without a
// credential_field.
func TestToolCredentials_PatternOnlyIsValid(t *testing.T) {
	sc := SecretsConfig{ToolCredentials: []ToolCredentialConfig{
		{Tool: "mcp__pagedrop__pagedrop_publish", CredentialPattern: `Password:\s*(\S+)`, ArtifactPattern: `View:\s*(\S+)`, Label: "viewing password"},
		// Neither field nor pattern → dropped.
		{Tool: "bad", Label: "x"},
	}}
	got := sc.NormalizedToolCredentials()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (pattern-only kept, field+pattern-less dropped)", len(got))
	}
	if got[0].CredentialPattern != `Password:\s*(\S+)` {
		t.Errorf("credential_pattern = %q", got[0].CredentialPattern)
	}
}

// The object form parses into the mapping.
func TestToolCredentials_ObjectFormParses(t *testing.T) {
	const y = `
tool_credentials:
  - tool: "mcp__pagedrop__pagedrop_publish"
    credential_field: "password"
    artifact_field: "url"
    label: "viewing password"
`
	var sc SecretsConfig
	if err := yaml.Unmarshal([]byte(y), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := sc.NormalizedToolCredentials()
	want := ToolCredentialConfig{
		Tool:            "mcp__pagedrop__pagedrop_publish",
		CredentialField: "password",
		ArtifactField:   "url",
		Label:           "viewing password",
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want [%+v]", got, want)
	}
}
