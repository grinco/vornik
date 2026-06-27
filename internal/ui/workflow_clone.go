package ui

// Clone an existing WORKFLOW.md under a new id — UI Design Refresh Track C
// (clone). The new file is the source with workflowId (and optionally
// displayName) rewritten; steps/terminals/body carry over unchanged. Validated
// + reloaded through the same pipeline as the schema editor.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// WorkflowClone handles POST /workflows/{id}/clone (form: newId, displayName?).
func (s *Server) WorkflowClone(w http.ResponseWriter, r *http.Request, srcID string) {
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if srcID == "" || strings.Contains(srcID, "/") || strings.Contains(srcID, "..") {
		http.Error(w, "invalid source id", http.StatusBadRequest)
		return
	}
	newID := strings.TrimSpace(r.FormValue("newId"))
	if !workflowIDPattern.MatchString(newID) {
		http.Error(w, "Invalid new workflow id (lowercase letters, digits, hyphens; 3–32 chars)", http.StatusBadRequest)
		return
	}
	dir := s.configDir()
	if dir == "" || s.projectReg == nil {
		http.Error(w, "registry not configured", http.StatusServiceUnavailable)
		return
	}

	srcBytes, err := os.ReadFile(filepath.Join(dir, "workflows", srcID+".md"))
	if err != nil {
		http.Error(w, "Source workflow not found", http.StatusNotFound)
		return
	}
	target := filepath.Join(dir, "workflows", newID+".md")
	if _, statErr := os.Stat(target); statErr == nil {
		http.Error(w, "A workflow with that id already exists", http.StatusConflict)
		return
	} else if !errors.Is(statErr, os.ErrNotExist) {
		http.Error(w, "filesystem check failed: "+statErr.Error(), http.StatusInternalServerError)
		return
	}

	fm, body, err := registry.SplitWorkflowContent(srcBytes, srcID+".md")
	if err != nil {
		http.Error(w, "failed to split source workflow: "+err.Error(), http.StatusBadRequest)
		return
	}
	patches := []yamlPatch{{Path: []string{"workflowId"}, Value: newID}}
	if dn := strings.TrimSpace(r.FormValue("displayName")); dn != "" {
		patches = append(patches, yamlPatch{Path: []string{"displayName"}, Value: dn})
	}
	newFM, err := applyYAMLPatches(fm, patches)
	if err != nil {
		http.Error(w, "failed to rewrite workflow id: "+err.Error(), http.StatusInternalServerError)
		return
	}

	joined := registry.JoinWorkflowContent(newFM, body)
	parsed, err := registry.ParseWorkflowMarkdown(joined, newID+".md")
	if err != nil {
		http.Error(w, trimWorkflowParserPrefix(err.Error()), http.StatusBadRequest)
		return
	}
	if err := parsed.Validate(newID + ".md"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(target, joined, 0o600); err != nil {
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			http.Error(w, "cloned to workflows/"+newID+".md but daemon reload failed: "+err.Error(), http.StatusConflict)
			return
		}
	} else {
		_ = s.projectReg.Load(dir)
	}
	s.writeWorkflowGraphAudit(r, newID, "clone")
	http.Redirect(w, r, "/ui/workflows/"+newID+"/graph", http.StatusSeeOther)
}
