// Tests for the 2026.7.0 F14 multi-path workflow discovery.
// Anchors:
//   - Empty paths slice → no-op, registry stays at zero state.
//   - Single path → identical behaviour to legacy Load.
//   - Multi-path: later wins on per-ID overlap.
//   - Distinct IDs from earlier layers stay visible.
//   - Missing layer directories are tolerated, not fatal.
//   - configDir reflects the last (most-specific) path so
//     Reload re-reads the user-editable layer.

package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// layerHelper writes a minimal project + swarm + workflow
// triple into a per-test directory. Keeps the test bodies
// short — every multi-path test needs at least two of these.
func layerHelper(t *testing.T, root, projectID, swarmID, workflowID string) {
	t.Helper()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", projectID+".yaml"), []byte(
		"projectId: "+projectID+"\ndisplayName: "+projectID+"\nswarmId: "+swarmID+"\ndefaultWorkflowId: "+workflowID+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", swarmID+".md"), []byte(
		"---\nswarmId: "+swarmID+"\nroles:\n  - name: worker\n    runtime:\n      image: fake-agent\n---\n",
	), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", workflowID+".md"), []byte(
		"---\nworkflowId: "+workflowID+"\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n",
	), 0o644))
}

// TestLoadFromPaths_EmptyIsNoop — defensive: zero paths
// doesn't trigger errors and doesn't overwrite state.
func TestLoadFromPaths_EmptyIsNoop(t *testing.T) {
	r := New()
	if err := r.LoadFromPaths(); err != nil {
		t.Fatalf("empty paths must not error: %v", err)
	}
	assert.Empty(t, r.ListProjects())
}

// TestLoadFromPaths_SinglePathMirrorsLegacyLoad — a single
// path call must behave identically to Load(path) so the
// daemon's init code can call LoadFromPaths uniformly.
func TestLoadFromPaths_SinglePathMirrorsLegacyLoad(t *testing.T) {
	root := t.TempDir()
	layerHelper(t, root, "p1", "s1", "w1")

	r := New()
	require.NoError(t, r.LoadFromPaths(root))
	assert.NotNil(t, r.GetProject("p1"))
	assert.NotNil(t, r.GetSwarm("s1"))
	assert.NotNil(t, r.GetWorkflow("w1"))
}

// TestLoadFromPaths_LaterPathOverridesSameID — same-ID
// entries from a later path replace the earlier ones. The
// org → project → user inheritance pattern relies on this.
func TestLoadFromPaths_LaterPathOverridesSameID(t *testing.T) {
	orgShared := t.TempDir()
	userPersonal := t.TempDir()

	// Both layers declare project "shared"; user layer wins.
	layerHelper(t, orgShared, "shared", "s-org", "w-org")
	layerHelper(t, userPersonal, "shared", "s-user", "w-user")
	// Org layer also has unique "extras" project to prove
	// non-overlapping entries from the earlier layer survive.
	layerHelper(t, orgShared, "org-only", "s-org-extra", "w-org-extra")

	r := New()
	require.NoError(t, r.LoadFromPaths(orgShared, userPersonal))

	shared := r.GetProject("shared")
	require.NotNil(t, shared)
	assert.Equal(t, "s-user", shared.SwarmID, "user-personal layer's swarm reference must win")
	assert.Equal(t, "w-user", shared.DefaultWorkflowID, "user-personal layer's workflow reference must win")

	// Distinct ID from the org layer stays visible.
	orgOnly := r.GetProject("org-only")
	assert.NotNil(t, orgOnly, "non-overlapping projects from the earlier layer must survive the merge")
}

// TestLoadFromPaths_MissingDirToleratedAsOptionalLayer — a
// deployment can ship "user-personal" as an optional override
// directory that may or may not exist on each host.
func TestLoadFromPaths_MissingDirToleratedAsOptionalLayer(t *testing.T) {
	orgShared := t.TempDir()
	layerHelper(t, orgShared, "p1", "s1", "w1")

	r := New()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	require.NoError(t, r.LoadFromPaths(orgShared, missing),
		"missing optional layer must not abort the load")
	assert.NotNil(t, r.GetProject("p1"))
}

// TestLoadFromPaths_ConfigDirReflectsLastPath — Reload
// re-reads the most-specific layer. Pins the contract so a
// future refactor doesn't move configDir back to the first
// path (which would make user-personal edits invisible).
func TestLoadFromPaths_ConfigDirReflectsLastPath(t *testing.T) {
	orgShared := t.TempDir()
	userPersonal := t.TempDir()
	layerHelper(t, orgShared, "p1", "s1", "w1")
	layerHelper(t, userPersonal, "p2", "s2", "w2")

	r := New()
	require.NoError(t, r.LoadFromPaths(orgShared, userPersonal))
	assert.Equal(t, userPersonal, r.ConfigDir())
}

// TestLoadFromPaths_MalformedFileIsSkipped — matches the
// existing single-path Load behaviour: a malformed YAML file
// inside a layer is logged + skipped rather than aborting the
// whole load. The good files in the same layer + the other
// layers still surface.
func TestLoadFromPaths_MalformedFileIsSkipped(t *testing.T) {
	good := t.TempDir()
	mixed := t.TempDir()
	layerHelper(t, good, "p1", "s1", "w1")
	// Mixed layer has one good triple + one bad file.
	layerHelper(t, mixed, "p2", "s2", "w2")
	require.NoError(t, os.WriteFile(filepath.Join(mixed, "projects", "broken.yaml"),
		[]byte("not: valid:: yaml::\n  - one\n - two\n"), 0o644))

	r := New()
	require.NoError(t, r.LoadFromPaths(good, mixed),
		"malformed file in a layer must be skipped, not abort the load")
	assert.NotNil(t, r.GetProject("p1"), "good projects from the first layer must survive")
	assert.NotNil(t, r.GetProject("p2"), "good projects in the mixed layer must survive alongside the skipped file")
}
