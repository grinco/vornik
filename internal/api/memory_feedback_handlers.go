package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// memoryFeedbackResponse is the JSON shape for
// GET /api/v1/projects/{p}/memory/feedback. Mirrors
// persistence.MemoryFeedbackStats plus an optional sample of
// auto-prune candidate chunk IDs.
type memoryFeedbackResponse struct {
	WindowDays           int      `json:"window_days"`
	TotalChunks          int      `json:"total_chunks"`
	RetrievedChunks      int      `json:"retrieved_chunks"`
	UnretrievedChunks    int      `json:"unretrieved_chunks"`
	TotalSearches        int      `json:"total_searches"`
	UnretrievedSampleIDs []string `json:"unretrieved_sample_ids,omitempty"`
}

// MemoryFeedback handles GET /api/v1/projects/{projectID}/memory/feedback.
// Returns chunk-utility analytics for the named project over a rolling
// window: how many chunks are indexed, how many were retrieved at
// least once in the window, and a sample of unretrieved chunk IDs
// the operator can review for pruning.
//
// Query params:
//   - days: window length in days (default 30, capped at 365)
//   - sample: number of unretrieved chunk IDs to return (default 20, capped at 200)
func (s *Server) MemoryFeedback(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memoryAuditRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_AUDIT_NOT_CONFIGURED",
			"memory feedback requires the retrieval audit repo (memory.enabled + post-2026.4.30 schema)")
		return
	}
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}

	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d > 0 {
			if d > 365 {
				d = 365
			}
			days = d
		}
	}
	sample := 20
	if v := r.URL.Query().Get("sample"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			if n > 200 {
				n = 200
			}
			sample = n
		}
	}

	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	stats, err := s.memoryAuditRepo.FeedbackStats(r.Context(), projectID, since)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "FEEDBACK_FAILED", err.Error())
		return
	}

	resp := memoryFeedbackResponse{
		WindowDays:        days,
		TotalChunks:       stats.TotalChunks,
		RetrievedChunks:   stats.RetrievedChunks,
		UnretrievedChunks: stats.UnretrievedChunks,
		TotalSearches:     stats.TotalSearches,
	}
	if sample > 0 && stats.UnretrievedChunks > 0 {
		ids, err := s.memoryAuditRepo.UnretrievedChunkIDs(r.Context(), projectID, since, sample)
		if err == nil {
			resp.UnretrievedSampleIDs = ids
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn().Err(err).Msg("memory feedback: response encode failed")
	}
}
