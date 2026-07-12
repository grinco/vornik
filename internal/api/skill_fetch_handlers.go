package api

import (
	"context"
	"net/http"

	"vornik.io/vornik/internal/persistence"
)

// Skill progressive-disclosure fetch (LLD
// 2026-07-12-skill-progressive-disclosure-design). The executor injects
// a compact INDEX of eligible skills into the role system prompt; the
// in-container agent pulls a skill's full body through this endpoint
// (its `skill_fetch` built-in tool) only when the task actually needs
// it. `fired` + the (execution, skill) association are recorded HERE —
// fetch time — so the learning loop's promote/decay worker sees honest
// use instead of v1's "fired = injected" noise.

// skillFetchResponse is the JSON body for a successful fetch.
type skillFetchResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// SkillFetch handles
// GET /api/v1/projects/{projectId}/skills/fetch?name=<n>&execution_id=<eid>.
//
// Eligibility mirrors the executor's index exactly (project OR global,
// maturity active/trusted): a skill the index could not have listed is
// not fetchable — drafts and retired skills 404, and a cross-project
// lookup 404s rather than 403s (existence-leak guard, same posture as
// skill_search).
func (s *Server) SkillFetch(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	// Defense in depth, same as MemorySearch: the global
	// ProjectAuthMiddleware already guards project-scoped endpoints,
	// but re-checking is cheap and this is a prompt-content channel.
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}
	if s.skillStore == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "skill store not wired on this deployment")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "query parameter 'name' is required")
		return
	}

	// List with the index's exact eligibility filter, then match by
	// name — this keeps fetchability and index visibility from ever
	// drifting apart (a dedicated by-name query would need the same
	// project-OR-global + maturity predicates duplicated).
	skills, err := s.skillStore.List(r.Context(), projectID, persistence.SkillListFilter{
		Maturities:    []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		IncludeGlobal: true,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "skill lookup failed")
		return
	}
	var match *persistence.Skill
	for _, sk := range skills {
		if sk.Name == name {
			match = sk
			break
		}
	}
	if match == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no eligible skill by that name")
		return
	}

	// Usage telemetry (learning-loop §D.1) — best-effort; a telemetry
	// failure must never block the fetch.
	_ = s.skillStore.RecordFeedback(r.Context(), match.ID, persistence.SkillSignalFired)
	if s.execSkillRepo != nil {
		if eid := r.URL.Query().Get("execution_id"); eid != "" {
			if err := s.validateSkillFetchExecutionProject(r.Context(), eid, projectID); err != nil {
				respondError(w, http.StatusForbidden, "FORBIDDEN", err.Error())
				return
			}
			_ = s.execSkillRepo.Record(r.Context(), eid, match.ID)
		}
	}

	respondJSON(w, http.StatusOK, skillFetchResponse{
		Name:        match.Name,
		Description: match.Description,
		Body:        match.Body,
	})
}

func (s *Server) validateSkillFetchExecutionProject(ctx context.Context, executionID, projectID string) error {
	if s == nil || s.executionRepo == nil {
		return nil
	}
	exec, err := s.executionRepo.Get(ctx, executionID)
	if err != nil || exec == nil {
		return persistence.ErrNotFound
	}
	if exec.ProjectID != projectID {
		return persistence.ErrNotFound
	}
	return nil
}
