package ui

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGateFixture adds workflow "gwg" with a gate-type step "g" (gates live
// on gate steps; an agent step may not set both on_success and gates). g
// starts with one gate so add/delete can be exercised without emptying it.
func writeGateFixture(t *testing.T) string {
	t.Helper()
	root := writeWorkflowFixture(t)
	gwg := `---
workflowId: gwg
displayName: "Gate WF"
version: "1.0.0"
entrypoint: a
maxStepVisits: 3
maxIterations: 20
steps:
  a:
    type: agent
    role: lead
    on_success: g
    on_fail: done
  g:
    type: gate
    gates:
      - condition: "ok == true"
        target: done
terminals:
  done:
    status: COMPLETED
---

# Gate WF

## Prompts

### a

Step a.
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "gwg.md"), []byte(gwg), 0o600))
	return root
}

func TestWorkflowGraphGate_Create(t *testing.T) {
	root := writeGateFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gwg/graph/edge", url.Values{
		"src": {"g"}, "kind": {"gate"}, "condition": {"review.approved == true"}, "dst": {"done"},
	})
	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())

	gates := reloader.reg.GetWorkflow("gwg").Steps["g"].Gates
	require.Len(t, gates, 2, "seeded gate + new gate")
	assert.Equal(t, "review.approved == true", gates[1].Condition)
	assert.Equal(t, "done", gates[1].Target)
}

func TestWorkflowGraphGate_Delete(t *testing.T) {
	root := writeGateFixture(t)
	server, reloader := workflowEditServer(t, root)

	// Add a second gate, then delete the first (index 0); one remains.
	require.Equal(t, http.StatusSeeOther, postGraph(server, "/workflows/gwg/graph/edge", url.Values{
		"src": {"g"}, "kind": {"gate"}, "condition": {"x == 1"}, "dst": {"done"},
	}).Code)

	rec := postGraph(server, "/workflows/gwg/graph/edge/delete", url.Values{
		"src": {"g"}, "kind": {"gate"}, "gateIndex": {"0"},
	})
	require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())

	gates := reloader.reg.GetWorkflow("gwg").Steps["g"].Gates
	require.Len(t, gates, 1, "one gate remains")
	assert.Equal(t, "x == 1", gates[0].Condition)
}

func TestWorkflowGraphGate_RejectsEmptyCondition(t *testing.T) {
	root := writeGateFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gwg/graph/edge", url.Values{
		"src": {"g"}, "kind": {"gate"}, "condition": {""}, "dst": {"done"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Len(t, reloader.reg.GetWorkflow("gwg").Steps["g"].Gates, 1, "unchanged on reject")
}

func TestWorkflowGraphGate_RejectsDanglingTarget(t *testing.T) {
	root := writeGateFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := postGraph(server, "/workflows/gwg/graph/edge", url.Values{
		"src": {"g"}, "kind": {"gate"}, "condition": {"x == 1"}, "dst": {"ghost"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Len(t, reloader.reg.GetWorkflow("gwg").Steps["g"].Gates, 1, "unchanged on reject")
}
