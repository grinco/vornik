package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalValidProjectYAML = `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`

func TestValidateProjectFile_ValidPasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-project.yaml")
	if err := os.WriteFile(path, []byte(minimalValidProjectYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProjectFile(path); err != nil {
		t.Errorf("ValidateProjectFile() = %v, want nil for a valid project file", err)
	}
}

func TestValidateProjectFile_MissingRequiredFieldFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-project.yaml")
	// swarmId omitted — Project.Validate requires it.
	if err := os.WriteFile(path, []byte("projectId: \"test-project\"\ndefaultWorkflowId: \"wf\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ValidateProjectFile(path)
	if err == nil {
		t.Fatal("ValidateProjectFile() = nil, want an error for a missing swarmId")
	}
	if !strings.Contains(err.Error(), "swarmId") {
		t.Errorf("error = %v, want it to name swarmId", err)
	}
}

func TestValidateProjectFile_MalformedYAMLFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-project.yaml")
	if err := os.WriteFile(path, []byte("projectId: [this is not\n  valid yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProjectFile(path); err == nil {
		t.Fatal("ValidateProjectFile() = nil, want an error for malformed YAML")
	}
}

func TestValidateProjectFile_MissingFileFails(t *testing.T) {
	if err := ValidateProjectFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("ValidateProjectFile() = nil, want an error for a missing file")
	}
}

// TestValidateProjectFile_RejectsCrossFieldViolation exercises one of
// Project.Validate's cross-field rules (github_app all-or-nothing) to prove
// ValidateProjectFile isn't just checking parse-ability — it runs the full
// project schema validator, catching the exact class of error the daemon
// schema (config.ValidateFile) would never know about.
func TestValidateProjectFile_RejectsCrossFieldViolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-project.yaml")
	content := minimalValidProjectYAML + `github_app:
  app_id: 123
  installation_id: 456
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ValidateProjectFile(path)
	if err == nil {
		t.Fatal("ValidateProjectFile() = nil, want an error (github_app set without webhook_secret_env/repo_allowlist)")
	}
	if !strings.Contains(err.Error(), "github_app") {
		t.Errorf("error = %v, want it to name github_app", err)
	}
}
