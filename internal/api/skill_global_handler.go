package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SkillSetGlobal handles POST /api/v1/skills/{id}/global — the operator
// CLI surface (vornikctl knowledge set-global / set-project) for flipping
// a knowledge skill's cross-project reach (LLD 2026-07-07-cross-project-
// global-skills-design).
//
// It is gated by requireOperatorScope, NOT requireAdminGate: cross-
// project skills are a Community feature (the editions matrix), so this
// route lives OUTSIDE /api/v1/admin/ (that prefix carries the EE admin-
// gate invariant enforced by admin_gate_lint_test) and uses the CE-
// available "daemon-level operator, not project self-service" gate the
// config-show and memory-firewall handlers already use.
//
// Setting global does NOT change maturity — an approved skill stays
// approved and simply widens/narrows where it injects next task.
func (s *Server) SkillSetGlobal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if !s.requireOperatorScope(w, r) {
		return
	}
	if s.skillStore == nil {
		respondError(w, http.StatusServiceUnavailable, "SKILLS_UNAVAILABLE", "skill store not wired on this daemon")
		return
	}
	// Path is /api/v1/skills/{id}/global — pull {id} from the tail.
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/")
	id := strings.TrimSuffix(rest, "/global")
	if id == "" || id == rest || strings.Contains(id, "/") {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "expected /api/v1/skills/{id}/global")
		return
	}

	var body struct {
		Global bool `json:"global"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "expected {\"global\": bool}: "+err.Error())
		return
	}

	// Confirm the skill exists (so the CLI gets a clean 404 rather than a
	// silent no-op) before flipping.
	skill, err := s.skillStore.GetByID(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "skill not found: "+err.Error())
		return
	}
	if err := s.skillStore.SetGlobal(r.Context(), id, body.Global); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "set global failed: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"id":        skill.ID,
		"name":      skill.Name,
		"is_global": body.Global,
	})
}
