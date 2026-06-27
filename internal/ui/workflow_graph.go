package ui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// workflowGraphPath resolves a workflow id to its on-disk WORKFLOW.md,
// applying the same id-safety guard as the schema editor. Returns ""
// when the id is unsafe or the registry/config is unavailable.
func (s *Server) workflowGraphPath(workflowID string) string {
	if workflowID == "" || strings.Contains(workflowID, "/") || strings.Contains(workflowID, "..") {
		return ""
	}
	dir := s.configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "workflows", workflowID+".md")
}

// applyWorkflowGraphEdit loads the workflow file, runs transform over its
// split frontmatter+body, validates the WHOLE workflow, writes atomically,
// reloads the registry, and audits. Returns "" on success or an
// operator-facing error message. Never persists an invalid workflow — the
// validator is the single source of truth, identical to the schema editor.
func (s *Server) applyWorkflowGraphEdit(
	r *http.Request, workflowID, action string,
	transform func(fm, body []byte) (newFM, newBody []byte, err error),
) string {
	path := s.workflowGraphPath(workflowID)
	if path == "" {
		return "Invalid workflow id"
	}
	if s.projectReg == nil {
		return "Project registry not configured"
	}
	if s.projectReg.GetWorkflow(workflowID) == nil {
		return "Workflow not found"
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		return "Failed to read workflow: " + err.Error()
	}
	base := filepath.Base(path)
	fm, body, err := registry.SplitWorkflowContent(existing, base)
	if err != nil {
		return "Failed to split workflow file: " + err.Error()
	}

	newFM, newBody, err := transform(fm, body)
	if err != nil {
		return err.Error()
	}

	joined := registry.JoinWorkflowContent(newFM, newBody)
	parsed, err := registry.ParseWorkflowMarkdown(joined, base)
	if err != nil {
		return trimWorkflowParserPrefix(err.Error())
	}
	if err := parsed.Validate(base); err != nil {
		return err.Error()
	}

	backupPath, err := writeProjectConfigAtomic(path, joined)
	if err != nil {
		return "Failed to write workflow: " + err.Error()
	}
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			return reloadErrMsg(err, backupPath)
		}
	} else if err := s.projectReg.Load(s.configDir()); err != nil {
		return reloadErrMsg(err, backupPath)
	}

	s.writeWorkflowGraphAudit(r, workflowID, action)
	return ""
}

// writeWorkflowGraphAudit records a graph-editor mutation, mirroring
// writeWorkflowSaveAudit but with a per-action label.
func (s *Server) writeWorkflowGraphAudit(r *http.Request, workflowID, action string) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := adminPrincipal(r)
	if principal == "" || principal == "unknown" {
		principal = "ui-admin"
	}
	_ = s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "ui",
		Action:    "workflow.graph." + action,
		Target:    workflowID,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
}

// graphEditResult finishes a mutation handler: redirect to the graph view
// on success, else re-render the page with the error banner (422).
func (s *Server) graphEditResult(w http.ResponseWriter, r *http.Request, workflowID, errMsg string) {
	if errMsg == "" {
		http.Redirect(w, r, "/ui/workflows/"+workflowID+"/graph", http.StatusSeeOther)
		return
	}
	data := s.workflowGraphData(workflowID)
	data.Error = errMsg
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, "workflow_graph.html", data)
}

// WorkflowGraphEntrypoint sets the workflow entrypoint (POST
// .../graph/entrypoint). Track B Slice 2.
func (s *Server) WorkflowGraphEntrypoint(w http.ResponseWriter, r *http.Request, workflowID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.graphEditResult(w, r, workflowID, "Failed to parse form: "+err.Error())
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	errMsg := s.applyWorkflowGraphEdit(r, workflowID, "entrypoint", func(fm, body []byte) ([]byte, []byte, error) {
		newFM, err := applyYAMLPatches(fm, []yamlPatch{{Path: []string{"entrypoint"}, Value: id}})
		return newFM, body, err
	})
	s.graphEditResult(w, r, workflowID, errMsg)
}

// WorkflowGraphEdge creates a success/fail edge (POST .../graph/edge).
// Gate edges arrive in Slice 4. Track B Slice 2.
func (s *Server) WorkflowGraphEdge(w http.ResponseWriter, r *http.Request, workflowID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.graphEditResult(w, r, workflowID, "Failed to parse form: "+err.Error())
		return
	}
	src := strings.TrimSpace(r.FormValue("src"))
	dst := strings.TrimSpace(r.FormValue("dst"))
	kind := strings.TrimSpace(r.FormValue("kind"))

	if kind == graphEdgeGate {
		cond := strings.TrimSpace(r.FormValue("condition"))
		errMsg := s.applyWorkflowGraphEdit(r, workflowID, "gate.create", func(fm, body []byte) ([]byte, []byte, error) {
			gates, e := gatesForStep(s.projectReg.GetWorkflow(workflowID), src)
			if e != nil {
				return nil, nil, e
			}
			gates = append(gates, map[string]any{"condition": cond, "target": dst})
			newFM, e := applyYAMLPatches(fm, []yamlPatch{{Path: []string{"steps", src, "gates"}, Value: gates}})
			return newFM, body, e
		})
		s.graphEditResult(w, r, workflowID, errMsg)
		return
	}

	key, err := edgeKindKey(kind)
	if err != nil {
		s.graphEditResult(w, r, workflowID, err.Error())
		return
	}
	errMsg := s.applyWorkflowGraphEdit(r, workflowID, "edge.create", func(fm, body []byte) ([]byte, []byte, error) {
		newFM, e := applyYAMLPatches(fm, []yamlPatch{{Path: []string{"steps", src, key}, Value: dst}})
		return newFM, body, e
	})
	s.graphEditResult(w, r, workflowID, errMsg)
}

// gatesForStep returns the step's current gates as []map[string]any (the
// shape applyYAMLPatches encodes back to a YAML sequence), or an error if
// the step is unknown.
func gatesForStep(wf *registry.Workflow, src string) ([]map[string]any, error) {
	if wf == nil {
		return nil, fmt.Errorf("workflow not found")
	}
	st, ok := wf.Steps[src]
	if !ok {
		return nil, fmt.Errorf("unknown source step %q", src)
	}
	gates := make([]map[string]any, 0, len(st.Gates))
	for _, g := range st.Gates {
		gates = append(gates, map[string]any{"condition": g.Condition, "target": g.Target})
	}
	return gates, nil
}

// WorkflowGraphEdgeDelete removes a success/fail edge (POST
// .../graph/edge/delete). Track B Slice 2.
func (s *Server) WorkflowGraphEdgeDelete(w http.ResponseWriter, r *http.Request, workflowID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.graphEditResult(w, r, workflowID, "Failed to parse form: "+err.Error())
		return
	}
	src := strings.TrimSpace(r.FormValue("src"))
	kind := strings.TrimSpace(r.FormValue("kind"))

	if kind == graphEdgeGate {
		idx, convErr := strconv.Atoi(strings.TrimSpace(r.FormValue("gateIndex")))
		errMsg := s.applyWorkflowGraphEdit(r, workflowID, "gate.delete", func(fm, body []byte) ([]byte, []byte, error) {
			if convErr != nil {
				return nil, nil, fmt.Errorf("invalid gate index")
			}
			gates, e := gatesForStep(s.projectReg.GetWorkflow(workflowID), src)
			if e != nil {
				return nil, nil, e
			}
			if idx < 0 || idx >= len(gates) {
				return nil, nil, fmt.Errorf("gate index %d out of range", idx)
			}
			gates = append(gates[:idx], gates[idx+1:]...)
			if len(gates) == 0 {
				newFM, e := applyYAMLPatches(fm, []yamlPatch{{Path: []string{"steps", src, "gates"}, Value: "", RemoveIfEmpty: true}})
				return newFM, body, e
			}
			newFM, e := applyYAMLPatches(fm, []yamlPatch{{Path: []string{"steps", src, "gates"}, Value: gates}})
			return newFM, body, e
		})
		s.graphEditResult(w, r, workflowID, errMsg)
		return
	}

	key, err := edgeKindKey(kind)
	if err != nil {
		s.graphEditResult(w, r, workflowID, err.Error())
		return
	}
	errMsg := s.applyWorkflowGraphEdit(r, workflowID, "edge.delete", func(fm, body []byte) ([]byte, []byte, error) {
		newFM, e := applyYAMLPatches(fm, []yamlPatch{{Path: []string{"steps", src, key}, Value: "", RemoveIfEmpty: true}})
		return newFM, body, e
	})
	s.graphEditResult(w, r, workflowID, errMsg)
}

// WorkflowGraphNode adds a step (POST .../graph/node) using the
// insert-into-edge model: the new node is wired in from an existing step
// (so it is reachable) and forwards to that step's previous target (so
// nothing is orphaned). All step types are accepted; the validator reports
// any type-specific required fields. Track B Slice 3.
func (s *Server) WorkflowGraphNode(w http.ResponseWriter, r *http.Request, workflowID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.graphEditResult(w, r, workflowID, "Failed to parse form: "+err.Error())
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	typ := strings.TrimSpace(r.FormValue("type"))
	from := strings.TrimSpace(r.FormValue("from"))
	if id == "" || strings.ContainsAny(id, "/. \t") {
		s.graphEditResult(w, r, workflowID, "Invalid step id")
		return
	}
	if typ == "" {
		s.graphEditResult(w, r, workflowID, "Step type is required")
		return
	}
	fromKey, err := edgeKindKey(r.FormValue("fromKind"))
	if err != nil {
		s.graphEditResult(w, r, workflowID, err.Error())
		return
	}
	errMsg := s.applyWorkflowGraphEdit(r, workflowID, "node.add", func(fm, body []byte) ([]byte, []byte, error) {
		wf := s.projectReg.GetWorkflow(workflowID)
		if wf == nil {
			return nil, nil, fmt.Errorf("workflow not found")
		}
		if _, exists := wf.Steps[id]; exists {
			return nil, nil, fmt.Errorf("step %q already exists", id)
		}
		fromStep, ok := wf.Steps[from]
		if !ok {
			return nil, nil, fmt.Errorf("unknown source step %q", from)
		}
		oldTarget := routingTarget(fromStep, fromKey)

		patches := []yamlPatch{{Path: []string{"steps", id, "type"}, Value: typ}}
		if oldTarget != "" {
			patches = append(patches, yamlPatch{Path: []string{"steps", id, "on_success"}, Value: oldTarget})
		}
		// agent/plan steps require a role; default to the source step's role
		// (a known-valid role in the same swarm). The operator can change it
		// in the form. Other types' required fields surface as validator
		// messages — those types are created via the schema form.
		if (typ == "agent" || typ == "plan") && fromStep.Role != "" {
			patches = append(patches, yamlPatch{Path: []string{"steps", id, "role"}, Value: fromStep.Role})
		}
		patches = append(patches, yamlPatch{Path: []string{"steps", from, fromKey}, Value: id})
		newFM, e := applyYAMLPatches(fm, patches)
		if e != nil {
			return nil, nil, e
		}
		// Seed a placeholder body prompt so prompt-requiring step types
		// (e.g. agent) validate; the operator edits it in the form.
		newBody, e := registry.ReplaceWorkflowStepPrompts(body, map[string]string{
			id: "TODO: describe this step.",
		})
		return newFM, newBody, e
	})
	s.graphEditResult(w, r, workflowID, errMsg)
}

// WorkflowGraphNodeDelete removes a step (POST .../graph/node/delete),
// dropping the steps{} key, clearing inbound success/fail edges, and pruning
// the deleted step's body prompt subsection (an orphan subsection is a parse
// error). Track B Slice 3.
func (s *Server) WorkflowGraphNodeDelete(w http.ResponseWriter, r *http.Request, workflowID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.graphEditResult(w, r, workflowID, "Failed to parse form: "+err.Error())
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	errMsg := s.applyWorkflowGraphEdit(r, workflowID, "node.delete", func(fm, body []byte) ([]byte, []byte, error) {
		wf := s.projectReg.GetWorkflow(workflowID)
		if wf == nil {
			return nil, nil, fmt.Errorf("workflow not found")
		}
		if _, exists := wf.Steps[id]; !exists {
			return nil, nil, fmt.Errorf("step %q not found", id)
		}

		// Survivors: every step except the deleted one (key-only reconcile
		// items drop the deleted key while preserving the rest in place).
		survivors := make([]mappingItem, 0, len(wf.Steps)-1)
		keep := make(map[string]bool, len(wf.Steps)-1)
		var inboundClears []yamlPatch
		for sid, st := range wf.Steps {
			if sid == id {
				continue
			}
			survivors = append(survivors, mappingItem{Key: sid})
			keep[sid] = true
			if st.OnSuccess == id {
				inboundClears = append(inboundClears, yamlPatch{Path: []string{"steps", sid, "on_success"}, Value: "", RemoveIfEmpty: true})
			}
			if st.OnFail == id {
				inboundClears = append(inboundClears, yamlPatch{Path: []string{"steps", sid, "on_fail"}, Value: "", RemoveIfEmpty: true})
			}
		}

		newFM, e := reconcileYAMLMapping(fm, "steps", survivors)
		if e != nil {
			return nil, nil, e
		}
		if len(inboundClears) > 0 {
			newFM, e = applyYAMLPatches(newFM, inboundClears)
			if e != nil {
				return nil, nil, e
			}
		}
		newBody, e := registry.ReplaceWorkflowStepPromptsKeeping(body, nil, keep)
		return newFM, newBody, e
	})
	s.graphEditResult(w, r, workflowID, errMsg)
}

// routingTarget returns the current target of a step's success/fail routing
// key (the value behind on_success / on_fail).
func routingTarget(st registry.WorkflowStep, key string) string {
	if key == "on_fail" {
		return st.OnFail
	}
	return st.OnSuccess
}

// edgeKindKey maps a UI edge kind to its WorkflowStep YAML routing key.
// Gate edges are handled by a dedicated path (Slice 4), not here.
func edgeKindKey(kind string) (string, error) {
	switch strings.TrimSpace(kind) {
	case graphEdgeSuccess:
		return "on_success", nil
	case graphEdgeFail:
		return "on_fail", nil
	case graphEdgeGate:
		return "", fmt.Errorf("gate edges are authored with the gate control")
	default:
		return "", fmt.Errorf("unknown edge kind %q", kind)
	}
}

// WorkflowGraphData backs the read-only workflow graph view.
type WorkflowGraphData struct {
	Title       string
	CurrentPage string
	WorkflowID  string
	Error       string
	Graph       GraphView
	// StepIDs / NodeIDs back the editing-control <select> lists.
	StepIDs []string // step ids only (valid edge sources)
	NodeIDs []string // steps + terminals (valid edge targets)
}

// WorkflowGraph renders the read-only node-graph view of a workflow
// (GET /workflows/{id}/graph). Track B Slice 1 — visualization only;
// editing arrives in later slices. Server-rendered SVG, mirroring
// MemorySubgraph: positions are computed by layoutWorkflow and the
// template just plots primitives (no client layout library).
func (s *Server) WorkflowGraph(w http.ResponseWriter, r *http.Request, workflowID string) {
	data := s.workflowGraphData(workflowID)
	switch data.Error {
	case "":
		// ok
	case "Invalid workflow id":
		w.WriteHeader(http.StatusBadRequest)
	case "Project registry not configured":
		w.WriteHeader(http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "workflow_graph.html", data)
}

// workflowGraphData builds the graph view model for a workflow: the
// laid-out graph plus the select-option lists the editing controls need.
// On any lookup failure it returns data with Error set (and an empty
// graph) so callers can render the page with a banner.
func (s *Server) workflowGraphData(workflowID string) WorkflowGraphData {
	data := WorkflowGraphData{
		Title:       "Workflow graph: " + workflowID,
		CurrentPage: "workflows",
		WorkflowID:  workflowID,
	}
	if workflowID == "" || strings.Contains(workflowID, "/") || strings.Contains(workflowID, "..") {
		data.Error = "Invalid workflow id"
		return data
	}
	if s.projectReg == nil {
		data.Error = "Project registry not configured"
		return data
	}
	wf := s.projectReg.GetWorkflow(workflowID)
	if wf == nil {
		data.Error = "Workflow not found"
		return data
	}
	data.Graph = layoutWorkflow(wf)
	for _, n := range data.Graph.Nodes {
		data.NodeIDs = append(data.NodeIDs, n.ID)
		if n.Kind == graphKindStep {
			data.StepIDs = append(data.StepIDs, n.ID)
		}
	}
	return data
}
