package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func findSource(ix *TreeIndex, kind, id string) []SourceRecord {
	var out []SourceRecord
	for _, s := range ix.Sources {
		if s.Kind == kind && s.ID == id {
			out = append(out, s)
		}
	}
	return out
}

// The index is built by the load itself (resolved-config provenance design
// §4.2): each accepted object with the file that supplied it, each file the
// loader refused with the decoder's error, and the role-library files.
func TestTreeIndex_RecordsSourcesRejectedAndRoleLibrary(t *testing.T) {
	root := t.TempDir()
	layerHelper(t, root, "p1", "s1", "w1")
	// A project file the strict decoder refuses: an unknown key. Today this
	// is a line on stderr and a project that silently is not there.
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "broken.yaml"),
		[]byte("projectId: broken\ndisplayName: b\nswarmId: s1\ndefaultWorkflowId: w1\nforge:\n  mention_handel: x\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "role-library"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "role-library", "reviewer.md"), []byte("# reviewer\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "role-library", "README.md"), []byte("# not a role\n"), 0o644))

	r := New()
	require.NoError(t, r.Load(root))
	ix := r.TreeIndex()
	require.NotNil(t, ix)
	assert.Equal(t, []string{root}, ix.Layers)

	p := findSource(ix, "project", "p1")
	require.Len(t, p, 1)
	assert.Equal(t, filepath.Join("projects", "p1.yaml"), p[0].Path)
	assert.Equal(t, root, p[0].Layer)
	assert.Empty(t, p[0].ShadowedBy)
	assert.Len(t, findSource(ix, "swarm", "s1"), 1)
	assert.Len(t, findSource(ix, "workflow", "w1"), 1)
	assert.Len(t, findSource(ix, "role-library", "reviewer"), 1)
	assert.Empty(t, findSource(ix, "role-library", "README"))

	require.Len(t, ix.Rejected, 1)
	assert.Equal(t, filepath.Join("projects", "broken.yaml"), ix.Rejected[0].Path)
	assert.Contains(t, ix.Rejected[0].Error, "mention_handel", "the decoder's error names the key")
	assert.Nil(t, r.GetProject("broken"))
}

// Layering: later wins per id, and the earlier object is listed as shadowed
// rather than dropped — an operator sees both files and which one is in effect.
func TestTreeIndex_LayeredShadowing(t *testing.T) {
	org, user := t.TempDir(), t.TempDir()
	layerHelper(t, org, "shared", "s-org", "w-org")
	layerHelper(t, user, "shared", "s-user", "w-user")
	layerHelper(t, org, "org-only", "s-x", "w-x")

	r := New()
	require.NoError(t, r.LoadFromPaths(org, user))
	ix := r.TreeIndex()
	require.NotNil(t, ix)
	assert.Equal(t, []string{org, user}, ix.Layers)

	shared := findSource(ix, "project", "shared")
	require.Len(t, shared, 2, "both layers' files are listed")
	byLayer := map[string]SourceRecord{}
	for _, s := range shared {
		byLayer[s.Layer] = s
	}
	assert.Equal(t, user, byLayer[org].ShadowedBy, "the org copy is shadowed by the user layer")
	assert.Empty(t, byLayer[user].ShadowedBy, "the user copy is in effect")
	only := findSource(ix, "project", "org-only")
	require.Len(t, only, 1)
	assert.Empty(t, only[0].ShadowedBy)
}

func TestTreeIndex_NilBeforeLoad(t *testing.T) {
	assert.Nil(t, New().TreeIndex())
}
