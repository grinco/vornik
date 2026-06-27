package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

func linearWorkflow() *registry.Workflow {
	return &registry.Workflow{
		ID:         "wf",
		Entrypoint: "scout",
		Steps: map[string]registry.WorkflowStep{
			"scout": {Type: "agent", OnSuccess: "build"},
			"build": {Type: "agent", OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
}

func nodeByID(gv GraphView, id string) (GraphNode, bool) {
	for _, n := range gv.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return GraphNode{}, false
}

func TestLayoutWorkflow_LinearRanks(t *testing.T) {
	gv := layoutWorkflow(linearWorkflow())

	require.Len(t, gv.Nodes, 3, "scout, build, done")

	scout, ok := nodeByID(gv, "scout")
	require.True(t, ok)
	build, ok := nodeByID(gv, "build")
	require.True(t, ok)
	done, ok := nodeByID(gv, "done")
	require.True(t, ok)

	assert.True(t, scout.IsEntry, "entrypoint flagged")
	assert.Equal(t, graphKindStep, scout.Kind)
	assert.Equal(t, graphKindTerminal, done.Kind)

	// Strictly increasing X by rank.
	assert.Less(t, scout.X, build.X)
	assert.Less(t, build.X, done.X)
}

func TestLayoutWorkflow_SuccessAndFailEdges(t *testing.T) {
	wf := &registry.Workflow{
		Entrypoint: "scout",
		Steps: map[string]registry.WorkflowStep{
			"scout":   {Type: "agent", OnSuccess: "build", OnFail: "recover"},
			"build":   {Type: "agent", OnSuccess: "done"},
			"recover": {Type: "agent", OnSuccess: "checkpoint"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":       {Status: "COMPLETED"},
			"checkpoint": {Status: "COMPLETED", Recovery: true},
		},
	}
	gv := layoutWorkflow(wf)

	var success, fail int
	for _, e := range gv.Edges {
		switch e.Kind {
		case graphEdgeSuccess:
			success++
		case graphEdgeFail:
			fail++
		}
	}
	assert.Equal(t, 3, success, "scout->build, build->done, recover->checkpoint")
	assert.Equal(t, 1, fail, "scout->recover")

	cp, ok := nodeByID(gv, "checkpoint")
	require.True(t, ok)
	assert.True(t, cp.Recovery, "recovery terminal flagged")
}

func TestLayoutWorkflow_GateEdgesCarryConditionLabel(t *testing.T) {
	wf := &registry.Workflow{
		Entrypoint: "review",
		Steps: map[string]registry.WorkflowStep{
			"review": {Type: "gate", Gates: []registry.WorkflowGate{
				{Condition: "review.approved == true", Target: "done"},
			}},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	gv := layoutWorkflow(wf)

	var found bool
	for _, e := range gv.Edges {
		if e.Kind == graphEdgeGate && e.From == "review" && e.To == "done" {
			found = true
			assert.Equal(t, "review.approved == true", e.Label)
		}
	}
	assert.True(t, found, "gate edge with condition label present")
}

func TestLayoutWorkflow_CycleIsBackEdgeAndDoesNotHang(t *testing.T) {
	wf := &registry.Workflow{
		Entrypoint: "scout",
		Steps: map[string]registry.WorkflowStep{
			"scout": {Type: "agent", OnSuccess: "build"},
			"build": {Type: "agent", OnSuccess: "done", OnFail: "scout"}, // back-edge
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	gv := layoutWorkflow(wf) // must return, not hang

	var back bool
	for _, e := range gv.Edges {
		if e.From == "build" && e.To == "scout" {
			back = e.IsBack
		}
	}
	assert.True(t, back, "build->scout classified as back-edge")
}

func TestLayoutWorkflow_UnreachableNodeFlagged(t *testing.T) {
	wf := &registry.Workflow{
		Entrypoint: "scout",
		Steps: map[string]registry.WorkflowStep{
			"scout":  {Type: "agent", OnSuccess: "done"},
			"orphan": {Type: "agent"}, // not wired in
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	gv := layoutWorkflow(wf)

	orphan, ok := nodeByID(gv, "orphan")
	require.True(t, ok)
	assert.True(t, orphan.Unreachable)

	scout, _ := nodeByID(gv, "scout")
	assert.False(t, scout.Unreachable)
}

func TestLayoutWorkflow_NoOutgoingFlagged(t *testing.T) {
	wf := &registry.Workflow{
		Entrypoint: "a",
		Steps: map[string]registry.WorkflowStep{
			"a": {Type: "agent", OnSuccess: "b"},
			"b": {Type: "agent"}, // reachable, but a dead-end (no outgoing edge)
		},
	}
	gv := layoutWorkflow(wf)

	b, ok := nodeByID(gv, "b")
	require.True(t, ok)
	assert.True(t, b.NoOutgoing, "dead-end step flagged")

	a, _ := nodeByID(gv, "a")
	assert.False(t, a.NoOutgoing)
}
