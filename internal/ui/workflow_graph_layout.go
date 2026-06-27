package ui

import (
	"sort"

	"vornik.io/vornik/internal/registry"
)

// Node kinds.
const (
	graphKindStep     = "step"
	graphKindTerminal = "terminal"
)

// Edge kinds drive colour + style in the template.
const (
	graphEdgeSuccess = "success"
	graphEdgeFail    = "fail"
	graphEdgeGate    = "gate"
)

// Layout geometry (pixels). Integer coords keep the template's int-typed
// `add` helper usable and avoid float formatting in SVG attributes.
const (
	graphColW  = 210 // horizontal gap between ranks
	graphRowH  = 96  // vertical gap between nodes in a rank
	graphNodeW = 150
	graphNodeH = 54
	graphPadX  = 40
	graphPadY  = 30
)

// GraphNode is a positioned workflow node (a step or a terminal).
type GraphNode struct {
	ID          string
	Type        string // step Type ("agent", "gate", ...) or terminal Status
	Kind        string // graphKindStep | graphKindTerminal
	IsEntry     bool
	Recovery    bool // recovery terminal (WorkflowTerminal.Recovery)
	Unreachable bool // not reachable from the entrypoint
	NoOutgoing  bool // step with no outgoing edge (dead-end)
	X, Y        int
	W, H        int
}

// GraphEdge is a routed directed edge between two nodes.
type GraphEdge struct {
	From       string
	To         string
	Kind       string // graphEdgeSuccess | graphEdgeFail | graphEdgeGate
	Label      string // gate condition; else ""
	IsBack     bool   // target ranks at/behind source — drawn dashed
	X1, Y1     int
	X2, Y2     int
	MidX, MidY int
}

// GraphView is the fully positioned graph the template plots.
type GraphView struct {
	Nodes  []GraphNode
	Edges  []GraphEdge
	Width  int
	Height int
}

// layoutWorkflow computes a deterministic layered left-to-right layout
// of a workflow's control-flow graph. Pure: no I/O, stable across runs.
func layoutWorkflow(wf *registry.Workflow) GraphView {
	if wf == nil {
		return GraphView{Width: graphPadX * 2, Height: graphPadY * 2}
	}

	rank := rankNodes(wf)

	// Group node ids by rank, ordered stably within a rank.
	maxRank := 0
	byRank := map[int][]string{}
	for id, rk := range rank {
		byRank[rk] = append(byRank[rk], id)
		if rk > maxRank {
			maxRank = rk
		}
	}
	for rk := range byRank {
		sort.Strings(byRank[rk])
	}

	pos := map[string][2]int{}
	maxRows := 0
	for rk := 0; rk <= maxRank; rk++ {
		ids := byRank[rk]
		if len(ids) > maxRows {
			maxRows = len(ids)
		}
		for row, id := range ids {
			x := graphPadX + rk*graphColW
			y := graphPadY + row*graphRowH
			pos[id] = [2]int{x, y}
		}
	}

	gv := GraphView{
		Width:  graphPadX*2 + maxRank*graphColW + graphNodeW,
		Height: graphPadY*2 + maxRows*graphRowH,
	}

	reach := reachable(wf)

	// Nodes (steps then terminals), emitted in stable id order.
	stepIDs := sortedKeys(wf.Steps)
	for _, id := range stepIDs {
		p := pos[id]
		gv.Nodes = append(gv.Nodes, GraphNode{
			ID: id, Type: wf.Steps[id].Type, Kind: graphKindStep,
			IsEntry: id == wf.Entrypoint, Unreachable: !reach[id],
			NoOutgoing: len(outEdges(id, wf.Steps[id])) == 0,
			X:          p[0], Y: p[1], W: graphNodeW, H: graphNodeH,
		})
	}
	termIDs := sortedTerminalKeys(wf.Terminals)
	for _, id := range termIDs {
		p := pos[id]
		gv.Nodes = append(gv.Nodes, GraphNode{
			ID: id, Type: wf.Terminals[id].Status, Kind: graphKindTerminal,
			Recovery: wf.Terminals[id].Recovery, Unreachable: !reach[id],
			X: p[0], Y: p[1], W: graphNodeW, H: graphNodeH,
		})
	}

	// Edges: only steps have outgoing edges.
	for _, sid := range stepIDs {
		from := pos[sid]
		for _, e := range outEdges(sid, wf.Steps[sid]) {
			to, ok := pos[e.to]
			if !ok {
				continue // dangling target (not in node set) — skip
			}
			gv.Edges = append(gv.Edges, GraphEdge{
				From: e.from, To: e.to, Kind: e.kind, Label: e.label,
				IsBack: rank[e.to] <= rank[sid],
				X1:     from[0] + graphNodeW, Y1: from[1] + graphNodeH/2,
				X2: to[0], Y2: to[1] + graphNodeH/2,
				MidX: (from[0] + graphNodeW + to[0]) / 2,
				MidY: (from[1] + to[1] + graphNodeH) / 2,
			})
		}
	}
	return gv
}

func sortedKeys(m map[string]registry.WorkflowStep) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedTerminalKeys(m map[string]registry.WorkflowTerminal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stepEdge is a raw outgoing edge from a step, pre-layout.
type stepEdge struct {
	from, to, kind, label string
}

// outEdges returns a step's outgoing edges in deterministic order
// (success, fail, then gates in declared order).
func outEdges(id string, st registry.WorkflowStep) []stepEdge {
	var es []stepEdge
	if st.OnSuccess != "" {
		es = append(es, stepEdge{id, st.OnSuccess, graphEdgeSuccess, ""})
	}
	if st.OnFail != "" {
		es = append(es, stepEdge{id, st.OnFail, graphEdgeFail, ""})
	}
	for _, g := range st.Gates {
		if g.Target != "" {
			es = append(es, stepEdge{id, g.Target, graphEdgeGate, g.Condition})
		}
	}
	return es
}

func nodeExists(wf *registry.Workflow, id string) bool {
	if _, ok := wf.Steps[id]; ok {
		return true
	}
	_, ok := wf.Terminals[id]
	return ok
}

// rankNodes assigns a left-to-right rank via BFS from the entrypoint.
// The visited set makes cycles terminate. Nodes not reachable from the
// entrypoint are ranked one column past the deepest reachable node.
func rankNodes(wf *registry.Workflow) map[string]int {
	rank := map[string]int{}
	if wf.Entrypoint != "" {
		if _, ok := wf.Steps[wf.Entrypoint]; ok {
			rank[wf.Entrypoint] = 0
			queue := []string{wf.Entrypoint}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				st, ok := wf.Steps[cur]
				if !ok {
					continue // terminals have no outgoing edges
				}
				for _, e := range outEdges(cur, st) {
					if !nodeExists(wf, e.to) {
						continue
					}
					if _, seen := rank[e.to]; !seen {
						rank[e.to] = rank[cur] + 1
						queue = append(queue, e.to)
					}
				}
			}
		}
	}

	maxReachable := 0
	for _, rk := range rank {
		if rk > maxReachable {
			maxReachable = rk
		}
	}
	// Place unreachable nodes after the reachable frontier, stable order.
	for _, id := range sortedKeys(wf.Steps) {
		if _, ok := rank[id]; !ok {
			rank[id] = maxReachable + 1
		}
	}
	for _, id := range sortedTerminalKeys(wf.Terminals) {
		if _, ok := rank[id]; !ok {
			rank[id] = maxReachable + 1
		}
	}
	return rank
}

// reachable reports which nodes the entrypoint can reach.
func reachable(wf *registry.Workflow) map[string]bool {
	r := map[string]bool{}
	if wf.Entrypoint == "" {
		return r
	}
	if _, ok := wf.Steps[wf.Entrypoint]; !ok {
		return r
	}
	queue := []string{wf.Entrypoint}
	r[wf.Entrypoint] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		st, ok := wf.Steps[cur]
		if !ok {
			continue
		}
		for _, e := range outEdges(cur, st) {
			if !nodeExists(wf, e.to) || r[e.to] {
				continue
			}
			r[e.to] = true
			queue = append(queue, e.to)
		}
	}
	return r
}
