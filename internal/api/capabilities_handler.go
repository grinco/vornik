package api

import (
	"net/http"
	"time"

	"vornik.io/vornik/internal/version"
)

// CapabilitiesResponse is the wire shape of GET /api/v1/capabilities.
//
// Companion plugins (Claude Code today, Codex / opencode / Gemini CLI
// tomorrow) call this on first connect to discover what the daemon
// supports without hard-coding versions or feature names. The shape
// is deliberately additive: never rename a feature flag once it ships;
// flip a bool, or add a new key.
//
// AllowedProjects and AllowedWorkflows are filtered to the calling
// key's scope when auth is enabled (same semantics as ListProjects /
// ListWorkflows). When auth is disabled (single-operator dev mode),
// both fields are nil and the client should treat that as "no scope
// filter, full registry visible".
type CapabilitiesResponse struct {
	Version          string            `json:"version"`
	APIVersion       string            `json:"apiVersion"`
	Transports       []string          `json:"transports"`
	Features         map[string]bool   `json:"features"`
	AllowedProjects  []ProjectSummary  `json:"allowedProjects"`
	AllowedWorkflows []WorkflowSummary `json:"allowedWorkflows"`
	ServerTime       time.Time         `json:"serverTime"`
}

// GetCapabilities handles GET /api/v1/capabilities. Read-only,
// passes through the same auth middleware as the rest of /api/v1
// (so an unauthenticated request 401s when auth is on, and an
// authenticated request sees only its scoped projects/workflows).
//
// Adding a new capability:
//   - For boolean knobs, append a key to Features. Existing clients
//     that don't recognise it ignore it; clients that need it check
//     for true. Never rename a key once shipped.
//   - For richer surfaces (e.g. companion MCP endpoint path), add a
//     new sibling field on CapabilitiesResponse. Old clients ignore
//     unknown fields by default in encoding/json.
func (s *Server) GetCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}

	resp := CapabilitiesResponse{
		Version:    version.Default,
		APIVersion: "v1",
		Transports: []string{"http", "sse"},
		Features:   s.featureFlags(),
		ServerTime: time.Now().UTC(),
	}

	if s.projectRegistry != nil {
		allowedProjects, scoped := requestScopedProjectSet(r)

		projects := s.projectRegistry.ListProjects()
		projSummaries := make([]ProjectSummary, 0, len(projects))
		for _, p := range projects {
			if p == nil {
				continue
			}
			if scoped && !allowedProjects[p.ID] {
				continue
			}
			projSummaries = append(projSummaries, projectSummaryFrom(p))
		}
		resp.AllowedProjects = projSummaries

		workflows := s.projectRegistry.ListWorkflows()
		visibleWFs := visibleWorkflowIDs(r, s.projectRegistry)
		wfSummaries := make([]WorkflowSummary, 0, len(workflows))
		for _, wf := range workflows {
			if wf == nil {
				continue
			}
			if visibleWFs != nil && !visibleWFs[wf.ID] {
				continue
			}
			wfSummaries = append(wfSummaries, workflowSummaryFrom(wf))
		}
		resp.AllowedWorkflows = wfSummaries
	}

	respondJSON(w, http.StatusOK, resp)
}

// featureFlags returns the daemon's capability flag map. Keep keys
// stable forever once shipped — clients pin behaviour on these.
// True means "fully wired and ready"; false means "not enabled in
// this build / config".
func (s *Server) featureFlags() map[string]bool {
	flags := map[string]bool{
		"tasks-v1":               true,
		"sse-events":             true,
		"registry-introspection": true,
		"project-templates":      true,
		"webhooks":               true,
		// Companion-plugin admin surface (LLD 21). True iff the
		// daemon has both the api-key persistence layer wired
		// AND a project registry to validate grants against.
		// The companion MCP server (companion-mcp) needs the
		// additional task-creator + task-repo dependencies to
		// actually serve delegate / status / result; the flag
		// stays honest so a half-wired daemon can advertise
		// admin-only mode without misleading the plugin.
		"companion-v1":  s.apiKeyRepo != nil && s.projectRegistry != nil,
		"companion-mcp": s.apiKeyRepo != nil && s.projectRegistry != nil && s.taskCreator != nil && s.taskRepo != nil,
		// A2A inbound — third parties posting tasks to vornik via
		// /a2a/v1/agents/. True only when the handler has been
		// explicitly wired in service.Container.
		"a2a-inbound": s.a2aHandler != nil,
	}
	return flags
}
