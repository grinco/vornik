package ui

// Project git-over-HTTPS access toggle. Flips the project YAML's
// `git.enabled` key from the project-detail Git-access panel so an operator
// can enable/disable workspace clone+push without hand-editing YAML.
//
// See https://docs.vornik.io Modelled on
// ProjectArchive: admin-gated, git-scoped field guard, atomic YAML write +
// registry reload, redirect back to the detail page.

import (
	"net/http"
)

// ProjectGitToggle enables or disables git-over-HTTPS access for a project.
// POST /ui/projects/{id}/git/toggle with form field `enabled` = "true"|"false"
// (the DESIRED state, not a flip — so a stale double-submit is idempotent).
func (s *Server) ProjectGitToggle(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Enabling exposes the workspace over the network — admin-gated, like
	// every other UI mutation (bypassed only when auth is disabled).
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	enabled := r.FormValue("enabled") == "true"

	// Short-circuit no-op: if the desired state already matches the in-memory
	// registry, skip the write+reload entirely. Makes stale double-submits a
	// pure redirect and narrows the write/reload race window.
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil && p.Git.Enabled == enabled {
			http.Redirect(w, r, gitToggleRedirect(projectID, enabled), http.StatusSeeOther)
			return
		}
	}

	if err := s.applyProjectPatches(projectID, gitPatchGuard, []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: enabled},
	}); err != nil {
		http.Error(w, "failed to update git access: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeProjectLifecycleAudit(r, "project.git-toggle", projectID, map[string]any{
		"enabled": enabled,
	})

	http.Redirect(w, r, gitToggleRedirect(projectID, enabled), http.StatusSeeOther)
}

// gitToggleRedirect builds the post-toggle redirect target. The git_enabled
// query flag drives the success banner / panel arm on the detail page.
func gitToggleRedirect(projectID string, enabled bool) string {
	flag := "0"
	if enabled {
		flag = "1"
	}
	return "/ui/projects/" + projectID + "?git_enabled=" + flag
}
