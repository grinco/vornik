package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postGraph(server *Server, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.workflowRouter(rec, withAdminUI(req))
	return rec
}

// writeGraphEditFixture adds a richly-connected workflow "gw" alongside the
// edit-wf fixture. Its a<->b cycle plus c->done keeps every node reachable
// from either a or b, so entrypoint re-pointing and single-edge rewires stay
// valid (the validator rejects any workflow with an unreachable step).
func writeGraphEditFixture(t *testing.T) string {
	t.Helper()
	root := writeWorkflowFixture(t)
	gw := `---
workflowId: gw
displayName: "Graph WF"
version: "1.0.0"
entrypoint: a
maxStepVisits: 3
maxIterations: 20
steps:
  a:
    type: agent
    role: lead
    on_success: b
    on_fail: c
  b:
    type: agent
    role: lead
    on_success: a
    on_fail: c
  c:
    type: agent
    role: lead
    on_success: done
    on_fail: done
terminals:
  done:
    status: COMPLETED
---

# Graph WF

## Prompts

### a

Step a.

### b

Step b.

### c

Step c.
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "gw.md"), []byte(gw), 0o600))
	return root
}

func TestWorkflowGraphEntrypoint_Updates(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gw/graph/entrypoint", url.Values{"id": {"b"}})

	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "/ui/workflows/gw/graph", rec.Header().Get("Location"))
	assert.Equal(t, "b", reloader.reg.GetWorkflow("gw").Entrypoint)
}

func TestWorkflowGraphEntrypoint_RejectsUnknownStep(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gw/graph/entrypoint", url.Values{"id": {"nope"}})

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "a", reloader.reg.GetWorkflow("gw").Entrypoint, "unchanged on reject")
}

func TestWorkflowGraphEdge_CreateSuccess(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	// Rewire b.on_success a -> c (a stays reachable as entrypoint).
	rec := postGraph(server, "/workflows/gw/graph/edge", url.Values{
		"src": {"b"}, "kind": {"success"}, "dst": {"c"},
	})

	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "c", reloader.reg.GetWorkflow("gw").Steps["b"].OnSuccess)
}

func TestWorkflowGraphEdge_DeleteFail(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	// Delete a.on_fail (c stays reachable via b.on_fail).
	rec := postGraph(server, "/workflows/gw/graph/edge/delete", url.Values{
		"src": {"a"}, "kind": {"fail"},
	})

	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())
	assert.Empty(t, reloader.reg.GetWorkflow("gw").Steps["a"].OnFail)
}

func TestWorkflowGraphEdge_RejectsDanglingTarget(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gw/graph/edge", url.Values{
		"src": {"a"}, "kind": {"success"}, "dst": {"ghost"},
	})

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "b", reloader.reg.GetWorkflow("gw").Steps["a"].OnSuccess, "unchanged on reject")
}
