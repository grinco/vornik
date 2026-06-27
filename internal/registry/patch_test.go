package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchYAMLAutonomyEnabled_ToggleExisting(t *testing.T) {
	data := []byte(`projectId: "test"
swarmId: "dev-swarm"
defaultWorkflowId: "dev-pipeline"
autonomy:
  enabled: false  # toggled by /autopilot
  goal: "Build things"
  maxTasksPerHour: 6
`)
	out, err := patchYAMLAutonomyEnabled(data, true)
	require.NoError(t, err)
	assert.Contains(t, string(out), "enabled: true")
	// Other fields preserved
	assert.Contains(t, string(out), `goal: "Build things"`)
	assert.Contains(t, string(out), "maxTasksPerHour: 6")

	// Round-trip back to false
	out2, err := patchYAMLAutonomyEnabled(out, false)
	require.NoError(t, err)
	assert.Contains(t, string(out2), "enabled: false")
}

func TestPatchYAMLAutonomyEnabled_MissingEnabledKey(t *testing.T) {
	data := []byte(`projectId: "test"
swarmId: "dev-swarm"
defaultWorkflowId: "dev-pipeline"
autonomy:
  goal: "Build things"
`)
	out, err := patchYAMLAutonomyEnabled(data, true)
	require.NoError(t, err)
	assert.Contains(t, string(out), "enabled: true")
	assert.Contains(t, string(out), "goal:")
}

func TestPatchYAMLAutonomyEnabled_MissingAutonomySection(t *testing.T) {
	data := []byte(`projectId: "test"
swarmId: "dev-swarm"
defaultWorkflowId: "dev-pipeline"
`)
	out, err := patchYAMLAutonomyEnabled(data, true)
	require.NoError(t, err)
	assert.Contains(t, string(out), "autonomy:")
	assert.Contains(t, string(out), "enabled: true")
}

func TestPatchYAMLAutonomyEnabled_PreservesIndent(t *testing.T) {
	// 2-space indented file (typical project config)
	data := []byte("projectId: \"test\"\nswarmId: \"x\"\ndefaultWorkflowId: \"y\"\nautonomy:\n  enabled: false\n  goal: \"Build\"\n")
	out, err := patchYAMLAutonomyEnabled(data, true)
	require.NoError(t, err)
	// Indent should be 2 spaces (detected from input)
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "enabled:") {
			assert.True(t, strings.HasPrefix(line, "  "), "expected 2-space indent, got: %q", line)
			break
		}
	}
}

func TestSetProjectAutonomyEnabled_UpdatesFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workflows"), 0o755))

	// Minimal swarm stub so registry cross-reference validation passes.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "test-swarm.md"), []byte(`---
swarmId: "test-swarm"
roles:
  - name: "worker"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "test-wf.md"), []byte(`---
workflowId: "test-wf"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "worker"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))

	yaml := `projectId: "my-proj"
swarmId: "test-swarm"
defaultWorkflowId: "test-wf"
autonomy:
  enabled: false
  goal: "Build things"
`
	path := filepath.Join(dir, "projects", "my-proj.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	reg := New()
	require.NoError(t, reg.Load(dir))

	require.NoError(t, reg.SetProjectAutonomyEnabled("my-proj", true))

	// In-memory update
	p := reg.GetProject("my-proj")
	require.NotNil(t, p)
	assert.True(t, p.Autonomy.Enabled)

	// Disk update
	updated, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(updated), "enabled: true")
}

func TestSetProjectAutonomyEnabled_ProjectNotFound(t *testing.T) {
	reg := New()
	err := reg.SetProjectAutonomyEnabled("nonexistent", true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
