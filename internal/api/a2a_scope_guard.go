package api

import (
	"net/http"
	"slices"
	"strings"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// a2aScopeGuard wraps the A2A agent-route handler
// (/a2a/v1/agents/<project>/<workflow>/...) with the project- and
// workflow-scope checks the internal/conversation/a2a package cannot
// perform itself — it can't import internal/api (that's an import
// cycle), so the enforcement lives here, where the scope-helper
// convention (requestAllowsProject) and the auth identity context
// already live.
//
// Without this, handleTaskSubmit created a task in the URL's project
// regardless of the caller key's BoundProjectID — any authenticated
// key could invoke any published workflow on any project (authz
// bypass, a2a-expert-federation-design §4). The guard enforces:
//
//  1. The caller's key is scoped to <project> (RequestAllowsProject —
//     which returns true for admin / auth-off / unscoped keys, so
//     single-tenant and admin flows are unaffected).
//  2. If the key's AllowedWorkflows allowlist is non-empty, <workflow>
//     must be in it.
//
// The public /.well-known/ card path is NOT wrapped (card metadata is
// public per the A2A spec); only /a2a/v1/agents/ routes through here.
func a2aScopeGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/a2a/v1/agents/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 {
			// Malformed path — let the handler emit its own 400.
			next(w, r)
			return
		}
		projectID, workflowID := parts[0], parts[1]

		if !RequestAllowsProject(r, projectID) {
			respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
			return
		}

		// Workflow-level narrowing: if the key declares an
		// AllowedWorkflows allowlist, honour it. The full key row is
		// stamped into the identity by the db-keys backend.
		if id := IdentityFromContext(r.Context()); id != nil {
			if row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey); ok &&
				len(row.AllowedWorkflows) > 0 &&
				!slices.Contains(row.AllowedWorkflows, workflowID) {
				respondError(w, http.StatusForbidden, "FORBIDDEN", "workflow not in key allowlist")
				return
			}
		}

		next(w, r)
	}
}
