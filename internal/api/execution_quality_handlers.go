package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionQualityList serves the operator's project-scoped execution score
// ledger together with publication backlog health.
func (s *Server) ExecutionQualityList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.executionQualityScores == nil {
		respondError(w, http.StatusServiceUnavailable, "QUALITY_SCORES_DISABLED", "execution quality score repository is not configured")
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "project_id query param required")
		return
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return
	}

	filter := persistence.ExecutionQualityScoreFilter{ProjectIDs: []string{projectID}, PageSize: 100}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		for _, status := range strings.Split(raw, ",") {
			status = strings.TrimSpace(status)
			if !knownExecutionQualityStatus(status) {
				respondError(w, http.StatusBadRequest, "BAD_REQUEST", "unknown quality score status: "+status)
				return
			}
			filter.Statuses = append(filter.Statuses, status)
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("max_score")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || value > 1 {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "max_score must be within [0,1]")
			return
		}
		filter.MaxScore = &value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "since must be RFC3339")
			return
		}
		filter.Since = &value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > 500 {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be between 1 and 500")
			return
		}
		filter.PageSize = value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "offset must be non-negative")
			return
		}
		filter.Offset = value
	}

	rows, err := s.executionQualityScores.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("list execution quality scores")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list execution quality scores")
		return
	}
	publication, err := s.executionQualityScores.PendingTerminalStats(r.Context(), []string{projectID})
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("read execution quality publication backlog")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to read execution quality publication health")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"scores": rows, "count": len(rows), "publication": publication,
		"filters": map[string]any{"project_id": projectID, "statuses": filter.Statuses, "limit": filter.PageSize, "offset": filter.Offset},
	})
}

func knownExecutionQualityStatus(status string) bool {
	switch status {
	case "scored", "missing_contract", "invalid_evidence", "not_applicable":
		return true
	default:
		return false
	}
}
