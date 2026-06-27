package ui

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowClone_CreatesCopy(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/edit-wf/clone", url.Values{
		"newId": {"edit-wf-copy"}, "displayName": {"Edit WF Copy"},
	})

	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "/ui/workflows/edit-wf-copy/graph", rec.Header().Get("Location"))

	clone := reloader.reg.GetWorkflow("edit-wf-copy")
	require.NotNil(t, clone, "clone registered after reload")
	assert.Equal(t, "edit-wf-copy", clone.ID)
	// Steps carry over unchanged.
	assert.Contains(t, clone.Steps, "plan")
	assert.Contains(t, clone.Steps, "implement")
	// Source is untouched.
	assert.NotNil(t, reloader.reg.GetWorkflow("edit-wf"))
}

func TestWorkflowClone_RejectsDuplicateID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/edit-wf/clone", url.Values{"newId": {"edit-wf"}})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestWorkflowClone_RejectsBadID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/edit-wf/clone", url.Values{"newId": {"Bad/Id"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWorkflowClone_RejectsUnknownSource(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/no-such/clone", url.Values{"newId": {"some-copy"}})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
