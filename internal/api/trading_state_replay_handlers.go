package api

import (
	"encoding/json"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stateReplayFill is the wire shape for fills the broker uses to
// rebuild dayTurnover after a process restart. Only the fields
// needed for `qty * price` aggregation are included — id /
// commission / order_id are irrelevant for the safety envelope's
// in-memory bookkeeping.
type stateReplayFill struct {
	Symbol   string  `json:"symbol"`
	Qty      float64 `json:"qty"`
	Price    float64 `json:"price"`
	FilledAt string  `json:"filled_at"` // RFC3339 UTC
}

// stateReplayOrder is the wire shape for orders the broker uses
// to rebuild the orderTimes sliding window. Only entries within
// the rate-limit horizon (last hour) are needed; older rows are
// filtered server-side so the broker doesn't pay for filtering it
// would just discard.
type stateReplayOrder struct {
	ClientTag   string  `json:"client_tag"` // = TradingOrder.ID (deterministic per place)
	Symbol      string  `json:"symbol"`
	Action      string  `json:"action"`
	Status      string  `json:"status"`
	SubmittedAt string  `json:"submitted_at"` // RFC3339 UTC
	LimitPrice  float64 `json:"limit_price,omitempty"`
}

// stateReplayResponse is what GET /api/v1/internal/trading-state-replay
// returns. Single response, no pagination — the volume of a single
// project's UTC-day's worth of fills + last-hour orders is bounded
// (typical strategist ticks every 5 min × 16 symbols → <200 rows
// even on a busy day).
type stateReplayResponse struct {
	TodayUTC string             `json:"today_utc"` // YYYY-MM-DD
	NowUTC   string             `json:"now_utc"`   // RFC3339 — server clock; broker uses it to pick the correct dayTurnover key
	Fills    []stateReplayFill  `json:"fills"`
	Orders   []stateReplayOrder `json:"orders"`
	// Audit T5: breaker-state recovery. The broker restores these at
	// boot so a restart does not launder a drawdown (zero HWM /
	// re-baseline to post-loss equity). Zero when no snapshot history.
	HWMUSD                float64 `json:"hwm_usd"`
	SessionStartEquityUSD float64 `json:"session_start_equity_usd"`
	SessionStartDate      string  `json:"session_start_date"` // YYYY-MM-DD UTC
	// Audit T6: still-working (non-terminal) orders today, so the
	// broker can rebuild the deterministic-tag double-submit guard
	// and a same-key retry after restart replays instead of placing
	// a duplicate real order.
	WorkingOrders []stateReplayWorkingOrder `json:"working_orders"`
	// Task 11: max(filled_at) across all of today's fills, used by
	// the broker to seed SafetyEnvelope.execCursor so
	// ReconcileExecutions resumes from the right point after a
	// restart rather than from zero (full re-scan). RFC3339 UTC;
	// empty when no fills have been persisted for this project.
	MaxFilledAt string `json:"max_filled_at,omitempty"` // RFC3339 UTC; empty when no fills
}

// stateReplayWorkingOrder carries what the broker's double-submit
// guard needs to re-track a still-working order across a restart
// (audit T6). The idempotency_key is the join the guard matches a
// same-key retry against.
type stateReplayWorkingOrder struct {
	ClientTag      string  `json:"client_tag"` // = TradingOrder.ID
	IdempotencyKey string  `json:"idempotency_key"`
	Symbol         string  `json:"symbol"`
	Action         string  `json:"action"`
	OrderType      string  `json:"order_type"`
	Qty            float64 `json:"qty"`
	LimitPrice     float64 `json:"limit_price,omitempty"`
	StopPrice      float64 `json:"stop_price,omitempty"`
	Status         string  `json:"status"`
}

// terminalTradingStatuses are order states that are DONE — excluded
// from the working-orders replay (audit T6) since a retry against a
// terminal order should not replay a stale result.
var terminalTradingStatuses = map[string]struct{}{
	"filled": {}, "cancelled": {}, "rejected": {},
}

// GetTradingStateReplay serves GET /api/v1/internal/trading-state-replay.
// The broker MCP calls this on startup so its in-memory dayTurnover /
// orderTimes / dayOpens reflect the state the daemon already persisted —
// without this, every broker-mcp recreate (env-var change, image rebuild,
// crash recovery) wipes today's accumulated turnover and rate-limit
// windows, making MaxDailyTurnoverUSD / MaxOrdersPerHour effectively
// off for the rest of the UTC day. Observed 2026-05-12 after three
// broker recreates wiped the day's two real fills ($1,753 turnover)
// from /caps' today_turnover_by_project.
//
// Scope: today's UTC fills + last-hour orders for ONE project (the
// caller's API-key-bound scope). Cross-project access returns 403,
// matching the ingest endpoints' contract.
func (s *Server) GetTradingStateReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tradingFillRepo == nil || s.tradingOrderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRADING_REPOS_NOT_CONFIGURED",
			"trading repos not wired; this should not happen in a production deployment")
		return
	}

	// HMAC request-authentication. The GET carries no body, so the
	// broker signs (and we verify) over an empty payload.
	if s.verifyTradingHMAC(w, r, nil, "state_replay") {
		return
	}

	projectID := r.URL.Query().Get("project")
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project query parameter is required")
		return
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return
	}

	now := time.Now().UTC()
	// Day bucket: midnight UTC. Matches the dayTurnover key
	// boundary on the broker side so a fill at 23:59:59Z lands
	// in the same bucket the broker expects, even when the
	// broker call happens seconds after the rollover.
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-1 * time.Hour)

	resp := stateReplayResponse{
		TodayUTC:         midnight.Format("2006-01-02"),
		NowUTC:           now.Format(time.RFC3339),
		Fills:            []stateReplayFill{},
		Orders:           []stateReplayOrder{},
		WorkingOrders:    []stateReplayWorkingOrder{},
		SessionStartDate: midnight.Format("2006-01-02"),
	}

	pid := projectID

	// Build the set of orders THIS project recorded since midnight UTC.
	// A fill is only allowed to feed the live dayTurnover replay if its
	// order_id maps to an order the daemon recorded for THIS project on
	// the same UTC day. This is a security control, not bookkeeping: the
	// trading_fills.order_id FK only enforces that the order EXISTS, not
	// that it belongs to the same project, so a forged/cross-project fill
	// would otherwise be replayed straight into this project's
	// daily-turnover cap + rate-limit window. We fail CLOSED — a fill with
	// no matching same-project order is dropped, not trusted. Valid
	// daemon-recorded fills are unaffected (the broker always records the
	// order before its fills).
	validOrders, err := s.tradingOrderRepo.List(r.Context(), persistence.TradingOrderFilter{
		ProjectID: &pid,
		Since:     &midnight,
		PageSize:  1000,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("trading state replay: list validation orders failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	validOrderIDs := make(map[string]struct{}, len(validOrders))
	for _, o := range validOrders {
		if o == nil || o.ProjectID != projectID {
			continue
		}
		validOrderIDs[o.ID] = struct{}{}
		// Audit T6: a still-working order today is re-tracked by the
		// broker's double-submit guard so a same-key retry after a
		// restart replays instead of placing a duplicate real order.
		if _, terminal := terminalTradingStatuses[o.Status]; terminal {
			continue
		}
		wo := stateReplayWorkingOrder{
			ClientTag:      o.ID,
			IdempotencyKey: o.IdempotencyKey,
			Symbol:         o.Symbol,
			Action:         o.Action,
			OrderType:      o.OrderType,
			Qty:            o.Qty,
			Status:         o.Status,
		}
		if o.LimitPrice != nil {
			wo.LimitPrice = *o.LimitPrice
		}
		if o.StopPrice != nil {
			wo.StopPrice = *o.StopPrice
		}
		resp.WorkingOrders = append(resp.WorkingOrders, wo)
	}

	// Task 11: seed the broker's exec-cursor (ReconcileExecutions
	// resumes from max(filled_at) rather than zero). A zero return
	// (no fills yet today) is handled gracefully — the field is
	// omitted from the response and the broker starts from zero.
	if s.tradingFillRepo != nil {
		if mfa, mfaErr := s.tradingFillRepo.MaxFilledAt(r.Context(), projectID); mfaErr == nil && !mfa.IsZero() {
			resp.MaxFilledAt = mfa.UTC().Format(time.RFC3339)
		}
	}

	// Audit T5: derive the breaker's HWM + UTC-day baseline from the
	// equity snapshots the daemon's sampler persisted, so the broker
	// can restore them across a restart instead of zeroing the HWM
	// and re-baselining to the current (post-loss) equity. HWM = the
	// peak equity seen today; baseline = the earliest snapshot today.
	if s.tradingPositionsRepo != nil {
		snaps, serr := s.tradingPositionsRepo.ListSince(r.Context(), projectID, midnight, 1000)
		if serr != nil {
			s.logger.Warn().Err(serr).Str("project", projectID).
				Msg("trading state replay: positions-snapshot fetch failed; breaker state not recovered")
		} else {
			// snaps are oldest-first (repo contract), so the first
			// usable row is today's opening baseline and the max is
			// the HWM.
			for _, sn := range snaps {
				if sn == nil || sn.EquityUSD <= 0 {
					continue
				}
				if sn.EquityUSD > resp.HWMUSD {
					resp.HWMUSD = sn.EquityUSD
				}
				if resp.SessionStartEquityUSD == 0 {
					resp.SessionStartEquityUSD = sn.EquityUSD
				}
			}
		}
	}

	// Fills since midnight UTC — feeds dayTurnover.
	fillFilter := persistence.TradingFillFilter{
		ProjectID: &pid,
		Since:     &midnight,
		PageSize:  1000, // wide enough that we never silently truncate a real day's flow
	}
	fills, err := s.tradingFillRepo.List(r.Context(), fillFilter)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("trading state replay: list fills failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	for _, f := range fills {
		if f == nil {
			continue
		}
		// Fail closed: a fill must (a) be scoped to the caller's project
		// and (b) reference an order this project recorded today. Orphan /
		// cross-project fills are dropped so a forged row cannot move the
		// daily-turnover cap or rate-limit window.
		if f.ProjectID != projectID {
			s.logger.Warn().Str("project", projectID).Str("fill_project", f.ProjectID).
				Str("fill_id", f.ID).
				Msg("trading state replay: dropping fill not scoped to caller's project")
			continue
		}
		if _, ok := validOrderIDs[f.OrderID]; !ok {
			s.logger.Warn().Str("project", projectID).Str("fill_id", f.ID).
				Str("order_id", f.OrderID).
				Msg("trading state replay: dropping fill with no matching same-project order")
			continue
		}
		resp.Fills = append(resp.Fills, stateReplayFill{
			Symbol:   f.Symbol,
			Qty:      f.Qty,
			Price:    f.Price,
			FilledAt: f.FilledAt.UTC().Format(time.RFC3339),
		})
	}

	// Orders in the last hour — feeds orderTimes sliding window
	// for MaxOrdersPerHour / PerMinute caps. We deliberately
	// include every status (filled, cancelled, submitted) here:
	// the rate limiter consumes a slot at PLACE time regardless
	// of terminal outcome, so cancelled orders still count.
	orderFilter := persistence.TradingOrderFilter{
		ProjectID: &pid,
		Since:     &hourAgo,
		PageSize:  1000,
	}
	orders, err := s.tradingOrderRepo.List(r.Context(), orderFilter)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("trading state replay: list orders failed")
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	for _, o := range orders {
		if o == nil {
			continue
		}
		lp := 0.0
		if o.LimitPrice != nil {
			lp = *o.LimitPrice
		}
		resp.Orders = append(resp.Orders, stateReplayOrder{
			ClientTag:   o.ID,
			Symbol:      o.Symbol,
			Action:      o.Action,
			Status:      o.Status,
			SubmittedAt: o.SubmittedAt.UTC().Format(time.RFC3339),
			LimitPrice:  lp,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn().Err(err).Msg("trading state replay: encode response failed")
	}
}
