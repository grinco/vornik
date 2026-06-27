package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutonomyModeValidationOnLoad pins the registry-load
// validation behaviour for the new ProjectAutonomy.Mode field.
// An unknown mode must drop the project from the loaded set
// rather than silently falling back, because the alternative is
// the autonomy loop dispatching on the wrong engine — costing
// LLM calls (llm fallback over cron) or skipping work (cron
// dispatching with no Goal). Enforcement is gated to projects
// with Autonomy.Enabled so a stale value in a disabled project
// doesn't block the rest of the registry from loading.
func TestAutonomyModeValidationOnLoad(t *testing.T) {
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
		wantLoadErr bool
		wantPresent bool
	}{
		{
			name:      "empty_mode_disabled_autonomy_loads",
			projectID: "p1",
			yaml: `projectId: "p1"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: false
`,
			wantLoadErr: false,
			wantPresent: true,
		},
		{
			name:      "cron_mode_loads",
			projectID: "p2",
			yaml: `projectId: "p2"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: cron
  goal: "every tick: run market check"
`,
			wantLoadErr: false,
			wantPresent: true,
		},
		{
			name:      "backlog_mode_no_goal_loads",
			projectID: "p3",
			yaml: `projectId: "p3"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: backlog
`,
			wantLoadErr: false,
			wantPresent: true,
		},
		{
			name:      "cron_mode_without_goal_rejected",
			projectID: "p4",
			yaml: `projectId: "p4"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: cron
`,
			wantLoadErr: true,
			wantPresent: false,
		},
		{
			name:      "unknown_mode_rejected",
			projectID: "p5",
			yaml: `projectId: "p5"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: manual
  goal: "x"
`,
			wantLoadErr: true,
			wantPresent: false,
		},
		{
			name:      "absolute_backlog_path_rejected",
			projectID: "p6",
			yaml: `projectId: "p6"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: true
  mode: backlog
  backlogFilePath: "/tmp/backlog.md"
`,
			wantLoadErr: true,
			wantPresent: false,
		},
		{
			name:      "unknown_mode_disabled_autonomy_loads",
			projectID: "p7",
			yaml: `projectId: "p7"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  enabled: false
  mode: manual
`,
			wantLoadErr: false,
			wantPresent: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Each subtest gets a unique project file so loads
			// don't bleed across cases.
			fname := strings.ToLower(c.name) + ".yaml"
			path := filepath.Join(tmpDir, "projects", fname)
			if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(path) }()

			reg := New()
			err := reg.Load(tmpDir)
			if c.wantLoadErr && err == nil {
				t.Errorf("expected load error for %s, got nil", c.name)
			}
			if !c.wantLoadErr && err != nil {
				t.Errorf("unexpected load error: %v", err)
			}
			// Inspect whether the case's project landed.
			if got := reg.GetProject(c.projectID); (got != nil) != c.wantPresent {
				t.Errorf("project %s present=%v, want %v", c.projectID, got != nil, c.wantPresent)
			}
		})
	}
}
