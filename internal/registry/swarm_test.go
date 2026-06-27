package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadSwarmValidation tests swarm validation rules.
func TestLoadSwarmValidation(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantError bool
	}{
		{
			name: "valid minimal swarm",
			yaml: `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`,
			wantError: false,
		},
		{
			name: "missing swarmId",
			yaml: `roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`,
			wantError: true,
		},
		{
			name: "empty roles",
			yaml: `swarmId: "test-swarm"
roles: []
`,
			wantError: true,
		},
		{
			name: "role missing name",
			yaml: `swarmId: "test-swarm"
roles:
  - runtime:
      image: "test:latest"
`,
			wantError: true,
		},
		{
			name: "role missing runtime",
			yaml: `swarmId: "test-swarm"
roles:
  - name: "coder"
`,
			wantError: true,
		},
		{
			name: "role missing runtime image",
			yaml: `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime: {}
`,
			wantError: true,
		},
		{
			name: "invalid runtime policy",
			yaml: `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtimePolicy: "invalid"
    runtime:
      image: "test:latest"
`,
			wantError: true,
		},
		{
			name: "multiple roles",
			yaml: `swarmId: "test-swarm"
roles:
  - name: "lead"
    count: 1
    runtimePolicy: "warm"
    runtime:
      image: "lead:latest"
  - name: "coder"
    count: 3
    runtimePolicy: "ephemeral"
    runtime:
      image: "coder:latest"
`,
			wantError: false,
		},
		{
			name: "duplicate role names",
			yaml: `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
  - name: "coder"
    runtime:
      image: "test2:latest"
`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "swarm-test")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()

			swarmsDir := filepath.Join(tmpDir, "swarms")
			if err := os.Mkdir(swarmsDir, 0755); err != nil {
				t.Fatalf("failed to create swarms dir: %v", err)
			}

			if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte("---\n"+tt.yaml+"---\n"), 0644); err != nil {
				t.Fatalf("failed to write swarm: %v", err)
			}

			_, err = LoadSwarms(tmpDir)
			if tt.wantError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestSwarmRoleDefaults tests default values for swarm roles.
func TestSwarmRoleDefaults(t *testing.T) {
	yaml := `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	swarm := swarms["test-swarm"]
	if swarm == nil {
		t.Fatal("expected test-swarm")
	}

	if len(swarm.Roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(swarm.Roles))
	}

	role := swarm.Roles[0]

	// Check defaults
	if role.Count != 1 {
		t.Errorf("expected default Count 1, got %d", role.Count)
	}
	if role.RuntimePolicy != "ephemeral" {
		t.Errorf("expected default RuntimePolicy 'ephemeral', got '%s'", role.RuntimePolicy)
	}
}

// TestSwarmWithPermissions tests swarm roles with permissions.
func TestSwarmWithPermissions(t *testing.T) {
	yaml := `swarmId: "test-swarm"
roles:
  - name: "lead"
    runtime:
      image: "lead:latest"
    permissions:
      delegationAllowed: true
      allowedTools:
        - "file_read"
        - "file_write"
        - "exec"
  - name: "coder"
    runtime:
      image: "coder:latest"
    permissions:
      delegationAllowed: false
`
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	swarm := swarms["test-swarm"]
	if swarm == nil {
		t.Fatal("expected test-swarm")
	}

	// Verify lead role permissions
	leadRole := swarm.Roles[0]
	if !leadRole.Permissions.DelegationAllowed {
		t.Error("expected lead DelegationAllowed to be true")
	}
	if len(leadRole.Permissions.AllowedTools) != 3 {
		t.Errorf("expected 3 allowed tools, got %d", len(leadRole.Permissions.AllowedTools))
	}

	// Verify coder role permissions
	coderRole := swarm.Roles[1]
	if coderRole.Permissions.DelegationAllowed {
		t.Error("expected coder DelegationAllowed to be false")
	}
}

// TestSwarmResourceLimits tests swarm role resource configuration.
func TestSwarmResourceLimits(t *testing.T) {
	yaml := `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
      cpu: "2"
      memory: "4Gi"
      envVars:
        LOG_LEVEL: "debug"
`
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	swarm := swarms["test-swarm"]
	if swarm == nil {
		t.Fatal("expected test-swarm")
	}

	role := swarm.Roles[0]

	if role.Runtime.CPU != "2" {
		t.Errorf("expected CPU '2', got '%s'", role.Runtime.CPU)
	}
	if role.Runtime.Memory != "4Gi" {
		t.Errorf("expected Memory '4Gi', got '%s'", role.Runtime.Memory)
	}
	if role.Runtime.EnvVars["LOG_LEVEL"] != "debug" {
		t.Errorf("expected LOG_LEVEL 'debug', got '%s'", role.Runtime.EnvVars["LOG_LEVEL"])
	}
}

// TestSwarmLeadRole tests lead role validation.
func TestSwarmLeadRole(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantError bool
	}{
		{
			name: "valid lead role",
			yaml: `swarmId: "test-swarm"
leadRole: "lead"
roles:
  - name: "lead"
    runtime:
      image: "lead:latest"
  - name: "coder"
    runtime:
      image: "coder:latest"
`,
			wantError: false,
		},
		{
			name: "invalid lead role reference",
			yaml: `swarmId: "test-swarm"
leadRole: "nonexistent"
roles:
  - name: "coder"
    runtime:
      image: "coder:latest"
`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "swarm-test")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()

			swarmsDir := filepath.Join(tmpDir, "swarms")
			if err := os.Mkdir(swarmsDir, 0755); err != nil {
				t.Fatalf("failed to create swarms dir: %v", err)
			}

			if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte("---\n"+tt.yaml+"---\n"), 0644); err != nil {
				t.Fatalf("failed to write swarm: %v", err)
			}

			_, err = LoadSwarms(tmpDir)
			if tt.wantError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestLoadSwarmsEmptyDir tests loading from empty directory.
func TestLoadSwarmsEmptyDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	if len(swarms) != 0 {
		t.Errorf("expected 0 swarms, got %d", len(swarms))
	}
}

// TestLoadSwarmsNoDir tests loading when swarms directory doesn't exist.
func TestLoadSwarmsNoDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Don't create swarms directory
	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	if len(swarms) != 0 {
		t.Errorf("expected 0 swarms, got %d", len(swarms))
	}
}

// TestSwarmRoleModelOverride tests per-role model override.
func TestSwarmRoleModelOverride(t *testing.T) {
	yaml := `swarmId: "test-swarm"
roles:
  - name: "coder"
    model: "claude-sonnet-4-20250514"
    runtime:
      image: "test:latest"
  - name: "reviewer"
    runtime:
      image: "test:latest"
`
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	swarm := swarms["test-swarm"]
	if swarm == nil {
		t.Fatal("expected test-swarm")
	}

	coder := swarm.Roles[0]
	if coder.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected coder model 'claude-sonnet-4-20250514', got '%s'", coder.Model)
	}

	reviewer := swarm.Roles[1]
	if reviewer.Model != "" {
		t.Errorf("expected reviewer model to be empty (uses daemon default), got '%s'", reviewer.Model)
	}
}

// TestSwarmDuplicates tests duplicate swarm ID detection.
func TestSwarmDuplicates(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "swarm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	// Create two swarms with same ID
	swarm := `swarmId: "duplicate"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`
	if err := os.WriteFile(filepath.Join(swarmsDir, "swarm1.md"), []byte("---\n"+swarm+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swarmsDir, "swarm2.md"), []byte("---\n"+swarm+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm2: %v", err)
	}

	_, err = LoadSwarms(tmpDir)
	if err == nil {
		t.Error("expected error for duplicate swarm IDs")
	}
}
