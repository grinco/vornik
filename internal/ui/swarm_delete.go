package ui

// Swarm + workflow delete handlers for the editor pages. Wired by
// swarmRouter / workflowRouter on POST {id}/delete; the editor
// templates render a "Delete" button at the bottom whose form
// targets these endpoints with a confirm() JavaScript prompt.
//
// Guardrails:
//   - The resource id is validated identically to the editor path
//     (no slashes / path separators).
//   - Projects that still reference the swarm/workflow block the
//     delete with a clear list of referrers. Operators must update
//     every referring project first.
//   - The .md file is removed from disk; on success the registry
//     is reloaded so the in-memory state matches.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SwarmDelete removes the swarm .md file after verifying no project
// still references the swarm. POST /ui/swarms/{id}/delete.
//
// On error: re-render the editor with the error banner so the
// operator sees which projects still point at the swarm.
// On success: redirect to /ui/swarms with a one-shot ?deleted=<id>
// query parameter so the list page can show a banner.
func (s *Server) SwarmDelete(w http.ResponseWriter, r *http.Request, swarmID string) {
	data := s.swarmEditData(swarmID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "swarm_edit.html", data)
		return
	}

	referrers := s.projectsReferencingSwarm(swarmID)
	if len(referrers) > 0 {
		data.Error = fmt.Sprintf(
			"Cannot delete swarm %q: still referenced by project(s): %s. Update those projects to a different swarmId first.",
			swarmID, strings.Join(referrers, ", "))
		w.WriteHeader(http.StatusConflict)
		s.render(w, "swarm_edit.html", data)
		return
	}

	if err := os.Remove(data.SwarmPath); err != nil {
		data.Error = "Failed to delete swarm file: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarm_edit.html", data)
		return
	}

	if s.configReloader != nil {
		_ = s.configReloader.Reload()
	} else if s.projectReg != nil {
		_ = s.projectReg.Load(s.configDir())
	}

	http.Redirect(w, r, "/ui/swarms?deleted="+swarmID, http.StatusSeeOther)
}

// WorkflowDelete removes the workflow .md file after verifying no
// project still references it as its defaultWorkflowId. POST
// /ui/workflows/{id}/delete.
func (s *Server) WorkflowDelete(w http.ResponseWriter, r *http.Request, workflowID string) {
	data := s.workflowEditData(workflowID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "workflow_edit.html", data)
		return
	}

	referrers := s.projectsReferencingWorkflow(workflowID)
	if len(referrers) > 0 {
		data.Error = fmt.Sprintf(
			"Cannot delete workflow %q: still referenced by project(s): %s. Update those projects to a different defaultWorkflowId first.",
			workflowID, strings.Join(referrers, ", "))
		w.WriteHeader(http.StatusConflict)
		s.render(w, "workflow_edit.html", data)
		return
	}

	if err := os.Remove(data.WorkflowPath); err != nil {
		data.Error = "Failed to delete workflow file: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflow_edit.html", data)
		return
	}

	if s.configReloader != nil {
		_ = s.configReloader.Reload()
	} else if s.projectReg != nil {
		_ = s.projectReg.Load(s.configDir())
	}

	http.Redirect(w, r, "/ui/workflows?deleted="+workflowID, http.StatusSeeOther)
}

// projectsReferencingSwarm returns the sorted list of project IDs whose
// swarmId field matches swarmID. Read from the in-memory registry; an
// empty registry returns an empty list (the editor render already
// guarded for that path).
func (s *Server) projectsReferencingSwarm(swarmID string) []string {
	if s.projectReg == nil {
		return nil
	}
	var out []string
	for _, p := range s.projectReg.ListProjects() {
		if p == nil {
			continue
		}
		if p.SwarmID == swarmID {
			out = append(out, p.ID)
		}
	}
	sort.Strings(out)
	return out
}

// projectsReferencingWorkflow returns the sorted list of project IDs
// whose defaultWorkflowId matches workflowID. Today the only project-
// level reference is defaultWorkflowId; future per-task workflow
// pinning would also need a check here.
func (s *Server) projectsReferencingWorkflow(workflowID string) []string {
	if s.projectReg == nil {
		return nil
	}
	var out []string
	for _, p := range s.projectReg.ListProjects() {
		if p == nil {
			continue
		}
		if p.DefaultWorkflowID == workflowID {
			out = append(out, p.ID)
		}
	}
	sort.Strings(out)
	return out
}

// Path helpers kept here so swarm_edit.go / workflow_edit.go don't
// need to know the underscore separator the templates use.
var _ = filepath.Join // keep filepath imported in case future helpers move here
