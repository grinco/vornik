package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeOrphanWorkflowFixture seeds writeWorkflowFixture's tree
// with an extra orphan workflow that no project references. Used
// to exercise the happy-path delete.
func writeOrphanWorkflowFixture(t *testing.T) string {
	t.Helper()
	root := writeWorkflowFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "orphan-wf.md"), []byte(`---
workflowId: orphan-wf
displayName: "Orphan Workflow"
version: "1.0.0"
entrypoint: only
steps:
  only:
    type: agent
    role: lead
    on_success: done
    on_fail: failed
    timeout: 15m
terminals:
  done:
    status: COMPLETED
  failed:
    status: FAILED
---

# Orphan Workflow

Used by tests for the delete path.

## Prompts

### only

Do the work then complete.
`), 0o600))
	return root
}

// TestWorkflowDelete_OrphanSucceeds — happy path: file goes
// away, response is a redirect to /ui/workflows with the
// deleted-id query parameter, registry no longer carries the
// workflow.
func TestWorkflowDelete_OrphanSucceeds(t *testing.T) {
	root := writeOrphanWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)
	wfPath := filepath.Join(root, "workflows", "orphan-wf.md")
	require.FileExists(t, wfPath)

	req := httptest.NewRequest(http.MethodPost, "/workflows/orphan-wf/delete", nil)
	rec := httptest.NewRecorder()
	server.WorkflowDelete(rec, req, "orphan-wf")

	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "/ui/workflows?deleted=orphan-wf")
	assert.NoFileExists(t, wfPath)
	assert.Equal(t, 1, reloader.calls)
	assert.Nil(t, server.projectReg.GetWorkflow("orphan-wf"))
}

// TestWorkflowDelete_ReferencedRefuses — the editable workflow
// is referenced by project p1 (writeWorkflowFixture wires that
// dependency). Delete must refuse and leave the file in place.
func TestWorkflowDelete_ReferencedRefuses(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)
	wfPath := filepath.Join(root, "workflows", "edit-wf.md")
	require.FileExists(t, wfPath)

	req := httptest.NewRequest(http.MethodPost, "/workflows/edit-wf/delete", nil)
	rec := httptest.NewRecorder()
	server.WorkflowDelete(rec, req, "edit-wf")

	require.Equal(t, http.StatusConflict, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "p1", "error must name the referring project")
	assert.Contains(t, body, "still referenced")
	assert.FileExists(t, wfPath, "workflow file must NOT be removed when referenced")
}

// TestWorkflowDelete_UnknownWorkflow — missing workflow id
// returns 404 via the editor data lookup, same as the editor
// page itself.
func TestWorkflowDelete_UnknownWorkflow(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodPost, "/workflows/does-not-exist/delete", nil)
	rec := httptest.NewRecorder()
	server.WorkflowDelete(rec, req, "does-not-exist")

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestWorkflowEditor_RendersDeleteButton — the GET render of
// the editor surfaces the danger-zone delete button.
func TestWorkflowEditor_RendersDeleteButton(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "edit-wf")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-testid="workflow-delete-button"`)
	assert.Contains(t, body, `action="/ui/workflows/edit-wf/delete"`)
	assert.Contains(t, body, "confirm(", "Delete button must trigger a JS confirm() prompt")
}

// TestWorkflowRouter_DispatchesDelete — POST /workflows/{id}/delete
// reaches WorkflowDelete (the router's branch coverage); we
// observe the redirect that only WorkflowDelete emits.
func TestWorkflowRouter_DispatchesDelete(t *testing.T) {
	root := writeOrphanWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)
	wfPath := filepath.Join(root, "workflows", "orphan-wf.md")
	require.FileExists(t, wfPath)

	req := httptest.NewRequest(http.MethodPost, "/workflows/orphan-wf/delete", nil)
	rec := httptest.NewRecorder()
	server.workflowRouter(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code, "router must reach WorkflowDelete (body: %s)", rec.Body.String())
	assert.NoFileExists(t, wfPath)
}

// TestProjectsReferencingWorkflow_NilRegistry — defensive: no
// registry → nil, no panic.
func TestProjectsReferencingWorkflow_NilRegistry(t *testing.T) {
	s := &Server{}
	assert.Empty(t, s.projectsReferencingWorkflow("any"))
}
