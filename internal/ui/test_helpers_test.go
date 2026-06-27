// Package ui: shared test helpers. Lives in *_test.go so it doesn't
// ship in the production binary.
package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// buildPopulatedUIRegistry stamps the minimum fixture needed for the
// list/detail render tests: one swarm, one workflow, one project.
// Returned registry is already Load()-ed and ready for
// WithProjectRegistry.
func buildPopulatedUIRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project-1.yaml"), []byte(`
projectId: project-1
displayName: First Project
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 42
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}
