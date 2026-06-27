package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadWorkflowValidation tests workflow validation rules.
func TestLoadWorkflowValidation(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantError bool
	}{
		{
			name: "valid minimal workflow",
			yaml: `workflowId: "test"
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
`,
			wantError: false,
		},
		{
			name: "missing workflowId",
			yaml: `entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: true,
		},
		{
			name: "missing entrypoint",
			yaml: `workflowId: "test"
steps:
  start:
    type: "agent"
    prompt: "do work"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: true,
		},
		{
			name: "missing step type",
			yaml: `workflowId: "test"
entrypoint: "start"
steps:
  start:
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: true,
		},
		{
			name: "workflow with gates",
			yaml: `workflowId: "test"
entrypoint: "review"
steps:
  review:
    type: "agent"
    prompt: "do work"
    role: "reviewer"
    gates:
      - condition: "approved == true"
        target: "done"
      - condition: "approved == false"
        target: "revise"
  revise:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "review"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: false,
		},
		{
			name: "invalid on_fail target",
			yaml: `workflowId: "test"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_fail: "missing"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: true,
		},
		{
			name: "entrypoint not found",
			yaml: `workflowId: "test"
entrypoint: "nonexistent"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: true,
		},
		{
			name: "no steps",
			yaml: `workflowId: "test"
entrypoint: "start"
terminals:
  done:
    status: "COMPLETED"
`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "workflow-test")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()

			workflowsDir := filepath.Join(tmpDir, "workflows")
			if err := os.Mkdir(workflowsDir, 0755); err != nil {
				t.Fatalf("failed to create workflows dir: %v", err)
			}

			if err := os.WriteFile(filepath.Join(workflowsDir, "test.md"), []byte("---\n"+tt.yaml+"---\n"), 0644); err != nil {
				t.Fatalf("failed to write workflow: %v", err)
			}

			_, err = LoadWorkflows(tmpDir)
			if tt.wantError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestWorkflowStepDefaults tests default value handling for steps.
func TestWorkflowStepDefaults(t *testing.T) {
	yaml := `workflowId: "test"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "Do work"
    on_success: "step2"
  step2:
    type: "agent"
    prompt: "do work"
    role: "tester"
terminals:
  done:
    status: "COMPLETED"
`
	tmpDir, err := os.MkdirTemp("", "workflow-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workflowsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	workflows, err := LoadWorkflows(tmpDir)
	if err != nil {
		t.Fatalf("LoadWorkflows failed: %v", err)
	}

	if len(workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(workflows))
	}

	wf := workflows["test"]
	if wf == nil {
		t.Fatal("expected test workflow")
	}

	// Verify entrypoint
	if wf.Entrypoint != "step1" {
		t.Errorf("expected entrypoint 'step1', got '%s'", wf.Entrypoint)
	}

	// Verify steps exist
	if len(wf.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(wf.Steps))
	}
}

// TestLoadWorkflowsEmptyDir tests loading from empty directory.
func TestLoadWorkflowsEmptyDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "workflow-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	workflows, err := LoadWorkflows(tmpDir)
	if err != nil {
		t.Fatalf("LoadWorkflows failed: %v", err)
	}

	if len(workflows) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(workflows))
	}
}

// TestLoadWorkflowsNoDir tests loading when workflows directory doesn't exist.
func TestLoadWorkflowsNoDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "workflow-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Don't create workflows directory
	workflows, err := LoadWorkflows(tmpDir)
	if err != nil {
		t.Fatalf("LoadWorkflows failed: %v", err)
	}

	if len(workflows) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(workflows))
	}
}

// TestWorkflowDuplicates tests duplicate workflow ID detection.
func TestWorkflowDuplicates(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "workflow-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	// Create two workflows with same ID
	workflow := `workflowId: "duplicate"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
terminals:
  done:
    status: "COMPLETED"
`
	if err := os.WriteFile(filepath.Join(workflowsDir, "wf1.md"), []byte("---\n"+workflow+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write wf1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, "wf2.md"), []byte("---\n"+workflow+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write wf2: %v", err)
	}

	_, err = LoadWorkflows(tmpDir)
	if err == nil {
		t.Error("expected error for duplicate workflow IDs")
	}
}

// TestWorkflowWithRetryPolicy tests workflow steps with retry policy.
func TestWorkflowWithRetryPolicy(t *testing.T) {
	yaml := `workflowId: "test"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
    timeout: "30m"
    retryPolicy:
      maxRetries: 3
      backoff: "exponential"
terminals:
  done:
    status: "COMPLETED"
`
	tmpDir, err := os.MkdirTemp("", "workflow-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workflowsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	workflows, err := LoadWorkflows(tmpDir)
	if err != nil {
		t.Fatalf("LoadWorkflows failed: %v", err)
	}

	wf := workflows["test"]
	if wf == nil {
		t.Fatal("expected test workflow")
	}

	step := wf.Steps["start"]
	if step.Timeout != "30m" {
		t.Errorf("expected timeout '30m', got '%s'", step.Timeout)
	}
	if step.RetryPolicy.MaxRetries != 3 {
		t.Errorf("expected max retries 3, got %d", step.RetryPolicy.MaxRetries)
	}
}

// TestWorkflowTerminalStates tests terminal state configuration.
func TestWorkflowTerminalStates(t *testing.T) {
	yaml := `workflowId: "test"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "success"
    on_failure: "failure"
terminals:
  success:
    status: "COMPLETED"
    message: "Operation succeeded"
  failure:
    status: "FAILED"
    message: "Operation failed"
`
	tmpDir, err := os.MkdirTemp("", "workflow-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	workflowsDir := filepath.Join(tmpDir, "workflows")
	if err := os.Mkdir(workflowsDir, 0755); err != nil {
		t.Fatalf("failed to create workflows dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workflowsDir, "test.md"), []byte("---\n"+yaml+"---\n"), 0644); err != nil {
		t.Fatalf("failed to write workflow: %v", err)
	}

	workflows, err := LoadWorkflows(tmpDir)
	if err != nil {
		t.Fatalf("LoadWorkflows failed: %v", err)
	}

	wf := workflows["test"]
	if wf == nil {
		t.Fatal("expected test workflow")
	}

	if len(wf.Terminals) != 2 {
		t.Errorf("expected 2 terminals, got %d", len(wf.Terminals))
	}

	successTerm := wf.Terminals["success"]
	if successTerm.Status != "COMPLETED" {
		t.Errorf("expected status 'COMPLETED', got '%s'", successTerm.Status)
	}
	if successTerm.Message != "Operation succeeded" {
		t.Errorf("expected message 'Operation succeeded', got '%s'", successTerm.Message)
	}
}
