package api

import (
	"net/http"
	"sort"

	"vornik.io/vornik/internal/registry"
)

// Registry introspection endpoints.
//
// These mirror the in-memory registry so operators (and the vornikctl CLI)
// can discover what the daemon is actually serving without grepping
// YAML files or tailing journald. All reads are cheap and go through
// Registry's own RWMutex — no DB round-trips.

// ProjectSummary is the shape returned by GET /api/v1/projects. Keeps
// the payload small so list views stay snappy even with hundreds of
// projects; full detail lives behind GET /api/v1/projects/{id}/config.
type ProjectSummary struct {
	ProjectID         string `json:"projectId"`
	DisplayName       string `json:"displayName,omitempty"`
	SwarmID           string `json:"swarmId,omitempty"`
	DefaultWorkflowID string `json:"defaultWorkflowId,omitempty"`
	AutonomyEnabled   bool   `json:"autonomyEnabled"`
}

// ProjectDetail is the full shape returned by GET /api/v1/projects/{id}/config.
// Autonomy goal is included in full so operators can audit what the
// lead is being instructed to do.
type ProjectDetail struct {
	ProjectSummary
	DefaultPriority    int                         `json:"defaultPriority"`
	MaxConcurrentTasks int                         `json:"maxConcurrentTasks,omitempty"`
	Budget             registry.ProjectBudget      `json:"budget"`
	Autonomy           registry.ProjectAutonomy    `json:"autonomy"`
	Retention          registry.ProjectRetention   `json:"retention"`
	Permissions        registry.ProjectPermissions `json:"permissions"`
	MCP                registry.ProjectMCP         `json:"mcp"`
}

// SwarmSummary is the list-view shape for swarms.
type SwarmSummary struct {
	SwarmID     string   `json:"swarmId"`
	DisplayName string   `json:"displayName,omitempty"`
	LeadRole    string   `json:"leadRole,omitempty"`
	Roles       []string `json:"roles,omitempty"`
}

// WorkflowSummary is the list-view shape for workflows.
type WorkflowSummary struct {
	WorkflowID  string   `json:"workflowId"`
	DisplayName string   `json:"displayName,omitempty"`
	Steps       []string `json:"steps,omitempty"`
}

// ListProjects handles GET /api/v1/projects. Returns a summary list of
// every project in the registry. No project-ID scope needed — this is
// the one unscoped projects endpoint.
func (s *Server) ListProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if s.projectRegistry == nil {
		respondJSON(w, http.StatusOK, map[string]any{"projects": []ProjectSummary{}, "total": 0})
		return
	}
	projects := s.projectRegistry.ListProjects()
	out := make([]ProjectSummary, 0, len(projects))
	allowed, scoped := requestScopedProjectSet(r)
	for _, p := range projects {
		if p == nil {
			continue
		}
		if scoped && !allowed[p.ID] {
			continue
		}
		out = append(out, projectSummaryFrom(p))
	}
	respondJSON(w, http.StatusOK, map[string]any{"projects": out, "total": len(out)})
}

// GetProjectConfig handles GET /api/v1/projects/{id}/config. Returns
// the full project definition.
func (s *Server) GetProjectConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.projectRegistry == nil {
		respondError(w, http.StatusServiceUnavailable, "REGISTRY_UNAVAILABLE", "project registry not initialised")
		return
	}
	p := s.projectRegistry.GetProject(projectID)
	if p == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
		return
	}
	respondJSON(w, http.StatusOK, projectDetailFrom(p))
}

// ListSwarms handles GET /api/v1/swarms.
func (s *Server) ListSwarms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if s.projectRegistry == nil {
		respondJSON(w, http.StatusOK, map[string]any{"swarms": []SwarmSummary{}, "total": 0})
		return
	}
	swarms := s.projectRegistry.ListSwarms()
	out := make([]SwarmSummary, 0, len(swarms))
	allowedSwarms := visibleSwarmIDs(r, s.projectRegistry)
	for _, sw := range swarms {
		if sw == nil {
			continue
		}
		if allowedSwarms != nil && !allowedSwarms[sw.ID] {
			continue
		}
		out = append(out, swarmSummaryFrom(sw))
	}
	respondJSON(w, http.StatusOK, map[string]any{"swarms": out, "total": len(out)})
}

// GetSwarm handles GET /api/v1/swarms/{id}. Returns the full swarm
// config including every role's system prompt / permissions — useful
// for auditing what a given role can actually do.
func (s *Server) GetSwarm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	id := extractPathSegmentAfter(r, "swarms")
	if id == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "swarmId is required")
		return
	}
	if s.projectRegistry == nil {
		respondError(w, http.StatusServiceUnavailable, "REGISTRY_UNAVAILABLE", "project registry not initialised")
		return
	}
	if allowed := visibleSwarmIDs(r, s.projectRegistry); allowed != nil && !allowed[id] {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "swarm not allowed")
		return
	}
	sw := s.projectRegistry.GetSwarm(id)
	if sw == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Swarm not found: "+id)
		return
	}
	respondJSON(w, http.StatusOK, sw)
}

// ListWorkflows handles GET /api/v1/workflows.
func (s *Server) ListWorkflows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if s.projectRegistry == nil {
		respondJSON(w, http.StatusOK, map[string]any{"workflows": []WorkflowSummary{}, "total": 0})
		return
	}
	workflows := s.projectRegistry.ListWorkflows()
	out := make([]WorkflowSummary, 0, len(workflows))
	allowedWorkflows := visibleWorkflowIDs(r, s.projectRegistry)
	for _, wf := range workflows {
		if wf == nil {
			continue
		}
		if allowedWorkflows != nil && !allowedWorkflows[wf.ID] {
			continue
		}
		out = append(out, workflowSummaryFrom(wf))
	}
	respondJSON(w, http.StatusOK, map[string]any{"workflows": out, "total": len(out)})
}

// GetWorkflow handles GET /api/v1/workflows/{id}.
func (s *Server) GetWorkflow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	id := extractPathSegmentAfter(r, "workflows")
	if id == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "workflowId is required")
		return
	}
	if s.projectRegistry == nil {
		respondError(w, http.StatusServiceUnavailable, "REGISTRY_UNAVAILABLE", "project registry not initialised")
		return
	}
	if allowed := visibleWorkflowIDs(r, s.projectRegistry); allowed != nil && !allowed[id] {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "workflow not allowed")
		return
	}
	wf := s.projectRegistry.GetWorkflow(id)
	if wf == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Workflow not found: "+id)
		return
	}
	respondJSON(w, http.StatusOK, wf)
}

func visibleSwarmIDs(r *http.Request, reg *registry.Registry) map[string]bool {
	allowedProjects, scoped := requestScopedProjectSet(r)
	if !scoped || reg == nil {
		return nil
	}
	out := make(map[string]bool, len(allowedProjects))
	for projectID := range allowedProjects {
		if p := reg.GetProject(projectID); p != nil && p.SwarmID != "" {
			out[p.SwarmID] = true
		}
	}
	return out
}

func visibleWorkflowIDs(r *http.Request, reg *registry.Registry) map[string]bool {
	allowedProjects, scoped := requestScopedProjectSet(r)
	if !scoped || reg == nil {
		return nil
	}
	out := make(map[string]bool, len(allowedProjects))
	for projectID := range allowedProjects {
		p := reg.GetProject(projectID)
		if p == nil {
			continue
		}
		if p.DefaultWorkflowID != "" {
			out[p.DefaultWorkflowID] = true
		}
		for _, id := range p.AdaptiveCandidateWorkflows {
			if id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// --- adapters ---------------------------------------------------------------

func projectSummaryFrom(p *registry.Project) ProjectSummary {
	return ProjectSummary{
		ProjectID:         p.ID,
		DisplayName:       p.DisplayName,
		SwarmID:           p.SwarmID,
		DefaultWorkflowID: p.DefaultWorkflowID,
		AutonomyEnabled:   p.Autonomy.Enabled,
	}
}

func projectDetailFrom(p *registry.Project) ProjectDetail {
	return ProjectDetail{
		ProjectSummary:     projectSummaryFrom(p),
		DefaultPriority:    p.DefaultPriority,
		MaxConcurrentTasks: p.MaxConcurrentTasks,
		Budget:             p.Budget,
		Autonomy:           p.Autonomy,
		Retention:          p.Retention,
		Permissions:        p.Permissions,
		MCP:                p.MCP,
	}
}

func swarmSummaryFrom(sw *registry.Swarm) SwarmSummary {
	roles := make([]string, 0, len(sw.Roles))
	for _, r := range sw.Roles {
		roles = append(roles, r.Name)
	}
	return SwarmSummary{
		SwarmID:     sw.ID,
		DisplayName: sw.DisplayName,
		LeadRole:    sw.LeadRole,
		Roles:       roles,
	}
}

func workflowSummaryFrom(wf *registry.Workflow) WorkflowSummary {
	steps := make([]string, 0, len(wf.Steps))
	for id := range wf.Steps {
		steps = append(steps, id)
	}
	sort.Strings(steps)
	return WorkflowSummary{
		WorkflowID:  wf.ID,
		DisplayName: wf.DisplayName,
		Steps:       steps,
	}
}
