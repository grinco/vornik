package api

import (
	"net/http"
	"strings"
)

// FixItConverseRequest is the operator-supplied shape on POST
// /api/v1/fixit/converse. session_id is empty on the first turn of a
// new repair session — failure_kind/failure_ref_id[/project_id] bind
// it to the failing object. On subsequent turns only session_id +
// message matter: failure_kind/failure_ref_id/project_id, even if
// present, are IGNORED by the implementation (see FixItDoctor.Converse
// doc comment) — a resumed session's ref comes from the session row,
// never from the request body.
type FixItConverseRequest struct {
	SessionID    string `json:"session_id,omitempty"`
	FailureKind  string `json:"failure_kind,omitempty"`
	FailureRefID string `json:"failure_ref_id,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	Message      string `json:"message"`
}

// FixItConverse handles the Fix-It Doctor repair chat. Each call is
// one turn. Mirrors Server.ProjectWizardConverse's shape (task 3.2,
// fix-it-doctor-design.md §5.2).
//
// Scope gate: for a resumed session, the session's OWN project_id
// (via FixItDoctor.SessionScope) is checked against
// RequestAllowsProject — never the request body's project_id, so a
// caller can't widen scope by pairing a valid session_id with a
// different project_id. An out-of-scope project resolves to 404
// (NOT_FOUND), never 403 — same "don't leak existence" convention
// AssistantSuggest and the project wizard use.
//
// Refusals:
//   - doctor unwired → 503 FIXIT_DISABLED
//   - missing message / missing ref on a new session → 400 VALIDATION_ERROR
//   - out-of-scope project → 404 NOT_FOUND
//   - unknown/foreign session → 404 NOT_FOUND
//   - closed session → 410 GONE
//   - session cap hit → 429 TOO_MANY_SESSIONS
//   - turn cap hit → 429 TURN_LIMIT
//   - budget exceeded → 429 BUDGET_EXCEEDED
//   - LLM / persistence error → 502/500 with detail
func (s *Server) FixItConverse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.fixItDoctor == nil {
		respondError(w, http.StatusServiceUnavailable, "FIXIT_DISABLED",
			"fix-it doctor not wired on this deployment")
		return
	}

	var body FixItConverseRequest
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "message is required")
		return
	}

	operatorID := RequestOperatorIDOrSingleTenant(r, SingleTenantOperatorIDFromConfig(s.config))
	if operatorID == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED",
			"operator identity required (provide an API key or admin Telegram session)")
		return
	}

	sessionID := strings.TrimSpace(body.SessionID)
	projectID, ok := s.resolveFixItScope(w, r, sessionID, operatorID, &body)
	if !ok {
		return
	}
	if projectID != "" && !RequestAllowsProject(r, projectID) {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
		return
	}

	result, err := s.fixItDoctor.Converse(r.Context(), sessionID, operatorID, body.FailureKind, body.FailureRefID, projectID, body.Message)
	if err != nil {
		respondFixItConverseError(w, s, sessionID, err)
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// resolveFixItScope resolves the project the request must be scoped
// to before Converse runs: for a resumed session, the session's OWN
// project_id (via SessionScope) — never the request body's, so a
// caller can't widen scope by pairing a valid session_id with a
// different project_id; for a new session, the body's failure_kind/
// failure_ref_id (required) plus its project_id. Writes an error
// response and returns ok=false when the request should stop here.
func (s *Server) resolveFixItScope(w http.ResponseWriter, r *http.Request, sessionID, operatorID string, body *FixItConverseRequest) (projectID string, ok bool) {
	if sessionID != "" {
		scopedProject, found, err := s.fixItDoctor.SessionScope(r.Context(), sessionID, operatorID)
		if err != nil {
			s.logger.Error().Err(err).Str("session_id", sessionID).Msg("fixit session-scope lookup failed")
			respondError(w, http.StatusBadGateway, "FIXIT_ERROR", "session lookup failed; see server logs")
			return "", false
		}
		if !found {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
			return "", false
		}
		return scopedProject, true
	}
	body.FailureKind = strings.TrimSpace(body.FailureKind)
	body.FailureRefID = strings.TrimSpace(body.FailureRefID)
	if body.FailureKind == "" || body.FailureRefID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"failure_kind and failure_ref_id are required to start a new session")
		return "", false
	}
	return strings.TrimSpace(body.ProjectID), true
}

// respondFixItConverseError maps a fixitdoctor.Service.Converse error
// to the HTTP status + code the operator/client sees.
func respondFixItConverseError(w http.ResponseWriter, s *Server, sessionID string, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already closed"):
		respondError(w, http.StatusGone, "SESSION_CLOSED", "session already closed; start a new session")
	case strings.Contains(msg, "too many active sessions"):
		respondError(w, http.StatusTooManyRequests, "TOO_MANY_SESSIONS",
			"too many open repair sessions; close one before starting another")
	case strings.Contains(msg, "turn limit reached"):
		respondError(w, http.StatusTooManyRequests, "TURN_LIMIT",
			"repair session reached its turn cap; start a new session")
	case strings.Contains(msg, "budget exceeded"):
		respondError(w, http.StatusTooManyRequests, "BUDGET_EXCEEDED", msg)
	case strings.Contains(msg, "empty user message"),
		strings.Contains(msg, "operator id required"),
		strings.Contains(msg, "failure ref required"):
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", msg)
	case strings.Contains(msg, "not found"):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
	default:
		s.logger.Error().Err(err).Str("session_id", sessionID).Msg("fix-it doctor converse failed")
		respondError(w, http.StatusBadGateway, "FIXIT_ERROR", "repair chat turn failed; see server logs")
	}
}
