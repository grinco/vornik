package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Human verdicts on execution output (LLD 2026-09-04-execution-ratings-design).
//
// Three endpoints under /api/v1/executions/{id}/rating: POST to record or
// change one, GET to read the caller's own, DELETE to withdraw it. All three
// act on the CALLER's rating — the rater is the resolved identity, never a
// request parameter, so no caller can file or withdraw a judgement under
// another operator's name.
//
// Nothing here feeds behaviour. A rating informs an operator deciding whether
// to approve or retire an automated change; it never gates a run (design §2.1).

// ExecutionRatingRequest is the POST body.
type ExecutionRatingRequest struct {
	// Verdict is "up" or "down". No scale, by design.
	Verdict string `json:"verdict"`
	// Reason is an optional one-line justification.
	Reason string `json:"reason,omitempty"`
}

// ExecutionRatingResponse is what GET and POST return.
type ExecutionRatingResponse struct {
	ExecutionID string    `json:"execution_id"`
	RaterID     string    `json:"rater_id"`
	Verdict     string    `json:"verdict"`
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func ratingResponse(r *persistence.ExecutionRating) ExecutionRatingResponse {
	return ExecutionRatingResponse{
		ExecutionID: r.ExecutionID,
		RaterID:     r.RaterID,
		Verdict:     r.Verdict,
		Reason:      r.Reason,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// ratingPreflight runs the checks all three handlers share and returns the
// resolved rater. It reports whether the caller may proceed, having already
// written the refusal when not.
//
// Order matters. The 503 comes first so an unwired deployment says so rather
// than 401-ing a caller who is perfectly well identified; the identity check
// comes before the execution lookup so an unidentified caller cannot use this
// endpoint to probe which execution ids exist.
func (s *Server) ratingPreflight(w http.ResponseWriter, r *http.Request, executionID string) (string, bool) {
	if s.ratingRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "RATINGS_DISABLED",
			"execution ratings not wired on this deployment")
		return "", false
	}
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "execution_id is required")
		return "", false
	}

	rater := requestOperatorID(r)
	if rater == "" {
		// Not stored as anonymous. A rating whose author is unknown cannot be
		// edited by its author, attributed in a rollup, or told apart from a
		// second rater's — the three properties this record exists to have.
		//
		// A deployment fronting the API with ONE service key for all users has
		// no per-caller identity and every rating here will 401; it needs
		// per-operator keys. It does NOT get an impersonation header: that path
		// exists only with auth disabled, and re-opening it for a write
		// endpoint would let any authenticated caller file judgements under
		// someone else's name, which is worse than the feature being
		// unavailable because a rollup would then read forged rows.
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED",
			"a rating needs an identified rater; this deployment resolved none")
		return "", false
	}

	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR",
			"Execution repository not available")
		return "", false
	}
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return "", false
		}
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("rating: failed to load execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return "", false
	}
	if exec == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
		return "", false
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return "", false
	}
	return rater, true
}

// ExecutionRatingUpsert handles POST /api/v1/executions/{id}/rating.
//
// Records or replaces the caller's verdict. Re-rating is ordinary: an
// operator's first reaction to a digest is not their considered one, and a
// rating that cannot be changed is one that gets left wrong.
func (s *Server) ExecutionRatingUpsert(w http.ResponseWriter, r *http.Request, executionID string) {
	rater, ok := s.ratingPreflight(w, r, executionID)
	if !ok {
		return
	}

	var body ExecutionRatingRequest
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}

	// The verdict set is closed here, at the Go constants, and by the table's
	// CHECK — three places, so a writer that skips any one still cannot
	// introduce a third value. Case-sensitive on purpose: accepting "UP" would
	// mean deciding whether to store it as typed or normalised, and a stored
	// value that differs from what a rollup groups by is a silent bucket split.
	if body.Verdict != persistence.VerdictUp && body.Verdict != persistence.VerdictDown {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			`verdict must be "up" or "down"`)
		return
	}

	reason := strings.TrimSpace(body.Reason)
	// REFUSED, not truncated: a rating that says half of what its author wrote
	// is worse than one that made them shorten it.
	if len(reason) > persistence.ExecutionRatingReasonMax {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"reason exceeds the 500-character limit; shorten it rather than let it be cut")
		return
	}

	rating := &persistence.ExecutionRating{
		ExecutionID: executionID,
		RaterID:     rater,
		Verdict:     body.Verdict,
		Reason:      reason,
	}
	if err := s.ratingRepo.Upsert(r.Context(), rating); err != nil {
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("rating: upsert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to store rating")
		return
	}

	// Read back so the response carries the authoritative timestamps —
	// created_at in particular, which the upsert preserves across an edit and
	// the caller has no way to compute.
	stored, err := s.ratingRepo.Get(r.Context(), executionID, rater)
	if err != nil {
		// The write landed; only the read-back failed. Answer with what we know
		// rather than reporting a failure that did not happen.
		respondJSON(w, http.StatusOK, ratingResponse(rating))
		return
	}
	respondJSON(w, http.StatusOK, ratingResponse(stored))
}

// ExecutionRatingGet handles GET /api/v1/executions/{id}/rating — the CALLER's
// own rating. 404 when they have not rated this run.
func (s *Server) ExecutionRatingGet(w http.ResponseWriter, r *http.Request, executionID string) {
	rater, ok := s.ratingPreflight(w, r, executionID)
	if !ok {
		return
	}
	stored, err := s.ratingRepo.Get(r.Context(), executionID, rater)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "You have not rated this execution")
			return
		}
		s.logger.Error().Err(err).Str("executionId", executionID).Msg("rating: get failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to read rating")
		return
	}
	respondJSON(w, http.StatusOK, ratingResponse(stored))
}

// ExecutionRatingDelete handles DELETE /api/v1/executions/{id}/rating —
// withdraws the CALLER's rating. Deleting one that is not there is 204: the
// caller's intent is "my rating should not exist", and it does not.
func (s *Server) ExecutionRatingDelete(w http.ResponseWriter, r *http.Request, executionID string) {
	rater, ok := s.ratingPreflight(w, r, executionID)
	if !ok {
		return
	}
	if err := s.ratingRepo.Delete(r.Context(), executionID, rater); err != nil {
		s.logger.Error().Err(err).Str("executionId", executionID).Msg("rating: delete failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to delete rating")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
