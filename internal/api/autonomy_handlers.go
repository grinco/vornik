package api

import (
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ListAutonomyEvaluations handles
// GET /api/v1/projects/{projectId}/autonomy/evaluations.
//
// Returns per-tick evaluation rows newest-first, with optional
// ?outcome= filter. Unlike task lists this only reports the audit
// trail — to surface the final task status (COMPLETED / FAILED /
// CANCELLED), pair with GET /projects/{p}/tasks.
func (s *Server) ListAutonomyEvaluations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
	}
	if s.autonomyEvalRepo == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"evaluations": []persistence.AutonomyEvaluation{},
			"total":       0,
		})
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	filter := persistence.AutonomyEvaluationFilter{
		ProjectID: &projectID,
		PageSize:  limit,
	}
	if outcome := r.URL.Query().Get("outcome"); outcome != "" {
		filter.Outcome = &outcome
	}

	rows, err := s.autonomyEvalRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy evaluations list failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list evaluations")
		return
	}
	if rows == nil {
		rows = []*persistence.AutonomyEvaluation{}
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"evaluations": rows,
		"total":       len(rows),
	})
}

// GetAutonomyEvaluationSummary handles
// GET /api/v1/projects/{projectId}/autonomy/summary.
//
// Aggregates evaluation outcomes over the last N hours (default 24).
// Cheap read (one aggregate, bounded time range). Useful for the
// landing-page autonomy widget and for a vornikctl autonomy summary CLI.
func (s *Server) GetAutonomyEvaluationSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
	}
	if s.autonomyEvalRepo == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"projectId": projectID,
			"windowHrs": 24,
			"counts":    map[string]int64{},
		})
		return
	}

	windowHrs := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowHrs = n
		}
	}
	if windowHrs > 24*30 {
		windowHrs = 24 * 30 // cap at 30d to keep the aggregate cheap
	}

	since := time.Now().UTC().Add(-time.Duration(windowHrs) * time.Hour)
	counts, err := s.autonomyEvalRepo.CountByOutcome(r.Context(), projectID, since, time.Time{})
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy summary failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to aggregate evaluations")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"projectId": projectID,
		"windowHrs": windowHrs,
		"since":     since.Format(time.RFC3339),
		"counts":    counts,
	})
}
