package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestProjectBacklogDepositsYAMLRoundTrip pins the config surface
// added for the autonomous-development-loop design: Task 5's
// backlog-deposit endpoint reads backlogDeposits.maxPerTask and
// Task 6's autonomy backlog tick reads autonomy.workflow_id. Both
// must parse from project YAML using the exact keys the design
// doc and lld-lint-allowlist.txt reference.
func TestProjectBacklogDepositsYAMLRoundTrip(t *testing.T) {
	raw := `
projectId: "p1"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: backlog
  workflow_id: "backlog-groom"
backlogDeposits:
  maxPerTask: 25
`
	var p Project
	if err := yaml.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Autonomy.WorkflowID != "backlog-groom" {
		t.Errorf("Autonomy.WorkflowID = %q, want %q", p.Autonomy.WorkflowID, "backlog-groom")
	}
	if p.BacklogDeposits.MaxPerTask != 25 {
		t.Errorf("BacklogDeposits.MaxPerTask = %d, want 25", p.BacklogDeposits.MaxPerTask)
	}
}

// TestProjectBacklogDepositsResolveMaxPerTask pins the defaulting
// contract the deposit endpoint relies on: unset/non-positive
// values fall back to 10 so a project that never configured the
// block still gets a sane cap instead of zero (which would refuse
// every deposit).
func TestProjectBacklogDepositsResolveMaxPerTask(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset_defaults_to_10", 0, 10},
		{"negative_defaults_to_10", -5, 10},
		{"explicit_value_kept", 25, 25},
		{"explicit_one_kept", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &ProjectBacklogDeposits{MaxPerTask: c.in}
			if got := b.ResolveMaxPerTask(); got != c.want {
				t.Errorf("ResolveMaxPerTask(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestProjectBacklogDepositsResolveMaxPerTask_NilReceiver(t *testing.T) {
	var b *ProjectBacklogDeposits
	if got := b.ResolveMaxPerTask(); got != 10 {
		t.Errorf("nil receiver: ResolveMaxPerTask() = %d, want 10", got)
	}
}

// TestAutonomyWorkflowIDValidationWarning pins the load-time
// behaviour for autonomy.workflow_id (autonomous-development-loop
// design, Task 4): an unresolvable id must NOT strip the project
// (the backlog tick falls back to defaultWorkflowId at runtime),
// but Load must still return a non-fatal *ValidationError so the
// operator sees the typo loudly instead of it silently rotting.
// A resolvable id, and an empty/unset id, must load clean.
func TestAutonomyWorkflowIDValidationWarning(t *testing.T) {
	tmpDir := t.TempDir()
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.Mkdir(filepath.Join(tmpDir, subdir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
	}
	swarmMD := `---
swarmId: "s1"
roles:
  - name: "coder"
    runtime:
      image: "x:latest"
---
`
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "s.md"), []byte(swarmMD), 0o644); err != nil {
		t.Fatal(err)
	}
	wfMD := `---
workflowId: "w1"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "do work"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "w.md"), []byte(wfMD), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		projectID   string
		yaml        string
		wantWarning bool
	}{
		{
			name:      "unknown_workflow_id_warns_but_keeps_project",
			projectID: "p1",
			yaml: `projectId: "p1"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: backlog
  workflow_id: "does-not-exist"
`,
			wantWarning: true,
		},
		{
			name:      "known_workflow_id_loads_clean",
			projectID: "p2",
			yaml: `projectId: "p2"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: backlog
  workflow_id: "w1"
`,
			wantWarning: false,
		},
		{
			name:      "empty_workflow_id_loads_clean",
			projectID: "p3",
			yaml: `projectId: "p3"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: backlog
`,
			wantWarning: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fname := strings.ToLower(c.name) + ".yaml"
			path := filepath.Join(tmpDir, "projects", fname)
			if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(path) }()

			reg := New()
			err := reg.Load(tmpDir)

			if got := reg.GetProject(c.projectID); got == nil {
				t.Fatalf("project %s must stay loaded regardless of the workflow_id warning", c.projectID)
			}

			var valErr *ValidationError
			isValidationWarning := errors.As(err, &valErr)
			if c.wantWarning {
				if err == nil || !isValidationWarning {
					t.Fatalf("expected a non-fatal *ValidationError for %s, got %v", c.name, err)
				}
				found := false
				for _, e := range valErr.Errors {
					if strings.Contains(e.Error(), "autonomy.workflow_id") {
						found = true
					}
				}
				if !found {
					t.Errorf("ValidationError does not mention autonomy.workflow_id: %v", valErr.Errors)
				}
			} else if err != nil {
				t.Errorf("unexpected load error for %s: %v", c.name, err)
			}
		})
	}
}
