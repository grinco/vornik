package api

// Admin workflow-architect propose endpoint — Slice 2c of the
// memetic-workflows arc. Runs one architect turn against the
// supplied workflow ID and persists a pending proposal. Same
// gate matrix as AdminAuditList / AdminWorkflowStats.
//
// Mapped error space (matches sentinels in internal/memetic +
// persistence.ErrProposalRateLimited):
//   - missing workflow             → 400 VALIDATION_ERROR
//   - architect not wired           → 503 ARCHITECT_DISABLED
//   - admin disabled                → 404
//   - no API key                    → 401 UNAUTHORIZED
//   - non-admin key                 → 403 ADMIN_SCOPE_REQUIRED
//   - workflow file not found       → 404 WORKFLOW_NOT_FOUND
//   - rate limited (pending exists) → 429 PROPOSAL_RATE_LIMITED
//   - low confidence                → 204 No Content (no body)
//   - insufficient / invalid evidence, workflow_id mismatch,
//     invalid proposed YAML         → 422 ARCHITECT_VALIDATION_FAILED
//   - malformed LLM output          → 502 ARCHITECT_OUTPUT_INVALID
//   - anything else                 → 500 INTERNAL

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
)

// AdminWorkflowArchitectPropose handles
// POST /api/v1/admin/workflow-architect/propose.
//
// Request body: { "workflow_id": "<id>" }
// Response: 200 with the inserted persistence.WorkflowProposal as
// JSON, OR one of the status codes documented above on failure.
func (s *Server) AdminWorkflowArchitectPropose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if !s.adminConfig.Enabled {
		http.NotFound(w, r)
		return
	}
	if s.workflowArchitect == nil {
		respondError(w, http.StatusServiceUnavailable, "ARCHITECT_DISABLED",
			"workflow-architect not wired on this deployment")
		return
	}
	// D4 (audit 2026-06-10): route through requireAdminGate so the
	// auth-disabled override admits the trusted local operator instead
	// of 401-ing on the inline IsAdminKey check. Same auth-ON matrix.
	if !s.requireAdminGate(w, r) {
		return
	}

	var body struct {
		WorkflowID string `json:"workflow_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.WorkflowID = strings.TrimSpace(body.WorkflowID)
	if body.WorkflowID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"workflow_id is required")
		return
	}

	proposal, err := s.workflowArchitect.Propose(r.Context(), body.WorkflowID)
	if err != nil {
		s.mapArchitectError(w, body.WorkflowID, err)
		return
	}
	respondJSON(w, http.StatusOK, proposal)
}

// mapArchitectError translates memetic sentinels + repository
// errors to HTTP statuses. Kept separate from the handler so the
// status-code matrix is reviewable in one place.
func (s *Server) mapArchitectError(w http.ResponseWriter, workflowID string, err error) {
	switch {
	case errors.Is(err, persistence.ErrProposalRateLimited):
		respondError(w, http.StatusTooManyRequests, "PROPOSAL_RATE_LIMITED",
			"a pending proposal already exists for this workflow")
	case errors.Is(err, memetic.ErrLowConfidence):
		// 204 No Content: the architect ran, no proposal worth
		// surfacing. Operators learn this from the response body
		// being empty rather than from a verbose error.
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, memetic.ErrInsufficientEvidence),
		errors.Is(err, memetic.ErrEvidenceInvalid),
		errors.Is(err, memetic.ErrWorkflowMismatch),
		errors.Is(err, memetic.ErrProposalYAMLInvalid):
		respondError(w, http.StatusUnprocessableEntity, "ARCHITECT_VALIDATION_FAILED", err.Error())
	case errors.Is(err, memetic.ErrMalformedOutput):
		respondError(w, http.StatusBadGateway, "ARCHITECT_OUTPUT_INVALID", err.Error())
	case errors.Is(err, memetic.ErrArchitectPaused):
		respondError(w, http.StatusServiceUnavailable, "ARCHITECT_PAUSED",
			"workflow architect is paused by operator; unset VORNIK_ARCHITECT_PAUSED or the admin config knob to re-enable")
	case errors.Is(err, memetic.ErrArchitectDisabledForWorkflow):
		// LEVEL 2 — per-workflow opt-out via frontmatter.
		respondError(w, http.StatusServiceUnavailable, "ARCHITECT_DISABLED_FOR_WORKFLOW",
			"this workflow has architect_enabled: false in its frontmatter; remove or set true to allow proposals")
	case errors.Is(err, memetic.ErrProposalKindDisabled):
		// LEVEL 3 — operator declined this class of change. 409
		// Conflict: the architect produced a valid proposal but
		// policy forbids the class.
		respondError(w, http.StatusConflict, "PROPOSAL_KIND_DISABLED", err.Error())
	case errors.Is(err, os.ErrNotExist):
		// WorkflowSource bubbles this up when the configs/workflows/<id>.md
		// file doesn't exist. 404 lets the CLI distinguish "typo in
		// workflow ID" from a server error.
		respondError(w, http.StatusNotFound, "WORKFLOW_NOT_FOUND",
			"workflow "+workflowID+" not found on disk")
	default:
		s.logger.Warn().Err(err).
			Str("workflow_id", workflowID).
			Msg("workflow-architect: propose failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}
