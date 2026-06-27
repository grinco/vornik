package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// tradingSafetyEventStreamRequest is the wire shape for
// POST /api/v1/internal/trading-safety-events. Same writer
// machinery in the broker emits these alongside trading
// orders — every refusal, kill-switch toggle, breaker trip,
// and idempotency replay hit shows up here.
//
// detail is a free-form JSON object that the writer marshals
// into the JSONB column verbatim. Each kind has its own
// expected shape (e.g. cap_refused carries cap_kind +
// attempted_value); the schema is intentionally loose so new
// event kinds don't require a wire-shape update.
type tradingSafetyEventStreamRequest struct {
	ID         string          `json:"id"`
	ProjectID  string          `json:"project_id"`
	Kind       string          `json:"kind"`
	Severity   string          `json:"severity,omitempty"`
	Symbol     string          `json:"symbol,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	RecordedAt string          `json:"recorded_at,omitempty"` // RFC3339
}

// IngestTradingSafetyEvent handles POST /api/v1/internal/
// trading-safety-events. Idempotent on id (PRIMARY KEY).
//
// Returns 204 No Content on success. The handler is fire-
// forget from the broker's perspective: a 4xx/5xx response
// signals "save this in your journal and retry"; a 2xx
// signals "row landed in DB, drop from journal".
//
// Same trust boundary + scoping as the trading-orders endpoint.
func (s *Server) IngestTradingSafetyEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tradingSafetyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRADING_SAFETY_NOT_CONFIGURED",
			"trading safety event repo not wired; this should not happen in a production deployment")
		return
	}

	body, err := readLimitedBody(w, r, 64*1024)
	if err != nil {
		var tooLarge bodyTooLargeError
		if errors.As(err, &tooLarge) {
			s.recordTradingIngestError("safety_event", "body_too_large")
			respondError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", err.Error())
			return
		}
		s.recordTradingIngestError("safety_event", "read_failed")
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	if s.verifyTradingHMAC(w, r, body, "safety_event") {
		return
	}

	var req tradingSafetyEventStreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.recordTradingIngestError("safety_event", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if req.ID == "" || req.ProjectID == "" || req.Kind == "" {
		s.recordTradingIngestError("safety_event", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"id, project_id, and kind are required")
		return
	}

	if !requestAllowsProject(r, req.ProjectID) {
		s.recordTradingIngestError("safety_event", "forbidden")
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return
	}

	event := &persistence.TradingSafetyEvent{
		ID:        req.ID,
		ProjectID: req.ProjectID,
		Kind:      req.Kind,
		Severity:  req.Severity,
	}
	if req.Symbol != "" {
		event.Symbol = &req.Symbol
	}
	if len(req.Detail) > 0 {
		event.Detail = req.Detail
	}
	if req.RecordedAt != "" {
		if t, err := time.Parse(time.RFC3339, req.RecordedAt); err == nil {
			event.RecordedAt = t
		}
	}
	if event.RecordedAt.IsZero() {
		event.RecordedAt = time.Now().UTC()
	}

	if err := s.tradingSafetyRepo.Record(r.Context(), event); err != nil {
		s.logger.Warn().
			Err(err).
			Str("event_id", req.ID).
			Str("project_id", req.ProjectID).
			Str("kind", req.Kind).
			Msg("trading safety event ingest: persist failed")
		s.recordTradingIngestError("safety_event", "persist_failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	if s.tradingMetrics != nil {
		severity := req.Severity
		if severity == "" {
			severity = "unknown"
		}
		s.tradingMetrics.SafetyEventsTotal.WithLabelValues(req.ProjectID, req.Kind, severity).Inc()
	}

	w.WriteHeader(http.StatusNoContent)
}

// recordTradingIngestError bumps the IngestErrorsTotal counter
// guarded by a nil check on tradingMetrics so the call sites stay
// terse.
func (s *Server) recordTradingIngestError(endpoint, reason string) {
	if s == nil || s.tradingMetrics == nil {
		return
	}
	s.tradingMetrics.IngestErrorsTotal.WithLabelValues(endpoint, reason).Inc()
}
