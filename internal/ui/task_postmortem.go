package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"vornik.io/vornik/internal/api"
)

// TaskPostMortemGenerate handles POST /tasks/{id}/post-mortem.
// Synchronous: blocks the handler while the LLM call runs (with
// a generous 60s budget — explainers run on a small judge-tier
// model and typically complete in 5-15s) and then redirects
// back to the task detail page where the cached row is now
// visible.
//
// Uses force_refresh=true when the operator submits the form
// with that hidden field so a re-trigger after a re-run actually
// re-fires the LLM instead of returning the stale cached row.
//
// Auth: the UI subtree is wrapped by api.AuthMiddleware as of
// 2026-05-04. The 2026-05-27 security audit (#3/#4/#6 et al.)
// added row-level project-scope checks to every mutating UI
// handler — post-mortem joined the sweep here. A scoped key
// for project A must not trigger an LLM call (cost-bearing,
// audit-row-emitting) on a project B task.
func (s *Server) TaskPostMortemGenerate(w http.ResponseWriter, r *http.Request, taskID string) {
	if taskID == "" {
		http.Error(w, "task id required", http.StatusBadRequest)
		return
	}
	if s.postMortemExplainer == nil {
		http.Error(w, "post-mortem explainer not configured on this daemon", http.StatusServiceUnavailable)
		return
	}
	if s.taskRepo != nil {
		// Confirm the task exists before burning an LLM call —
		// avoids a 5-second wasted timeout when the operator
		// hits the URL with a typo. Doubles as the scope-check
		// site: refuse to fire on tasks the caller can't see.
		task, err := s.taskRepo.Get(r.Context(), taskID)
		if err != nil || task == nil {
			http.NotFound(w, r)
			return
		}
		if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
			http.NotFound(w, r)
			return
		}
	}

	forceRefresh := r.FormValue("force_refresh") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := s.postMortemExplainer.Generate(ctx, taskID, forceRefresh)
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("post-mortem generate failed")
		http.Redirect(w, r,
			fmt.Sprintf("/ui/tasks/%s?post_mortem_error=%s", taskID, url.QueryEscape(truncateError(err))),
			http.StatusSeeOther,
		)
		return
	}
	suffix := "&post_mortem=generated"
	if res != nil && res.Cached {
		suffix = "&post_mortem=cached"
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/tasks/%s?notice=post-mortem-ready%s", taskID, suffix), http.StatusSeeOther)
}

// truncateError keeps the redirect URL short — long error
// messages would overflow URL length limits and look ugly in
// the address bar.
func truncateError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
