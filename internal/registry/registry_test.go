package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRegistryFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(root, subdir), 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", subdir, err)
		}
	}

	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}
}

func TestLoadProjects(t *testing.T) {
	// Create a temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create test project directory
	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	// Test valid project
	validProject := `projectId: "test-project"
displayName: "Test Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
defaultPriority: 50
maxConcurrentTasks: 2
`
	if err := os.WriteFile(filepath.Join(projectsDir, "test.yaml"), []byte(validProject), 0644); err != nil {
		t.Fatalf("failed to write test project: %v", err)
	}

	projects, err := LoadProjects(tmpDir)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}

	if len(projects) != 1 {
		t.Errorf("expected 1 project, got %d", len(projects))
	}

	if projects["test-project"] == nil {
		t.Error("expected test-project to exist")
	} else {
		if projects["test-project"].DisplayName != "Test Project" {
			t.Errorf("expected DisplayName 'Test Project', got '%s'", projects["test-project"].DisplayName)
		}
	}
}

func TestLoadProjectsValidation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	// Test missing projectId
	invalidProject := `displayName: "Invalid Project"`
	if err := os.WriteFile(filepath.Join(projectsDir, "invalid.yaml"), []byte(invalidProject), 0644); err != nil {
		t.Fatalf("failed to write test project: %v", err)
	}

	_, err = LoadProjects(tmpDir)
	if err == nil {
		t.Error("expected validation error for missing projectId")
	}
}

func TestLoadSwarms(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	// Test valid swarm
	validSwarm := `---
swarmId: "test-swarm"
displayName: "Test Swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`
	if err := os.WriteFile(filepath.Join(swarmsDir, "test.md"), []byte(validSwarm), 0644); err != nil {
		t.Fatalf("failed to write test swarm: %v", err)
	}

	swarms, err := LoadSwarms(tmpDir)
	if err != nil {
		t.Fatalf("LoadSwarms failed: %v", err)
	}

	if len(swarms) != 1 {
		t.Errorf("expected 1 swarm, got %d", len(swarms))
	}

	if swarms["test-swarm"] == nil {
		t.Error("expected test-swarm to exist")
	}

	// Check default values
	if swarms["test-swarm"].Roles[0].Count != 1 {
		t.Errorf("expected default count 1, got %d", swarms["test-swarm"].Roles[0].Count)
	}
	if swarms["test-swarm"].Roles[0].RuntimePolicy != "ephemeral" {
		t.Errorf("expected default runtimePolicy 'ephemeral', got '%s'", swarms["test-swarm"].Roles[0].RuntimePolicy)
	}
}

func TestLoadWorkflows(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	// Test valid workflow
	validWorkflow := `workflowId: "test-workflow"
displayName: "Test Workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "Do something"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
`
	if err := os.WriteFile(filepath.Join(workflowsDir, "test.md"), []byte("---\n"+validWorkflow+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write test workflow: %v", err)
	}

	workflows, err := LoadWorkflows(tmpDir)
	if err != nil {
		t.Fatalf("LoadWorkflows failed: %v", err)
	}

	if len(workflows) != 1 {
		t.Errorf("expected 1 workflow, got %d", len(workflows))
	}

	if workflows["test-workflow"] == nil {
		t.Error("expected test-workflow to exist")
	}
}

func TestRegistryCrossReference(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create all directories
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", subdir, err)
		}
	}

	// Write swarm
	swarmYAML := `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "test.md"), []byte("---\n"+swarmYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	// Write workflow
	workflowYAML := `workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "test.md"), []byte("---\n"+workflowYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	// Write valid project
	validProject := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "valid.yaml"), []byte(validProject), 0644); err != nil {
		t.Fatalf("failed to write valid project: %v", err)
	}

	// Test successful load
	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	project, swarm, workflow, err := reg.ResolveProjectConfig("test-project")
	if err != nil {
		t.Fatalf("ResolveProjectConfig failed: %v", err)
	}

	if project.ID != "test-project" {
		t.Errorf("expected project ID 'test-project', got '%s'", project.ID)
	}
	if swarm.ID != "test-swarm" {
		t.Errorf("expected swarm ID 'test-swarm', got '%s'", swarm.ID)
	}
	if workflow.ID != "test-workflow" {
		t.Errorf("expected workflow ID 'test-workflow', got '%s'", workflow.ID)
	}

	// Test cross-reference validation - project with invalid swarm
	invalidProject := `projectId: "bad-project"
swarmId: "nonexistent-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "invalid.yaml"), []byte(invalidProject), 0644); err != nil {
		t.Fatalf("failed to write invalid project: %v", err)
	}

	reg2 := New()
	err = reg2.Load(tmpDir)
	if err == nil {
		t.Error("expected validation error for invalid swarm reference")
	}
}

func TestRegistryCrossReferenceRejectsWorkflowRoleMissingFromSwarm(t *testing.T) {
	tmpDir := t.TempDir()

	writeRegistryFixture(t, tmpDir, map[string]string{
		"projects/test.yaml": `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "reviewer"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`,
	})

	reg := New()
	err := reg.Load(tmpDir)
	if err == nil {
		t.Fatal("expected cross-reference validation error")
	}
	if !strings.Contains(err.Error(), "references role 'reviewer' not present in swarm 'test-swarm'") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRegistryRejectsUnknownReplyWorkflow confirms a github_app
// reply_workflow_id pointing at a non-existent workflow fails at
// registry load rather than silently routing GitHub tasks to a
// missing workflow at runtime (N7).
func TestRegistryRejectsUnknownReplyWorkflow(t *testing.T) {
	tmpDir := t.TempDir()
	writeRegistryFixture(t, tmpDir, map[string]string{
		"projects/test.yaml": `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
github_app:
  webhook_secret_env: "GH_SECRET"
  repo_allowlist:
    - "acme/api"
  reply_workflow_id: "does-not-exist"
`,
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`,
	})

	reg := New()
	err := reg.Load(tmpDir)
	if err == nil {
		t.Fatal("expected error for unknown reply_workflow_id")
	}
	if !strings.Contains(err.Error(), "reply_workflow_id references non-existent workflow 'does-not-exist'") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRegistryAcceptsValidReplyWorkflow confirms a github_app
// reply_workflow_id that names an existing workflow loads and is
// preserved on the project struct (N7 YAML round-trip).
func TestRegistryAcceptsValidReplyWorkflow(t *testing.T) {
	tmpDir := t.TempDir()
	writeRegistryFixture(t, tmpDir, map[string]string{
		"projects/test.yaml": `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
github_app:
  webhook_secret_env: "GH_SECRET"
  repo_allowlist:
    - "acme/api"
  reply_workflow_id: "reply-workflow"
`,
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"workflows/reply.md": `---
workflowId: "reply-workflow"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "reply"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`,
	})

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	p := reg.GetProject("test-project")
	if p == nil {
		t.Fatal("project not loaded")
	}
	if p.GitHubApp.ReplyWorkflowID != "reply-workflow" {
		t.Errorf("ReplyWorkflowID = %q, want reply-workflow", p.GitHubApp.ReplyWorkflowID)
	}
	if got := p.GitHubApp.EffectiveReplyWorkflowID(p.DefaultWorkflowID); got != "reply-workflow" {
		t.Errorf("EffectiveReplyWorkflowID = %q, want reply-workflow", got)
	}
}

// TestEffectiveReplyWorkflowID_FallsBackToDefault pins the
// fallback-to-default behaviour when reply_workflow_id is unset.
func TestEffectiveReplyWorkflowID_FallsBackToDefault(t *testing.T) {
	var g ProjectGitHubApp
	if got := g.EffectiveReplyWorkflowID("proj-default"); got != "proj-default" {
		t.Errorf("empty ReplyWorkflowID should fall back to default; got %q", got)
	}
	g.ReplyWorkflowID = "   "
	if got := g.EffectiveReplyWorkflowID("proj-default"); got != "proj-default" {
		t.Errorf("whitespace ReplyWorkflowID should fall back to default; got %q", got)
	}
	g.ReplyWorkflowID = "explicit"
	if got := g.EffectiveReplyWorkflowID("proj-default"); got != "explicit" {
		t.Errorf("set ReplyWorkflowID should win; got %q", got)
	}
}

func TestRegistryThreadSafety(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create minimal config
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", subdir, err)
		}
	}

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Concurrent reads
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = reg.GetProject("test")
				_ = reg.GetSwarm("test")
				_ = reg.GetWorkflow("test")
				_ = reg.ListProjects()
				_ = reg.GetStats()
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestRegistryReload tests the Reload method.
func TestRegistryReload(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create all directories
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", subdir, err)
		}
	}

	// Write initial swarm
	swarmYAML := `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "test.md"), []byte("---\n"+swarmYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	// Write workflow
	workflowYAML := `workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "test.md"), []byte("---\n"+workflowYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	// Write initial project
	projectYAML := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(projectYAML), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify initial state
	if reg.GetProject("test-project") == nil {
		t.Error("expected test-project to exist")
	}

	// Update project file
	updatedProject := `projectId: "test-project"
displayName: "Updated Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(updatedProject), 0644); err != nil {
		t.Fatalf("failed to update project: %v", err)
	}

	// Reload
	if err := reg.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	// Verify updated state
	project := reg.GetProject("test-project")
	if project == nil {
		t.Fatal("expected test-project to exist after reload")
	}
	if project.DisplayName != "Updated Project" {
		t.Errorf("expected DisplayName 'Updated Project', got '%s'", project.DisplayName)
	}
}

func TestRegistryStageValidateActivate(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	writeRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Original Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Staged Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`), 0644); err != nil {
		t.Fatalf("failed to update project: %v", err)
	}

	if err := reg.Stage(tmpDir); err != nil {
		t.Fatalf("Stage failed: %v", err)
	}
	if err := reg.ValidateStaged(); err != nil {
		t.Fatalf("ValidateStaged failed: %v", err)
	}

	if got := reg.GetProject("test-project").DisplayName; got != "Original Project" {
		t.Fatalf("active config changed before activation: %s", got)
	}

	if err := reg.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged failed: %v", err)
	}

	if got := reg.GetProject("test-project").DisplayName; got != "Staged Project" {
		t.Fatalf("expected staged config to be active, got %s", got)
	}
}

func TestRegistryValidateStagedPreservesActiveOnFailure(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	writeRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Stable Project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Broken Project"
swarmId: "missing-swarm"
defaultWorkflowId: "test-workflow"
`), 0644); err != nil {
		t.Fatalf("failed to update project: %v", err)
	}

	if err := reg.Stage(tmpDir); err != nil {
		t.Fatalf("Stage failed: %v", err)
	}

	if err := reg.ValidateStaged(); err == nil {
		t.Fatal("expected ValidateStaged to fail")
	}

	if got := reg.GetProject("test-project").DisplayName; got != "Stable Project" {
		t.Fatalf("active config should remain unchanged, got %s", got)
	}
}

func TestRegistryDiffStaged(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	writeRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/test.md": `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
---
`,
		"projects/test.yaml": `projectId: "test-project"
displayName: "Original"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`,
	})

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(`projectId: "test-project"
displayName: "Updated"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`), 0644); err != nil {
		t.Fatalf("failed to update project: %v", err)
	}
	if err := os.Remove(filepath.Join(tmpDir, "workflows", "test.md")); err != nil {
		t.Fatalf("failed to remove workflow: %v", err)
	}

	if err := reg.Stage(tmpDir); err != nil {
		t.Fatalf("Stage failed: %v", err)
	}

	diff, err := reg.DiffStaged()
	if err != nil {
		t.Fatalf("DiffStaged failed: %v", err)
	}

	if len(diff.ChangedProjects) != 1 || diff.ChangedProjects[0] != "test-project" {
		t.Fatalf("unexpected changed projects diff: %#v", diff.ChangedProjects)
	}
	if len(diff.DeletedWorkflows) != 1 || diff.DeletedWorkflows[0] != "test-workflow" {
		t.Fatalf("unexpected deleted workflows diff: %#v", diff.DeletedWorkflows)
	}
}

// TestRegistryGetConfigDir tests GetConfigDir method.
func TestRegistryGetConfigDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	configDir := reg.GetConfigDir()
	if configDir != tmpDir {
		t.Errorf("expected config dir '%s', got '%s'", tmpDir, configDir)
	}
}

// TestRegistryListSwarms tests ListSwarms method.
func TestRegistryListSwarms(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	swarmsDir := filepath.Join(tmpDir, "swarms")
	if err := os.Mkdir(swarmsDir, 0755); err != nil {
		t.Fatalf("failed to create swarms dir: %v", err)
	}

	// Create multiple swarms
	for i := 1; i <= 3; i++ {
		swarmYAML := fmt.Sprintf(`swarmId: "swarm-%d"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`, i)
		filename := fmt.Sprintf("swarm%d.md", i)
		if err := os.WriteFile(filepath.Join(swarmsDir, filename), []byte("---\n"+swarmYAML+"---\n"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", filename, err)
		}
	}

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	swarms := reg.ListSwarms()
	if len(swarms) != 3 {
		t.Errorf("expected 3 swarms, got %d", len(swarms))
	}
}

// TestRegistryListWorkflows tests ListWorkflows method.
func TestRegistryListWorkflows(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	// Create multiple workflows
	for i := 1; i <= 3; i++ {
		workflowYAML := fmt.Sprintf(`workflowId: "workflow-%d"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`, i)
		filename := fmt.Sprintf("workflow%d.md", i)
		if err := os.WriteFile(filepath.Join(workflowsDir, filename), []byte("---\n"+workflowYAML+"---\n"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", filename, err)
		}
	}

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	workflows := reg.ListWorkflows()
	if len(workflows) != 3 {
		t.Errorf("expected 3 workflows, got %d", len(workflows))
	}
}

// TestRegistryGetProjectWithSwarm tests GetProjectWithSwarm method.
func TestRegistryGetProjectWithSwarm(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create all directories
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", subdir, err)
		}
	}

	// Write swarm
	swarmYAML := `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "test.md"), []byte("---\n"+swarmYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	// Write workflow
	workflowYAML := `workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "test.md"), []byte("---\n"+workflowYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	// Write project
	projectYAML := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(projectYAML), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	project, swarm, err := reg.GetProjectWithSwarm("test-project")
	if err != nil {
		t.Fatalf("GetProjectWithSwarm failed: %v", err)
	}
	if project == nil {
		t.Error("expected project to exist")
	}
	if swarm == nil {
		t.Error("expected swarm to exist")
	}
}

// TestRegistryGetProjectWithWorkflow tests GetProjectWithWorkflow method.
func TestRegistryGetProjectWithWorkflow(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Create all directories
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", subdir, err)
		}
	}

	// Write swarm
	swarmYAML := `swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "test.md"), []byte("---\n"+swarmYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write swarm: %v", err)
	}

	// Write workflow
	workflowYAML := `workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "test.md"), []byte("---\n"+workflowYAML+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	// Write project
	projectYAML := `projectId: "test-project"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "test.yaml"), []byte(projectYAML), 0644); err != nil {
		t.Fatalf("failed to write project: %v", err)
	}

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	project, workflow, err := reg.GetProjectWithWorkflow("test-project")
	if err != nil {
		t.Fatalf("GetProjectWithWorkflow failed: %v", err)
	}
	if project == nil {
		t.Error("expected project to exist")
	}
	if workflow == nil {
		t.Error("expected workflow to exist")
	}
}

// TestRegistryErrorMethods tests error type methods.
func TestRegistryErrorMethods(t *testing.T) {
	// Test ProjectValidationError
	pve := ProjectValidationError{File: "test.yaml", Field: "projectId", Message: "required"}
	if pve.Error() == "" {
		t.Error("expected non-empty error string")
	}

	// Test SwarmValidationError
	sve := SwarmValidationError{File: "test.yaml", Field: "swarmId", Message: "required"}
	if sve.Error() == "" {
		t.Error("expected non-empty error string")
	}

	// Test WorkflowValidationError
	wve := WorkflowValidationError{File: "test.yaml", Field: "workflowId", Message: "required"}
	if wve.Error() == "" {
		t.Error("expected non-empty error string")
	}
}
