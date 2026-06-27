package ui

// Shared project-scope helpers for the cross-project UI pages. A scoped
// session must query its own project(s) directly rather than fetch a
// global latest-N slice and post-filter — otherwise other tenants' rows
// fill the page and the caller's own rows fall past the cap.

import (
	"context"
	"net/http"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// resolveProjectScope decides which projects an aggregate/list page
// should query for the current caller, and the selector options to
// offer. `explicit` is the ?project value ("" = the "All" option).
//
// Returns:
//   - queryIDs: the project IDs to query and merge. A nil slice means
//     "all projects, unscoped" (a single global query) and is returned
//     ONLY for an all-access caller; a project-scoped caller always gets
//     a concrete, non-empty allow-set (their union) so nothing leaks.
//   - options: the selector choices — the caller's allowed projects.
//     For an all-access caller this is every project (admins get a
//     picker too); for a scoped caller it's their allow-set.
//   - ok: false AFTER a 403 has been written (explicit not allowed) or
//     when a scoped caller has zero projects (caller should render an
//     empty state).
//
// "All" semantics: all-access + "" → nil (global); scoped + "" → the
// union of the caller's projects (NOT global — that was the spend "All"
// bug). Either caller + an allowed project X → [X].
func (s *Server) resolveProjectScope(w http.ResponseWriter, r *http.Request, explicit string) (queryIDs []string, options []string, ok bool) {
	scopedSet, isScoped := api.RequestScopedProjects(r)
	if isScoped {
		// Scoped caller: options + query-set come from the context
		// allowlist (authoritative; no registry dependency).
		options = scopedSet
		if explicit != "" {
			if !api.RequestAllowsProject(r, explicit) {
				http.Error(w, "access denied to project", http.StatusForbidden)
				return nil, options, false
			}
			return []string{explicit}, options, true
		}
		// "All" = the union of THEIR projects (non-nil; empty for an
		// awaiting-access caller → query nothing, never global).
		return append([]string{}, scopedSet...), options, true
	}
	// All-access caller: the selector lists every project; "All" = a
	// single global query (nil), an explicit project narrows to it.
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			if p != nil {
				options = append(options, p.ID)
			}
		}
	}
	if explicit != "" {
		return []string{explicit}, options, true
	}
	return nil, options, true
}

// projectsToIterate expands a resolved scope into the per-query project
// list for repo calls that take a single project id: nil (global) → one
// "" (= all projects in repo filters); otherwise the explicit set
// (possibly empty → no queries, for an awaiting-access caller).
func projectsToIterate(queryIDs []string) []string {
	if queryIDs == nil {
		return []string{""}
	}
	return queryIDs
}

// scopeQueryIDs maps a request to the project set a list/stat page
// should query: nil for an all-access caller (a single global query),
// otherwise the caller's allow-set (a per-project merge). This is the
// no-selector counterpart to resolveProjectScope's "All" semantics, for
// pages without a project picker (inbox, live, reminders) — it stops a
// scoped session's own rows being buried past a global latest-N cap.
func scopeQueryIDs(r *http.Request) []string {
	if scoped, isScoped := api.RequestScopedProjects(r); isScoped {
		return scoped
	}
	return nil
}

// listTasksForScope returns the task sample for a resolved scope: nil
// queryIDs = a single global query; otherwise the per-project merge.
func (s *Server) listTasksForScope(ctx context.Context, queryIDs []string, filter persistence.TaskFilter) []*persistence.Task {
	if queryIDs == nil {
		rows, err := s.taskRepo.List(ctx, filter)
		if err != nil {
			s.logger.Warn().Err(err).Msg("scope task list failed")
			return nil
		}
		return rows
	}
	return s.listTasksScoped(ctx, queryIDs, filter)
}
