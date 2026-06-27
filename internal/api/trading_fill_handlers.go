package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// tradingFillStreamRequest is the wire shape for
// POST /api/v1/internal/trading-fills. The broker MCP's poll
// loop posts one row per fill it observes (partial or full)
// against an order in its placedOrders state map.
//
// IDs are deterministic per (order_id, filled_at) so retries
// under daemon outage collide on PRIMARY KEY.
//
// All numeric fields are float64 to match the broker's wire
// shape; the persistence layer rounds to NUMERIC(18,6) on
// insert.
type tradingFillStreamRequest struct {
	ID            string  `json:"id"`
	OrderID       string  `json:"order_id"`
	ProjectID     string  `json:"project_id"`
	Symbol        string  `json:"symbol"`
	Qty           float64 `json:"qty"`
	Price         float64 `json:"price"`
	CommissionUSD float64 `json:"commission_usd,omitempty"`
	FilledAt      string  `json:"filled_at,omitempty"` // RFC3339
	// Exec-keyed provenance, posted by the broker's ReconcileExecutions
	// loop (the placedOrders-independent booking path). ExecID is the
	// broker's stable per-fill id (the row's natural key is "exec-"+ExecID);
	// Source distinguishes a tracked-order reconcile ("reconcile") from a
	// fill booked for an order absent from the in-memory map
	// ("broker_reconcile", e.g. a GTC stop placed before a broker restart —
	// the 2026-06-25 AAPL incident). All optional: the legacy
	// placedOrders-keyed emitter (emitFillAudit) omits them.
	ExecID       string `json:"exec_id,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	Source       string `json:"source,omitempty"`
	SourceDetail string `json:"source_detail,omitempty"`
}

// fillFromRequest maps a validated tradingFillStreamRequest onto a
// persistence.TradingFill, setting optional pointer fields and defaulting
// FilledAt to now when absent or unparseable.
func fillFromRequest(req tradingFillStreamRequest) *persistence.TradingFill {
	fill := &persistence.TradingFill{
		ID:        req.ID,
		OrderID:   req.OrderID,
		ProjectID: req.ProjectID,
		Symbol:    req.Symbol,
		Qty:       req.Qty,
		Price:     req.Price,
	}
	if req.CommissionUSD != 0 {
		v := req.CommissionUSD
		fill.CommissionUSD = &v
	}
	// Exec-keyed provenance (broker ReconcileExecutions path). Optional:
	// empty/absent values leave the columns NULL (Source defaults to
	// "reconcile" at the repo layer).
	if req.ExecID != "" {
		v := req.ExecID
		fill.ExecID = &v
	}
	if req.AccountID != "" {
		v := req.AccountID
		fill.AccountID = &v
	}
	fill.Source = req.Source
	if req.SourceDetail != "" {
		v := req.SourceDetail
		fill.SourceDetail = &v
	}
	if req.FilledAt != "" {
		if t, err := time.Parse(time.RFC3339, req.FilledAt); err == nil {
			fill.FilledAt = t
		}
	}
	if fill.FilledAt.IsZero() {
		fill.FilledAt = time.Now().UTC()
	}
	return fill
}

// readAndParseFillRequest reads the body, verifies the HMAC, decodes and
// validates the JSON, and builds the persistence.TradingFill. label is the
// metric/log label ("fill" or "fill_shadow"). Returns (nil, true) when the
// handler has already written a response and the caller must return; returns
// (fill, false) on success.
func (s *Server) readAndParseFillRequest(w http.ResponseWriter, r *http.Request, label string) (*persistence.TradingFill, bool) {
	body, err := readLimitedBody(w, r, 64*1024)
	if err != nil {
		var tooLarge bodyTooLargeError
		if errors.As(err, &tooLarge) {
			s.recordTradingIngestError(label, "body_too_large")
			respondError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", err.Error())
			return nil, true
		}
		s.recordTradingIngestError(label, "read_failed")
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return nil, true
	}
	defer func() { _ = r.Body.Close() }()

	if s.verifyTradingHMAC(w, r, body, label) {
		return nil, true
	}

	var req tradingFillStreamRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.recordTradingIngestError(label, "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return nil, true
	}
	if req.ID == "" || req.OrderID == "" || req.ProjectID == "" || req.Symbol == "" {
		s.recordTradingIngestError(label, "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"id, order_id, project_id, and symbol are required")
		return nil, true
	}
	if req.Qty <= 0 || math.IsNaN(req.Qty) || math.IsInf(req.Qty, 0) {
		s.recordTradingIngestError(label, "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "qty must be a positive finite number")
		return nil, true
	}
	if req.Price < 0 || math.IsNaN(req.Price) || math.IsInf(req.Price, 0) {
		s.recordTradingIngestError(label, "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "price must be a finite non-negative number")
		return nil, true
	}
	if math.IsNaN(req.CommissionUSD) || math.IsInf(req.CommissionUSD, 0) {
		s.recordTradingIngestError(label, "validation")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "commission_usd must be finite")
		return nil, true
	}
	if !requestAllowsProject(r, req.ProjectID) {
		s.recordTradingIngestError(label, "forbidden")
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return nil, true
	}

	return fillFromRequest(req), false
}

// IngestTradingFill handles POST /api/v1/internal/trading-fills.
// Idempotent on id. Returns 204 on success, 4xx for validation
// failures so the broker's writer can decide retry vs drop.
func (s *Server) IngestTradingFill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tradingFillRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRADING_FILLS_NOT_CONFIGURED",
			"trading fill repo not wired; this should not happen in a production deployment")
		return
	}

	fill, handled := s.readAndParseFillRequest(w, r, "fill")
	if handled {
		return
	}

	if err := s.tradingFillRepo.Record(r.Context(), fill); err != nil {
		s.logger.Warn().
			Err(err).
			Str("fill_id", fill.ID).
			Str("order_id", fill.OrderID).
			Str("project_id", fill.ProjectID).
			Msg("trading fill ingest: persist failed")
		s.recordTradingIngestError("fill", "persist_failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	if s.tradingMetrics != nil {
		s.tradingMetrics.FillsIngestedTotal.WithLabelValues(fill.ProjectID).Inc()
	}

	// Best-effort fill notification. The bot's debouncer collapses
	// partial-fill bursts on the same order into one message; here
	// we just hand off the fill and let the notifier own timing.
	// Pass a detached context — the handler returns 204 immediately
	// and the notifier's own goroutine drives the timer.
	if s.fillNotifier != nil {
		s.fillNotifier.NotifyFill(r.Context(), fill)
	}

	w.WriteHeader(http.StatusNoContent)
}

// IngestTradingFillShadow handles POST /api/v1/internal/trading-fills-shadow.
// It is the shadow-mode mirror of IngestTradingFill: it accepts the same wire
// shape and enforces the same validation, but calls RecordShadow instead of
// Record so fills land in trading_fills_shadow for comparison rather than the
// live table. Idempotent on id (the shadow table also carries an id PRIMARY KEY).
func (s *Server) IngestTradingFillShadow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tradingFillRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRADING_FILLS_NOT_CONFIGURED",
			"trading fill repo not wired; this should not happen in a production deployment")
		return
	}

	fill, handled := s.readAndParseFillRequest(w, r, "fill_shadow")
	if handled {
		return
	}

	if err := s.tradingFillRepo.RecordShadow(r.Context(), fill); err != nil {
		s.logger.Warn().
			Err(err).
			Str("fill_id", fill.ID).
			Str("order_id", fill.OrderID).
			Str("project_id", fill.ProjectID).
			Msg("trading fill shadow ingest: persist failed")
		s.recordTradingIngestError("fill_shadow", "persist_failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
