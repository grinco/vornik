package registry

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLoadSwarms_OutputSchemaDerivesLegacyFields exercises the
// end-to-end flow: a swarm YAML declaring `outputSchema:` survives
// LoadSwarms with the legacy RequiredOutputKeys + PlausibilityRules
// fields populated by the derivation walker. Pins the contract
// downstream consumers (executor's validateRequiredOutputKeys,
// EvaluatePlausibility) rely on — they read those legacy fields
// directly today; phase 2 of the deterministic-output-schema delivery
// plan moves them to read OutputSchema directly.
func TestLoadSwarms_OutputSchemaDerivesLegacyFields(t *testing.T) {
	tmpDir := t.TempDir()
	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir swarms: %v", err)
	}

	// Schema mirrors the writer-role shape from the design doc:
	// nested required path (writing.written), MinLength:1 on
	// `message`, an explicit conditional plausibility rule. Every
	// derivation behaviour in one fixture.
	swarmMD := `---
swarmId: "schema-test-swarm"
displayName: "Schema test"
roles:
  - name: "writer"
    runtime:
      image: "vornik-agent:latest"
    outputSchema:
      type: object
      required: [writing, message]
      properties:
        writing:
          type: object
          required: [written]
          properties:
            written: { type: bool }
            path: { type: string }
            reason: { type: string }
        message:
          type: string
          minLength: 1
      plausibility:
        - name: written_implies_path
          when: { "writing.written": true }
          require: ["writing.path"]
---
`
	if err := os.WriteFile(filepath.Join(swarmsDir, "schema-test.md"), []byte(swarmMD), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms: %v", err)
	}
	swarm := swarms["schema-test-swarm"]
	if swarm == nil {
		t.Fatalf("schema-test-swarm not loaded")
	}
	if len(swarm.Roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(swarm.Roles))
	}
	role := swarm.Roles[0]

	// Derived RequiredOutputKeys: top-level required + nested
	// required + types from properties. Sorted output for
	// determinism.
	wantKeys := []string{"message:string", "writing.written:bool", "writing:object"}
	if !reflect.DeepEqual(role.RequiredOutputKeys, wantKeys) {
		t.Errorf("RequiredOutputKeys = %v, want %v",
			role.RequiredOutputKeys, wantKeys)
	}

	// Derived PlausibilityRules: explicit rules first, then
	// implicit minLength rules. The implicit rule for `message`
	// rejects "" (which the type check would otherwise accept).
	wantRules := []PlausibilityRule{
		{
			Name:    "written_implies_path",
			When:    map[string]any{"writing.written": true},
			Require: []string{"writing.path"},
		},
		{
			Name:    "min_length_message",
			Require: []string{"message"},
		},
	}
	if !reflect.DeepEqual(role.PlausibilityRules, wantRules) {
		t.Errorf("PlausibilityRules = %#v, want %#v",
			role.PlausibilityRules, wantRules)
	}
}

// TestLoadSwarms_OutputSchemaRefusesLegacyFieldsAlongside ensures the
// strict single-source-of-truth policy at config load: a role that sets
// outputSchema AND requiredOutputKeys (or plausibilityRules) is the
// exact regression class — two prose copies of the same fact — that
// the schema field exists to prevent. Validate must refuse rather than
// pick a winner silently.
func TestLoadSwarms_OutputSchemaRefusesLegacyFieldsAlongside(t *testing.T) {
	tmpDir := t.TempDir()
	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir swarms: %v", err)
	}

	cases := []struct {
		name string
		yaml string
		want string // substring expected in the error
	}{
		{
			name: "outputSchema + requiredOutputKeys both set",
			yaml: `swarmId: "bad-swarm"
roles:
  - name: "role"
    runtime: { image: "x" }
    requiredOutputKeys: ["foo"]
    outputSchema:
      type: object
      required: [foo]
`,
			want: "outputSchema is set",
		},
		{
			name: "outputSchema + plausibilityRules both set",
			yaml: `swarmId: "bad-swarm"
roles:
  - name: "role"
    runtime: { image: "x" }
    plausibilityRules:
      - name: r
        require: [foo]
    outputSchema:
      type: object
`,
			want: "outputSchema is set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(swarmsDir, "bad.md")
			if err := os.WriteFile(path, []byte("---\n"+tc.yaml+"---\n"), 0o644); err != nil {
				t.Fatalf("write swarm: %v", err)
			}
			defer func() { _ = os.Remove(path) }()
			_, err := LoadSwarms(tmpDir)
			if err == nil {
				t.Fatalf("expected LoadSwarms to refuse the config; got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
