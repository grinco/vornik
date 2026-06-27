package ui

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Add node uses the insert-into-edge model: the new node is wired in from an
// existing step so it is reachable (the validator rejects unreachable steps),
// and it forwards to that step's previous target so nothing is orphaned.
func TestWorkflowGraphNode_AddInsertsIntoEdge(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	// Insert agent node "x" on a's success edge (a.on_success was b).
	rec := postGraph(server, "/workflows/gw/graph/node", url.Values{
		"id": {"x"}, "type": {"agent"}, "from": {"a"}, "fromKind": {"success"},
	})
	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())

	wf := reloader.reg.GetWorkflow("gw")
	require.NotNil(t, wf)
	assert.Equal(t, "agent", wf.Steps["x"].Type)
	assert.Equal(t, "x", wf.Steps["a"].OnSuccess, "a now points at x")
	assert.Equal(t, "b", wf.Steps["x"].OnSuccess, "x forwards to a's old target")
}

func TestWorkflowGraphNode_RejectsDuplicateID(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gw/graph/node", url.Values{
		"id": {"a"}, "type": {"agent"}, "from": {"b"}, "fromKind": {"success"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "agent", reloader.reg.GetWorkflow("gw").Steps["a"].Type, "a unchanged")
}

func TestWorkflowGraphNode_RejectsBadID(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gw/graph/node", url.Values{
		"id": {"x/y"}, "type": {"agent"}, "from": {"a"}, "fromKind": {"success"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// Gotcha: a deleted step's "### <id>" body prompt subsection must be pruned,
// else ParseWorkflowMarkdown rejects the orphan subsection.
func TestWorkflowGraphNodeDelete_PrunesBodySubsection(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	// Delete c. Inbound edges a.on_fail=c and b.on_fail=c get cleaned; the
	// "### c" body subsection must be pruned for the reparse to succeed.
	rec := postGraph(server, "/workflows/gw/graph/node/delete", url.Values{"id": {"c"}})
	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())

	wf := reloader.reg.GetWorkflow("gw")
	require.NotNil(t, wf, "reload succeeded (no orphan-subsection parse error)")
	_, exists := wf.Steps["c"]
	assert.False(t, exists, "step c removed")
}

// Gotcha: deleting a node must clear inbound edges that pointed at it, else
// they dangle (validation would reject the transition target).
func TestWorkflowGraphNodeDelete_CleansInboundEdges(t *testing.T) {
	root := writeGraphEditFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gw/graph/node/delete", url.Values{"id": {"c"}})
	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())

	wf := reloader.reg.GetWorkflow("gw")
	assert.Empty(t, wf.Steps["a"].OnFail, "a.on_fail (was c) cleaned")
	assert.Empty(t, wf.Steps["b"].OnFail, "b.on_fail (was c) cleaned")
}
