package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression (incident 2026-06-17): the swarm/workflow AI-assist failed
// with "project not found" when the editor was opened directly via the
// /ui/swarms or /ui/workflows list rather than from a project, because
// data-assist-project was sourced only from ?projectId= and was empty.
// The editors now resolve the owning project (one that references the
// asset) so the assist grounds correctly. writeSwarmFixture wires
// project p1 → swarm edit-swarm + workflow w1.

func TestSwarmEdit_ResolvesAssistProjectWhenNoQuery(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	server.SwarmEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-assist-project="p1"`,
		"editor opened without ?projectId= must resolve the owning project")
}

func TestSwarmEdit_ExplicitProjectQueryWins(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit?projectId=p1", nil)
	server.SwarmEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-assist-project="p1"`)
}

func TestSwarmSchemaConfigEdit_ResolvesAssistProjectWhenNoQuery(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/schema", nil)
	server.SwarmSchemaConfigEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-assist-project="p1"`)
}

func TestWorkflowEdit_ResolvesAssistProjectWhenNoQuery(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workflows/w1/edit", nil)
	server.WorkflowEdit(rec, req, "w1")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `data-assist-project="p1"`)
}

// Resolver unit coverage: unknown asset → "" (assist stays disabled, no
// false grounding).
func TestDefaultAssistProject_UnknownAsset(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	assert.Equal(t, "", server.defaultAssistProjectForSwarm("no-such-swarm"))
	assert.Equal(t, "", server.defaultAssistProjectForWorkflow("no-such-workflow"))
	assert.Equal(t, "p1", server.defaultAssistProjectForSwarm("edit-swarm"))
	assert.Equal(t, "p1", server.defaultAssistProjectForWorkflow("w1"))
}
