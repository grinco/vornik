// Composer preview — task 1.2a (NL Automation Composer, design §5.5).
// The tier-3 composer synthesises a full project + swarm + workflow(s)
// bundle inline in the existing project-setup wizard's Converse loop
// (task 1.1b); this file backs the Graph tab of that preview: one
// control-flow SVG per bundle workflow, rendered via the same
// server-side layout+SVG pipeline /ui/workflows/{id}/graph uses
// (workflow_graph_layout.go's layoutWorkflow), through a transient
// registry.Workflow registration so the composed-but-uncommitted
// workflow(s) never touch the loaded config.

package ui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/registry"
)

// composerPreviewGraphRequest is the body of POST
// /ui/projects/new/wizard/graph-preview: the tier-3 envelope's
// bundle.workflows array, posted back by the wizard page's JS
// immediately after a converse turn returns a bundle. Only Workflows
// is needed here — Project/Swarm don't affect the control-flow graph.
type composerPreviewGraphRequest struct {
	Workflows []map[string]any `json:"workflows"`
}

// composerPreviewGraphItem is one rendered graph in the response —
// one per bundle workflow (design §5.4/§11 Q3 caps v1 at 1-2, but this
// handler renders however many the bundle carries).
type composerPreviewGraphItem struct {
	WorkflowID string `json:"workflow_id"`
	SVG        string `json:"svg"`
}

type composerPreviewGraphResponse struct {
	Graphs []composerPreviewGraphItem `json:"graphs"`
}

// ComposerGraphViewData is the template payload for the read-only
// preview SVG (composer_graph_svg.html) — deliberately narrower than
// WorkflowGraphData (no StepIDs/NodeIDs/editing controls): the
// composed workflow isn't committed yet, so there is nothing to edit
// and nowhere real to link a node to.
type ComposerGraphViewData struct {
	WorkflowID string
	Graph      GraphView
}

// ComposerPreviewGraph handles POST
// /ui/projects/new/wizard/graph-preview: renders one control-flow SVG
// per workflow in a tier-3 bundle. Every workflow is registered
// transiently on the live registry (RegisterTransient) just long
// enough to lay out + render, then deregistered (DeregisterTransient)
// — on every path, including a render failure partway through a
// multi-workflow bundle — so a preview render can never leak a
// transient registration into the live registry (design §5.5).
func (s *Server) ComposerPreviewGraph(w http.ResponseWriter, r *http.Request) {
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.projectReg == nil {
		http.Error(w, "project registry not configured", http.StatusServiceUnavailable)
		return
	}

	var body composerPreviewGraphRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "request body must be JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Workflows) == 0 {
		http.Error(w, "at least one workflow is required", http.StatusBadRequest)
		return
	}

	items, err := s.renderComposerPreviewGraphs(body.Workflows)
	if err != nil {
		http.Error(w, "failed to render preview graph: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	writeComposerPreviewJSON(w, http.StatusOK, composerPreviewGraphResponse{Graphs: items})
}

func writeComposerPreviewJSON(w http.ResponseWriter, status int, body composerPreviewGraphResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// renderComposerPreviewGraphs parses workflows (a tier-3 bundle's raw
// bundle.workflows maps) into registry.Workflow values via
// projectwizard.BuildTransientWorkflows, then for each one:
// registers it transiently under a per-call random id (never the
// workflow's own declared id — that avoids any collision with a real
// loaded workflow, or between two concurrent preview requests),
// lays it out + renders its SVG, and deregisters. The deregister runs
// via an inline defer-equivalent (immediate call, not a closure defer)
// so a render failure on workflow N doesn't stop workflows 0..N-1's
// registrations from being cleaned up — each iteration cleans up its
// OWN transient before the loop moves on or returns.
func (s *Server) renderComposerPreviewGraphs(workflows []map[string]any) ([]composerPreviewGraphItem, error) {
	bundle := &projectwizard.ComposedBundle{Workflows: workflows}
	wfs, err := projectwizard.BuildTransientWorkflows(bundle)
	if err != nil {
		return nil, err
	}

	items := make([]composerPreviewGraphItem, 0, len(wfs))
	for i, wf := range wfs {
		// Captured before RegisterTransient overwrites wf.ID with the
		// transient id (registry.Registry.RegisterTransient's
		// documented self-consistency rewrite) — the response should
		// show the bundle's OWN workflow id, not our internal one.
		displayID := wf.ID
		transientID := composerPreviewTransientID(i)
		if err := s.projectReg.RegisterTransient(transientID, wf); err != nil {
			return nil, fmt.Errorf("workflows[%d]: %w", i, err)
		}
		svg, rerr := s.renderComposerGraphSVG(displayID, wf)
		s.projectReg.DeregisterTransient(transientID)
		if rerr != nil {
			return nil, fmt.Errorf("workflows[%d]: %w", i, rerr)
		}
		items = append(items, composerPreviewGraphItem{WorkflowID: displayID, SVG: svg})
	}
	return items, nil
}

// composerPreviewTransientID returns a random, per-call transient
// registry id. RegisterTransient re-registering the same id just
// overwrites, and a loaded config workflow always wins over any
// transient sharing its id — but a preview render has no reason to
// share the id space with anything real, and using a random id per
// call also means two admins previewing concurrently never collide.
func composerPreviewTransientID(index int) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively unreachable in practice;
		// fall back to a monotonic-ish id rather than fail the whole
		// preview render over an id-uniqueness nicety.
		return fmt.Sprintf("composer-preview-fallback-%d-%d", time.Now().UnixNano(), index)
	}
	return fmt.Sprintf("composer-preview-%s-%d", hex.EncodeToString(b[:]), index)
}

// renderComposerGraphSVG lays out wf via the existing pure
// layoutWorkflow (workflow_graph_layout.go) and renders the read-only
// preview SVG fragment (composer_graph_svg.html) to a string.
func (s *Server) renderComposerGraphSVG(workflowID string, wf *registry.Workflow) (string, error) {
	data := ComposerGraphViewData{WorkflowID: workflowID, Graph: layoutWorkflow(wf)}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "composer_graph_svg.html", data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
