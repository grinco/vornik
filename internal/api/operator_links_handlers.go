package api

// Operator identity-link REST surface — the cross-channel
// consolidation half of the per-operator profile feature.
//
//   GET    /api/v1/operators/{id}/links                       — list
//   POST   /api/v1/operators/{id}/links                       — create
//                                                               {channel_speaker_id, linked_by?}
//   DELETE /api/v1/operators/{id}/links/{channel_speaker_id}  — drop one
//
// The {id} path segment is the canonical operator id; created
// links all resolve TO that id. To consolidate two existing
// profiles, the operator picks one as the canonical (typically
// the more-populated one) and POSTs the other's speaker id
// here.
//
// See https://docs.vornik.io (Phase A).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// OperatorLinkJSON is the wire shape for one identity-link row.
type OperatorLinkJSON struct {
	ChannelSpeakerID string `json:"channel_speaker_id"`
	OperatorID       string `json:"operator_id"`
	LinkedAt         string `json:"linked_at"`
	LinkedBy         string `json:"linked_by"`
}

// OperatorLinksResponse wraps the list result.
type OperatorLinksResponse struct {
	Links []OperatorLinkJSON `json:"links"`
}

// OperatorLinkCreateRequest is the POST body for new links.
type OperatorLinkCreateRequest struct {
	ChannelSpeakerID string `json:"channel_speaker_id"`
	LinkedBy         string `json:"linked_by,omitempty"`
}

// ListOperatorLinks handles GET /api/v1/operators/{id}/links.
// Returns every channel speaker id that resolves to {id}.
func (s *Server) ListOperatorLinks(w http.ResponseWriter, r *http.Request, operatorID string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorIdentityLinkRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"operator identity-link repository not wired")
		return
	}
	if operatorID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "operator id required")
		return
	}
	rows, err := s.operatorIdentityLinkRepo.ListForOperator(r.Context(), operatorID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	out := make([]OperatorLinkJSON, 0, len(rows))
	for _, l := range rows {
		out = append(out, operatorLinkToJSON(l))
	}
	respondJSON(w, http.StatusOK, OperatorLinksResponse{Links: out})
}

// CreateOperatorLink handles POST /api/v1/operators/{id}/links.
// Adds a row pointing channel_speaker_id at {id}.
func (s *Server) CreateOperatorLink(w http.ResponseWriter, r *http.Request, operatorID string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorIdentityLinkRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"operator identity-link repository not wired")
		return
	}
	if operatorID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "operator id required")
		return
	}
	var req OperatorLinkCreateRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	req.ChannelSpeakerID = strings.TrimSpace(req.ChannelSpeakerID)
	if req.ChannelSpeakerID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "channel_speaker_id required")
		return
	}
	if req.ChannelSpeakerID == operatorID {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"channel_speaker_id cannot equal the canonical operator id (would be a self-link)")
		return
	}
	linkedBy := strings.TrimSpace(req.LinkedBy)
	if linkedBy == "" {
		linkedBy = "cli"
	}
	link := &persistence.OperatorIdentityLink{
		ChannelSpeakerID: req.ChannelSpeakerID,
		OperatorID:       operatorID,
		LinkedBy:         linkedBy,
	}
	if err := s.operatorIdentityLinkRepo.Upsert(r.Context(), link); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	// Re-fetch so the response carries the persisted linked_at.
	stored, err := s.operatorIdentityLinkRepo.Get(r.Context(), req.ChannelSpeakerID)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if stored == nil {
		// Race: a concurrent delete dropped the row between
		// upsert + reread. Treat as success (upsert returned
		// nil) — the operator will see the row reappear on
		// the next list if they retry.
		stored = link
		stored.LinkedAt = time.Now().UTC()
	}
	respondJSON(w, http.StatusCreated, operatorLinkToJSON(stored))
}

// DeleteOperatorLink handles
// DELETE /api/v1/operators/{id}/links/{channel_speaker_id}.
// {id} is parsed but not strictly enforced — the canonical
// scope check is done client-side by `vornikctl operator unlink`.
func (s *Server) DeleteOperatorLink(w http.ResponseWriter, r *http.Request, operatorID, channelSpeakerID string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorIdentityLinkRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"operator identity-link repository not wired")
		return
	}
	if channelSpeakerID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "channel_speaker_id required")
		return
	}
	if err := s.operatorIdentityLinkRepo.Delete(r.Context(), channelSpeakerID); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func operatorLinkToJSON(l *persistence.OperatorIdentityLink) OperatorLinkJSON {
	return OperatorLinkJSON{
		ChannelSpeakerID: l.ChannelSpeakerID,
		OperatorID:       l.OperatorID,
		LinkedAt:         l.LinkedAt.UTC().Format(time.RFC3339),
		LinkedBy:         l.LinkedBy,
	}
}
