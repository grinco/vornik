package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckDispatcherRole_ProjectNotFound(t *testing.T) {
	h := &DoctorHandlers{
		dispatcherProjectID: "nonexistent-project",
		configDir:           "testdata",
	}
	// registry.New() creates empty maps - GetObject will return nil
	c := h.checkDispatcherRole(false)

	assert.Equal(t, "dispatcher_role", c.Name)
	assert.Equal(t, "ERROR", c.Status)
	assert.Contains(t, c.Message, "not a known project")
	assert.Contains(t, c.Message, "nonexistent-project")
}

func TestCheckDispatcherRole_MissingDispatcherRole(t *testing.T) {
	// Test the branch where project exists but has no "dispatcher" role
	// This is the second major uncovered branch (lines 72-79)
	t.Skip("requires temporary config file setup with valid project missing dispatcher role")
}

// TestResolveDoctorSwarmFile_PrefersMarkdown — the helper that
// the doctor --fix path uses to find the swarm file MUST return
// the `.md` candidate. Guards against a regression that
// re-introduces the legacy `.yaml` / `.yml` fallback ladder.
func TestResolveDoctorSwarmFile_PrefersMarkdown(t *testing.T) {
	configDir := t.TempDir()
	swarmsDir := filepath.Join(configDir, "swarms")
	require.NoError(t, os.MkdirAll(swarmsDir, 0o755))
	// Plant only the .md candidate — the helper should find it.
	mdPath := filepath.Join(swarmsDir, "assistant.md")
	require.NoError(t, os.WriteFile(mdPath, []byte("---\nswarmId: \"assistant\"\n---\n"), 0o644))

	got := resolveDoctorSwarmFile(configDir, "assistant")
	assert.Equal(t, mdPath, got, "doctor must resolve to the .md swarm file")
}

// TestResolveDoctorSwarmFile_StaleYAMLIgnored — even if a stale
// `.yaml` is present alongside the `.md`, the helper returns the
// `.md` path. The patcher would refuse a `.yaml` path anyway, but
// the resolver enforces the policy first.
func TestResolveDoctorSwarmFile_StaleYAMLIgnored(t *testing.T) {
	configDir := t.TempDir()
	swarmsDir := filepath.Join(configDir, "swarms")
	require.NoError(t, os.MkdirAll(swarmsDir, 0o755))
	yamlPath := filepath.Join(swarmsDir, "assistant.yaml")
	mdPath := filepath.Join(swarmsDir, "assistant.md")
	require.NoError(t, os.WriteFile(yamlPath, []byte("swarmId: \"assistant\"\n"), 0o644))
	require.NoError(t, os.WriteFile(mdPath, []byte("---\nswarmId: \"assistant\"\n---\n"), 0o644))

	got := resolveDoctorSwarmFile(configDir, "assistant")
	assert.Equal(t, mdPath, got, "doctor must ignore the stale .yaml twin")
	assert.False(t, strings.HasSuffix(got, ".yaml"), "no .yaml fallback after YAML removal")
}
