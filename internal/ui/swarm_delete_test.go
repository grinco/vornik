package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// writeOrphanSwarmFixture seeds a registry root with two swarms,
// one referenced by a project (the editable-swarm fixture) and one
// orphan swarm with no project references. The orphan is the
// happy-path delete target; the referenced one exercises the
// referrer guard.
func writeOrphanSwarmFixture(t *testing.T) string {
	t.Helper()
	root := writeSwarmFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "orphan-swarm.md"), []byte(`---
swarmId: orphan-swarm
displayName: "Orphan Swarm"
leadRole: lead
roles:
  - name: "lead"
    description: "Plans"
    model: "test-lead-model"
    runtime:
      image: "vornik-agent:latest"
---
`), 0o600))
	return root
}

// TestSwarmDelete_OrphanSucceeds — deleting a swarm no project
// references removes the file and redirects to the swarm list
// page with the success query parameter.
func TestSwarmDelete_OrphanSucceeds(t *testing.T) {
	root := writeOrphanSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)
	swarmPath := filepath.Join(root, "swarms", "orphan-swarm.md")
	require.FileExists(t, swarmPath)

	req := httptest.NewRequest(http.MethodPost, "/swarms/orphan-swarm/delete", nil)
	rec := httptest.NewRecorder()
	server.SwarmDelete(rec, req, "orphan-swarm")

	require.Equal(t, http.StatusSeeOther, rec.Code, "expected redirect on success")
	assert.Contains(t, rec.Header().Get("Location"), "/ui/swarms?deleted=orphan-swarm")
	assert.NoFileExists(t, swarmPath, "swarm file must be removed from disk")
	// The registry reload counter advances exactly once.
	assert.Equal(t, 1, reloader.calls)
	// Registry no longer carries the deleted swarm.
	assert.Nil(t, server.projectReg.GetSwarm("orphan-swarm"))
}

// TestSwarmDelete_ReferencedRefuses — deleting a swarm that a
// project still references returns a conflict-status error page
// listing every referring project, and the file stays on disk.
func TestSwarmDelete_ReferencedRefuses(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)
	swarmPath := filepath.Join(root, "swarms", "edit-swarm.md")
	require.FileExists(t, swarmPath)

	req := httptest.NewRequest(http.MethodPost, "/swarms/edit-swarm/delete", nil)
	rec := httptest.NewRecorder()
	server.SwarmDelete(rec, req, "edit-swarm")

	require.Equal(t, http.StatusConflict, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "p1", "error must name the referring project")
	assert.Contains(t, body, "still referenced", "error must explain why")
	assert.FileExists(t, swarmPath, "swarm file must NOT be removed when referenced")
}

// TestSwarmDelete_UnknownSwarm — the editor data lookup 404s
// when the swarm doesn't exist; the same lookup gates the
// delete. No file is touched.
func TestSwarmDelete_UnknownSwarm(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodPost, "/swarms/does-not-exist/delete", nil)
	rec := httptest.NewRecorder()
	server.SwarmDelete(rec, req, "does-not-exist")

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSwarmEditor_RendersDeleteButton — the GET render of the
// editor surfaces the danger-zone delete button so operators
// don't have to know the POST path. data-testid attribute keeps
// the assertion robust to copy edits.
func TestSwarmEditor_RendersDeleteButton(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "edit-swarm")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-testid="swarm-delete-button"`)
	assert.Contains(t, body, `action="/ui/swarms/edit-swarm/delete"`)
	// Client-side confirmation must be present.
	assert.Contains(t, body, "confirm(", "Delete button must trigger a JS confirm() prompt")
}

// TestSwarmRouter_DispatchesDelete — POST /swarms/{id}/delete
// reaches SwarmDelete (not SwarmSave). Verified indirectly by
// asserting on the SeeOther redirect that only the delete branch
// emits.
func TestSwarmRouter_DispatchesDelete(t *testing.T) {
	root := writeOrphanSwarmFixture(t)
	server, _ := swarmEditServer(t, root)
	swarmPath := filepath.Join(root, "swarms", "orphan-swarm.md")
	require.FileExists(t, swarmPath)

	req := httptest.NewRequest(http.MethodPost, "/swarms/orphan-swarm/delete", strings.NewReader(""))
	rec := httptest.NewRecorder()
	server.swarmRouter(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code, "swarmRouter must dispatch DELETE to SwarmDelete (got body: %s)", rec.Body.String())
	assert.NoFileExists(t, swarmPath)
}

// TestProjectsReferencingSwarm_Empty — no registry → nil
// referrers (defensive: the editor handlers already guard nil
// projectReg, but the helper should never panic).
func TestProjectsReferencingSwarm_Empty(t *testing.T) {
	s := &Server{}
	assert.Empty(t, s.projectsReferencingSwarm("anything"))
}

// TestProjectsReferencingSwarm_Sorted — multiple referrers come
// out alphabetically so the error message is stable.
func TestProjectsReferencingSwarm_Sorted(t *testing.T) {
	root := writeSwarmFixture(t)
	// Add two more projects pointing at the same swarm.
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "z-proj.yaml"), []byte(`projectId: z-proj
displayName: Z
swarmId: edit-swarm
defaultWorkflowId: w1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "a-proj.yaml"), []byte(`projectId: a-proj
displayName: A
swarmId: edit-swarm
defaultWorkflowId: w1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := &Server{projectReg: reg}

	got := server.projectsReferencingSwarm("edit-swarm")
	assert.Equal(t, []string{"a-proj", "p1", "z-proj"}, got)
}
