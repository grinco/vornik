package api

// Profile-use audit REST surface — the read side of the Phase-B
// audit feature (the write side is the dispatcher's per-turn
// insert via profile_use_audit). One endpoint:
//
//   GET /api/v1/operators/{id}/audit?limit=&since=&until=
//
// Operators query this via `vornikctl operator audit` to answer
// "when did the model start citing my 'prefers Czech'
// preference?". Results paginate newest-first.

import (
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProfileUseAuditEntryJSON is the wire shape for one audit row.
type ProfileUseAuditEntryJSON struct {
	ID         int64    `json:"id"`
	OperatorID string   `json:"operator_id"`
	TaskID     string   `json:"task_id,omitempty"`
	UsedKeys   []string `json:"used_keys"`
	UsedNotes  bool     `json:"used_notes"`
	CreatedAt  string   `json:"created_at"`
}

// ProfileUseAuditResponse wraps the list result.
type ProfileUseAuditResponse struct {
	Entries []ProfileUseAuditEntryJSON `json:"entries"`
}

// ListOperatorAudit handles
// GET /api/v1/operators/{id}/audit?limit&since&until.
func (s *Server) ListOperatorAudit(w http.ResponseWriter, r *http.Request, operatorID string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.profileUseAuditRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"profile-use audit repository not wired")
		return
	}
	if operatorID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "operator id required")
		return
	}
	q := persistence.ProfileUseAuditQuery{}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Since = t
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Until = t
		}
	}
	rows, err := s.profileUseAuditRepo.ListForOperator(r.Context(), operatorID, q)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	out := make([]ProfileUseAuditEntryJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, profileUseAuditToJSON(row))
	}
	respondJSON(w, http.StatusOK, ProfileUseAuditResponse{Entries: out})
}

func profileUseAuditToJSON(r *persistence.ProfileUseAudit) ProfileUseAuditEntryJSON {
	keys := r.UsedKeys
	if keys == nil {
		keys = []string{}
	}
	return ProfileUseAuditEntryJSON{
		ID:         r.ID,
		OperatorID: r.OperatorID,
		TaskID:     r.TaskID,
		UsedKeys:   keys,
		UsedNotes:  r.UsedNotes,
		CreatedAt:  r.CreatedAt.UTC().Format(time.RFC3339),
	}
}
