package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// tradingOrderStreamRequest is the wire shape for
// POST /api/v1/internal/trading-orders. The broker MCP's
// AuditWriter posts one per place_order / place_bracket_order
// call (success or refused) so the daemon has an independent
// audit trail of every broker-side decision.
//
// IdempotencyKey is the broker-generated per-call key. The
// daemon-side handler upserts on (project_id, idempotency_key);
// the broker's retry loop can post the same row repeatedly
// under transient daemon outages and the row lands once.
//
// All numeric fields use string-encoded floats to preserve the
// Decimal precision the broker uses internally — the daemon
// parses them via the json.Number / float64 conversion so the
// schema stays loose to evolution.
type tradingOrderStreamRequest struct {
	ID               string  `json:"id"`
	ProjectID        string  `json:"project_id"`
	TaskID           string  `json:"task_id,omitempty"`
	ExecutionID      string  `json:"execution_id,omitempty"`
	BrokerOrderID    string  `json:"broker_order_id,omitempty"`
	IdempotencyKey   string  `json:"idempotency_key"`
	Mode             string  `json:"mode"`
	Symbol           string  `json:"symbol"`
	Action           string  `json:"action"`
	OrderType        string  `json:"order_type"`
	Qty              float64 `json:"qty"`
	LimitPrice       float64 `json:"limit_price,omitempty"`
	StopPrice        float64 `json:"stop_price,omitempty"`
	TimeInForce      string  `json:"time_in_force"`
	Status           string  `json:"status"`
	LastStatusReason string  `json:"last_status_reason,omitempty"`
	SubmittedAt      string  `json:"submitted_at,omitempty"` // RFC3339
	TerminalAt       string  `json:"terminal_at,omitempty"`  // RFC3339
}

// IngestTradingOrder handles POST /api/v1/internal/trading-orders.
// Body shape: tradingOrderStreamRequest. Idempotent on
// (project_id, idempotency_key).
//
// Returns 204 No Content on success. The handler is fire-
// forget from the broker's perspective: a 4xx/5xx response
// signals "save this in your journal and retry"; a 2xx
// signals "row landed in DB, drop from journal".
//
// "Internal" path: the broker uses the same VORNIK_API_KEY
// scheme as agent containers. AuthMiddleware requires the
// key on every call. Per-project scoping is enforced by the
// requestAllowsProject helper — a key scoped to project A
// cannot ingest orders for project B even if the wire
// payload claims project B.
func (s *Server) IngestTradingOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tradingOrderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRADING_AUDIT_NOT_CONFIGURED",
			"trading order repo not wired; this should not happen in a production deployment")
		return
	}

	body, err := readLimitedBody(w, r, 64*1024)
	if err != nil {
		var tooLarge bodyTooLargeError
		if errors.As(err, &tooLarge) {
			s.recordTradingIngestError("order", "body_too_large")
			respondError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", err.Error())
			return
		}
		s.recordTradingIngestError("order", "read_failed")
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	// HMAC request-authentication (fail-closed when wired). Runs on
	// the exact body bytes the broker signed.
	if s.verifyTradingHMAC(w, r, body, "order") {
		return
	}

	var req tradingOrderStreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.recordTradingIngestError("order", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if req.ID == "" || req.ProjectID == "" || req.IdempotencyKey == "" || req.Symbol == "" || req.Status == "" {
		s.recordTradingIngestError("order", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"id, project_id, idempotency_key, symbol, and status are required")
		return
	}
	if req.Qty <= 0 || math.IsNaN(req.Qty) || math.IsInf(req.Qty, 0) {
		s.recordTradingIngestError("order", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "qty must be a positive finite number")
		return
	}
	if req.LimitPrice < 0 || math.IsNaN(req.LimitPrice) || math.IsInf(req.LimitPrice, 0) {
		s.recordTradingIngestError("order", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "limit_price must be a finite non-negative number")
		return
	}
	if req.StopPrice < 0 || math.IsNaN(req.StopPrice) || math.IsInf(req.StopPrice, 0) {
		s.recordTradingIngestError("order", "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "stop_price must be a finite non-negative number")
		return
	}

	if !requestAllowsProject(r, req.ProjectID) {
		s.recordTradingIngestError("order", "forbidden")
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return
	}

	// Per-project trading-order rate limit. Checked here (after
	// scope) so a 429 only burns on orders this caller may actually
	// place; the slot is Recorded only after a successful persist.
	if s.enforceTradingRateLimit(w, req.ProjectID) {
		return
	}

	// Pointer fields default to nil when the JSON didn't carry
	// them. tradingOrderStreamRequest uses scalar zero values
	// for omitempty round-tripping; we lift to pointers here so
	// the persistence layer can write SQL NULL cleanly.
	order := &persistence.TradingOrder{
		ID:               req.ID,
		ProjectID:        req.ProjectID,
		IdempotencyKey:   req.IdempotencyKey,
		Mode:             req.Mode,
		Symbol:           req.Symbol,
		Action:           req.Action,
		OrderType:        req.OrderType,
		Qty:              req.Qty,
		TimeInForce:      req.TimeInForce,
		Status:           req.Status,
		LastStatusReason: req.LastStatusReason,
	}
	if req.TaskID != "" {
		order.TaskID = &req.TaskID
	}
	if req.ExecutionID != "" {
		order.ExecutionID = &req.ExecutionID
	}
	if req.BrokerOrderID != "" {
		order.BrokerOrderID = &req.BrokerOrderID
	}
	if req.LimitPrice != 0 {
		v := req.LimitPrice
		order.LimitPrice = &v
	}
	if req.StopPrice != 0 {
		v := req.StopPrice
		order.StopPrice = &v
	}
	if req.SubmittedAt != "" {
		if t, err := time.Parse(time.RFC3339, req.SubmittedAt); err == nil {
			order.SubmittedAt = t
		}
	}
	if order.SubmittedAt.IsZero() {
		order.SubmittedAt = time.Now().UTC()
	}
	if req.TerminalAt != "" {
		if t, err := time.Parse(time.RFC3339, req.TerminalAt); err == nil {
			order.TerminalAt = &t
		}
	}

	if err := s.tradingOrderRepo.Record(r.Context(), order); err != nil {
		s.logger.Warn().
			Err(err).
			Str("order_id", req.ID).
			Str("project_id", req.ProjectID).
			Str("idempotency_key", req.IdempotencyKey).
			Msg("trading order ingest: persist failed")
		s.recordTradingIngestError("order", "persist_failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	if s.tradingMetrics != nil {
		// Bucket the broker's status into "placed" vs "refused" so
		// the dashboard can graph one ratio rather than every
		// fine-grained status. "refused" covers any envelope
		// rejection (cap_refused / kill_switch / breaker); anything
		// else is treated as a successful placement (working / filled /
		// cancelled — the order at least passed the envelope).
		status := "placed"
		if req.Status == "refused" || req.Status == "rejected" {
			status = "refused"
		}
		s.tradingMetrics.OrdersIngestedTotal.WithLabelValues(req.ProjectID, status).Inc()
	}

	// Count this accepted order toward the project's trading window.
	s.recordTradingOrderRate(req.ProjectID)

	w.WriteHeader(http.StatusNoContent)
}
