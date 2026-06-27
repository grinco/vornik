package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Phase 2/3 of memory hardening: HTTP surface for epochs, rollback,
// quarantine, and the composite health view that powers both the
// CLI and the dashboard's RAG section.

// MemoryEpochs handles GET /api/v1/projects/{id}/memory/epochs?limit=N.
// Returns the recent epoch manifest list so the CLI/UI can surface
// snapshots + the rollback-target picker.
func (s *Server) MemoryEpochs(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.corpusEpochs == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_HARDENING_DISABLED",
			"corpus epoch repository not configured")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	epochs, err := s.corpusEpochs.ListEpochs(r.Context(), projectID, limit)
	if err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).Msg("api: list epochs failed")
		respondError(w, http.StatusInternalServerError, "EPOCHS_ERROR", "list epochs failed")
		return
	}
	out := make([]map[string]any, 0, len(epochs))
	for _, e := range epochs {
		out = append(out, marshalEpoch(e))
	}
	respondJSON(w, http.StatusOK, map[string]any{"epochs": out, "total": len(out)})
}

// MemoryRollback handles POST /api/v1/projects/{id}/memory/rollback.
// Body: { target_epoch_id, reason?, triggered_by?, apply? }
// Default is dry-run preview. Response includes plan + (when
// apply=true) the actual deactivated/reactivated counts.
func (s *Server) MemoryRollback(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.corpusEpochs == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_HARDENING_DISABLED",
			"corpus epoch repository not configured")
		return
	}
	var req struct {
		TargetEpochID string `json:"target_epoch_id"`
		Reason        string `json:"reason"`
		TriggeredBy   string `json:"triggered_by"`
		Apply         bool   `json:"apply"`
	}
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	if req.TargetEpochID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "target_epoch_id is required")
		return
	}
	if req.TriggeredBy == "" {
		req.TriggeredBy = "system"
	}

	target, err := s.corpusEpochs.GetEpoch(r.Context(), req.TargetEpochID)
	if err != nil {
		respondError(w, http.StatusNotFound, "EPOCH_NOT_FOUND",
			"target epoch lookup failed: "+err.Error())
		return
	}
	if target.ProjectID != projectID {
		respondError(w, http.StatusBadRequest, "EPOCH_PROJECT_MISMATCH",
			"target epoch belongs to project "+target.ProjectID)
		return
	}

	// Compute the plan (always — same query as CLI's preview).
	all, err := s.corpusEpochs.ListEpochs(r.Context(), projectID, 500)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "EPOCHS_ERROR", err.Error())
		return
	}
	wouldDeactivate, wouldReactivate := 0, 0
	for _, e := range all {
		if e.CreatedAt.After(target.CreatedAt) {
			if e.IsActive {
				wouldDeactivate++
			}
		} else if e.ClosedAt != nil && !e.IsActive {
			wouldReactivate++
		}
	}

	resp := map[string]any{
		"project":         projectID,
		"target":          target.ID,
		"targetCreatedAt": target.CreatedAt.UTC().Format(time.RFC3339),
		"wouldDeactivate": wouldDeactivate,
		"wouldReactivate": wouldReactivate,
		"applied":         false,
	}
	// Restore-pass preview (migration 89, rollback × supersession):
	// how many superseded chunks would come back, and how many CANNOT
	// (no recorded provenance — pre-migration history) so the gap is
	// visible rather than silent. Best-effort: a count failure doesn't
	// block the rollback itself.
	if restorable, nonRestorable, cerr := s.corpusEpochs.CountRollbackRestorable(r.Context(), projectID, req.TargetEpochID); cerr == nil {
		resp["wouldRestoreChunks"] = restorable
		resp["nonRestorableSupersessions"] = nonRestorable
	} else {
		s.logger.Warn().Err(cerr).Str("project_id", projectID).
			Msg("api: rollback restore-preview count failed (non-fatal)")
	}
	if !req.Apply {
		respondJSON(w, http.StatusOK, resp)
		return
	}

	deact, act, restored, rerr := s.corpusEpochs.RollbackTo(r.Context(), projectID, req.TargetEpochID, req.TriggeredBy, req.Reason)
	if rerr != nil {
		s.logger.Warn().Err(rerr).Str("project_id", projectID).Str("target", req.TargetEpochID).
			Msg("api: rollback failed")
		respondError(w, http.StatusInternalServerError, "ROLLBACK_ERROR", rerr.Error())
		return
	}
	resp["applied"] = true
	resp["actuallyDeactivated"] = deact
	resp["actuallyReactivated"] = act
	resp["chunksRestored"] = restored
	respondJSON(w, http.StatusOK, resp)
}

// MemoryRollbacks handles GET /api/v1/projects/{id}/memory/rollbacks?limit=N.
// Audit trail of past rollback events.
func (s *Server) MemoryRollbacks(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.corpusEpochs == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_HARDENING_DISABLED", "")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	rows, err := s.corpusEpochs.ListRollbacks(r.Context(), projectID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "ROLLBACKS_ERROR", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rb := range rows {
		out = append(out, map[string]any{
			"id":          rb.ID,
			"fromEpochId": ptrStrToAny(rb.FromEpochID),
			"toEpochId":   ptrStrToAny(rb.ToEpochID),
			"triggeredBy": rb.TriggeredBy,
			"reason":      ptrStrToAny(rb.Reason),
			"appliedAt":   rb.AppliedAt.UTC().Format(time.RFC3339),
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"rollbacks": out, "total": len(out)})
}

// MemoryQuarantineList handles GET /api/v1/projects/{id}/memory/quarantine?limit=N.
func (s *Server) MemoryQuarantineList(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.memoryQuarantine == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_HARDENING_DISABLED",
			"quarantine repository not configured")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	items, err := s.memoryQuarantine.ListPending(r.Context(), projectID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "QUARANTINE_ERROR", err.Error())
		return
	}
	gateCounts, _ := s.memoryQuarantine.CountByGate(r.Context(), projectID)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		preview := it.Content
		if len(preview) > 240 {
			preview = preview[:237] + "..."
		}
		out = append(out, map[string]any{
			"id":               it.ID,
			"sourceArtifactId": it.SourceArtifactID,
			"producerRole":     ptrStrToAny(it.ProducerRole),
			"proposedClass":    ptrStrToAny(it.ProposedClass),
			"failedGate":       it.FailedGate,
			"failureDetail":    ptrStrToAny(it.FailureDetail),
			"contentPreview":   preview,
			"contentBytes":     len(it.Content),
			"contentHash":      it.ContentHash,
			"quarantinedAt":    it.QuarantinedAt.UTC().Format(time.RFC3339),
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items":        out,
		"total":        len(out),
		"countsByGate": gateCounts,
	})
}

// MemoryQuarantineAction handles POST /api/v1/projects/{id}/memory/quarantine/{qid}/{release|drop}.
// Phase 2 ships drop only — release pipes through a re-evaluation of
// the gates which is Phase 4 work. drop is the operator's "this should
// stay out" decision.
func (s *Server) MemoryQuarantineAction(w http.ResponseWriter, r *http.Request, projectID, suffix string) {
	if s.memoryQuarantine == nil {
		respondError(w, http.StatusServiceUnavailable, "MEMORY_HARDENING_DISABLED", "")
		return
	}
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "expected /quarantine/<id>/<action>")
		return
	}
	id, action := parts[0], parts[1]
	if id == "" || action == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id and action required")
		return
	}
	// Defense-in-depth: ensure the quarantine row belongs to the
	// project the URL claims. Cheaper than carrying a project filter
	// through every repo call.
	item, err := s.memoryQuarantine.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "QUARANTINE_NOT_FOUND", "no such quarantine row")
			return
		}
		respondError(w, http.StatusInternalServerError, "QUARANTINE_ERROR", err.Error())
		return
	}
	if item.ProjectID != projectID {
		respondError(w, http.StatusForbidden, "QUARANTINE_PROJECT_MISMATCH",
			"quarantine row belongs to a different project")
		return
	}
	switch action {
	case "drop":
		if err := s.memoryQuarantine.MarkDropped(r.Context(), id); err != nil {
			respondError(w, http.StatusInternalServerError, "QUARANTINE_ERROR", err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"id": id, "action": "dropped"})
	case "release":
		// Phase 2 release without gate re-evaluation: stamp
		// released_at + released_chunk_id="" so operator knows it's
		// no longer pending. Phase 4 will re-run the gates and
		// either ingest or re-quarantine.
		respondError(w, http.StatusNotImplemented, "RELEASE_NOT_IMPLEMENTED",
			"operator-driven release with re-gate is on the roadmap — drop or wait")
	default:
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "action must be release or drop")
	}
}

// MemoryHealth handles GET /api/v1/projects/{id}/memory/health.
// One-shot snapshot the dashboard renders: queue depth, last epoch
// summary, quarantine count, recent rollbacks.
func (s *Server) MemoryHealth(w http.ResponseWriter, r *http.Request, projectID string) {
	out := map[string]any{
		"project":           projectID,
		"phase":             "phase3",
		"epochs":            nil,
		"queueDepth":        0,
		"quarantinePending": 0,
	}
	if s.corpusEpochs != nil {
		eps, err := s.corpusEpochs.ListEpochs(r.Context(), projectID, 5)
		if err == nil {
			out["epochs"] = eps
		}
	}
	if s.ingestQueue != nil {
		if d, err := s.ingestQueue.QueueDepth(r.Context(), projectID); err == nil {
			out["queueDepth"] = d
		}
	}
	if s.memoryQuarantine != nil {
		if items, err := s.memoryQuarantine.ListPending(r.Context(), projectID, 1); err == nil {
			out["quarantinePending"] = len(items)
		}
		if counts, err := s.memoryQuarantine.CountByGate(r.Context(), projectID); err == nil {
			out["quarantineByGate"] = counts
		}
	}
	respondJSON(w, http.StatusOK, out)
}

// marshalEpoch shapes a CorpusEpoch for JSON responses with
// camelCase fields (matches the rest of the API surface).
func marshalEpoch(e *persistence.CorpusEpoch) map[string]any {
	out := map[string]any{
		"id":                e.ID,
		"projectId":         e.ProjectID,
		"createdAt":         e.CreatedAt.UTC().Format(time.RFC3339),
		"chunksAdmitted":    e.ChunksAdmitted,
		"chunksQuarantined": e.ChunksQuarantined,
		"chunksVerified":    e.ChunksVerified,
		"chunksRefuted":     e.ChunksRefuted,
		"chunksSuperseded":  e.ChunksSuperseded,
		"isActive":          e.IsActive,
	}
	if e.ClosedAt != nil {
		out["closedAt"] = e.ClosedAt.UTC().Format(time.RFC3339)
	}
	if e.IngestExecutionID != nil {
		out["ingestExecutionId"] = *e.IngestExecutionID
	}
	if e.Notes != nil {
		out["notes"] = *e.Notes
	}
	return out
}

func ptrStrToAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
