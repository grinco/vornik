package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowGraph_RendersSVG(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/graph", nil)
	rec := httptest.NewRecorder()
	server.WorkflowGraph(rec, req, "edit-wf")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<svg", "renders an SVG canvas")
	// The fixture's steps + terminals appear as node labels.
	assert.Contains(t, body, "plan")
	assert.Contains(t, body, "implement")
	assert.Contains(t, body, "done")
}

func TestWorkflowGraph_RendersControls(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/graph", nil)
	rec := httptest.NewRecorder()
	server.WorkflowGraph(rec, req, "edit-wf")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Edge-connect form.
	assert.Contains(t, body, `name="src"`)
	assert.Contains(t, body, `name="kind"`)
	assert.Contains(t, body, `name="dst"`)
	// Entrypoint form posts to the entrypoint route.
	assert.Contains(t, body, `action="/ui/workflows/edit-wf/graph/entrypoint"`)
	// Progressive-enhancement click-to-connect hook.
	assert.Contains(t, body, "data-graph-connect")
	// Node add/delete controls (all 8 types offered).
	assert.Contains(t, body, `action="/ui/workflows/edit-wf/graph/node"`)
	assert.Contains(t, body, `action="/ui/workflows/edit-wf/graph/node/delete"`)
	assert.Contains(t, body, `value="call_project"`)
}

func TestWorkflowGraph_InvalidID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/x/graph", nil)
	rec := httptest.NewRecorder()
	server.WorkflowGraph(rec, req, "../etc/passwd")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWorkflowGraph_UnknownID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/no-such/graph", nil)
	rec := httptest.NewRecorder()
	server.WorkflowGraph(rec, req, "no-such")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWorkflowRouter_GraphRoute(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/graph", nil)
	rec := httptest.NewRecorder()
	server.workflowRouter(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<svg")
}
