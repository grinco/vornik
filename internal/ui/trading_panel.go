package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/tradingpnl"
)

// TradingPanel is the per-project caps + portfolio panel payload.
// Populated only when the project has an MCP server named "broker"
// — the UI uses that as the signal for "trading is enabled here".
//
// Every field can be empty: the daemon fetches /caps from the
// broker MCP with a 3s timeout, and a missing / unreachable
// broker just leaves Caps + Portfolio as zero values. The
// template renders an "offline" hint in that case rather than
// hiding the panel entirely — the operator's "is the trading
// stack up?" question is itself a useful answer.
type TradingPanel struct {
	// Enabled is true when the project has a broker MCP server
	// configured. The template uses this to decide whether to
	// show the panel at all.
	Enabled bool

	// BrokerReachable is true when the daemon successfully
	// fetched /caps from the broker MCP. False on timeout, dial
	// error, non-2xx response, or malformed JSON.
	BrokerReachable bool
	// BrokerError carries the failure reason when
	// BrokerReachable is false. Surfaced as an inline hint on
	// the panel header.
	BrokerError string

	// Configured fields mirror the broker MCP's /caps response.
	// Zero-value floats / ints mean "unlimited" for cap fields,
	// "off" for the breaker.
	Mode                      string
	MaxPositionUSD            float64
	MaxDailyTurnoverUSD       float64
	MaxOrdersPerHour          int
	MaxOrdersPerMinute        int
	DrawdownCircuitBreakerPct float64
	RequireStopLoss           bool
	DefaultStopLossPct        float64

	// State fields are derived from the safety envelope's
	// in-memory snapshot at the moment of the fetch. The
	// "headroom" fields are convenience derivations the
	// template uses to render progress bars without doing
	// arithmetic in the template.
	KillSwitch         bool
	HWMUSD             float64
	TodayUTC           string
	TodayTurnoverUSD   float64
	OrdersInLastHour   int
	OrdersInLastMinute int

	// Portfolio fields come from a fresh broker call. Populated
	// only when PortfolioReachable is true. The /caps endpoint
	// fetches them under a 3s timeout so a stuck IBKR connection
	// can't block the project page render.
	PortfolioReachable bool
	PortfolioError     string
	Account            string
	CashUSD            float64
	EquityUSD          float64
	BuyingPowerUSD     float64
	UnrealisedPLUSD    float64
	RealisedPLDayUSD   float64
	OpenPositions      int
	DrawdownPct        float64

	// FetchedAt records when the panel was populated. Surfaced
	// in the panel footer so an operator on a stale page sees
	// "as of 14:32 UTC" rather than silently trusting numbers
	// that may be minutes out of date.
	FetchedAt time.Time

	// Soak holds the snapshot-derived rollup (Sharpe, max
	// drawdown, equity 24h change). Populated when the daemon
	// has tradingSnapshotRepo wired AND has accumulated some
	// samples; until SoakReady=true the template hides the
	// headline numbers and shows a "soak warming up — N days
	// of data" hint instead.
	Soak SoakMetrics

	// SafetyEvents are the most recent broker-side safety
	// decisions (refusals, replay hits, kill toggles,
	// breaker trips) — the cross-component audit feed. Capped
	// at 20 rows for the inline tile; the operator opens the
	// project's safety-events page (future) for full history.
	SafetyEvents []SafetyEventRow
	// SafetyEvents24hCount is the total over the last 24h —
	// surfaced as a headline number above the list so the
	// operator sees "24 refusals today" without counting
	// rows manually.
	SafetyEvents24hCount int64

	// RecentOrders + RecentFills are the trading-history tables
	// rendered below the safety-events list. Capped at the
	// constants below so the inline view stays scannable; a
	// future /ui/projects/<id>/trading page can paginate.
	// Populated only when the respective repo is wired AND the
	// project has trading enabled.
	RecentOrders []TradingOrderRow
	RecentFills  []TradingFillRow

	// Perf is the realized round-trip P&L over the trailing
	// PerfWindowDays — the "are we winning?" headline the dedicated
	// /ui/trading page leads with, brought to the project-detail embed so
	// the panel doesn't open on broker caps alone. Populated only by the
	// project-detail path (buildTradingPanel); the Insight dashboard
	// computes its own windowed Perf into the page model, not the panel.
	Perf tradingpnl.Performance
	// PerfProfitFactor is the pre-formatted profit factor ("—" when
	// undefined, i.e. no losing trades in the window).
	PerfProfitFactor string
	// PerfWindowDays is the trailing window Perf was computed over. Zero
	// means Perf was not computed (e.g. the shared window-path caller).
	PerfWindowDays int
}

// recentOrdersLimit / recentFillsLimit cap the inline history
// tables. Chosen so the project page stays one-screen on a
// 13" laptop; bigger windows live in the dashboard or a
// dedicated trading page if/when that ships.
const (
	recentOrdersLimit = 20
	recentFillsLimit  = 20
)

// TradingOrderRow flattens a persistence.TradingOrder into the
// shape the template renders. Pre-resolved pointer fields and a
// status-pill class keep the template free of nil checks and
// switch statements.
type TradingOrderRow struct {
	ID            string
	BrokerOrderID string
	Symbol        string
	Action        string // BUY / SELL
	OrderType     string // MKT / LMT / STP / STP_LMT
	Qty           float64
	LimitPrice    float64 // 0 when not set (MKT)
	StopPrice     float64 // 0 when not set
	TimeInForce   string
	Status        string
	StatusReason  string
	SubmittedAt   time.Time
	TerminalAt    time.Time // zero when still open
	// StatusClass drives the row's status-pill colour. submitted
	// / partial → neutral, filled → success, cancelled / refused
	// / rejected → bad, anything else → warn so unknown surfaces.
	StatusClass string
	// ActionClass colours the action pill green for BUY, rose for
	// SELL so an operator scanning the table can spot direction
	// without reading the column.
	ActionClass string
}

// TradingFillRow flattens a persistence.TradingFill for the
// template — same shape rules as TradingOrderRow.
type TradingFillRow struct {
	ID            string
	OrderID       string
	Symbol        string
	Qty           float64
	Price         float64
	Notional      float64 // Qty × Price, precomputed for the row
	CommissionUSD float64 // 0 when not reported
	FilledAt      time.Time
}

// SafetyEventRow projects a TradingSafetyEvent for the
// template — pre-formatted timestamp + parsed detail map so
// the template doesn't decode JSON inline.
type SafetyEventRow struct {
	Kind       string
	Severity   string
	Symbol     string
	Detail     map[string]any
	RecordedAt time.Time
	// SeverityClass drives the row's pill colour. info →
	// neutral, warn → amber, critical → rose.
	SeverityClass string
}

// projectCapsHeader serialises the project's trading caps into
// the same JSON shape container.brokerHeadersFor uses — keeps
// the UI panel and the daemon's place_order path in lock step on
// what the broker sees as authoritative for this project.
func projectCapsHeader(project *registry.Project) string {
	if project == nil {
		return ""
	}
	caps := struct {
		MaxPositionUSD            float64 `json:"max_position_usd"`
		MaxDailyTurnoverUSD       float64 `json:"max_daily_turnover_usd"`
		MaxOrdersPerHour          int     `json:"max_orders_per_hour"`
		MaxOrdersPerMinute        int     `json:"max_orders_per_minute"`
		DrawdownCircuitBreakerPct float64 `json:"drawdown_circuit_breaker_pct"`
		KillSwitch                bool    `json:"kill_switch"`
	}{
		MaxPositionUSD:            project.Trading.Caps.MaxPositionUSD,
		MaxDailyTurnoverUSD:       project.Trading.Caps.MaxDailyTurnoverUSD,
		MaxOrdersPerHour:          project.Trading.Caps.MaxOrdersPerHour,
		MaxOrdersPerMinute:        project.Trading.Caps.MaxOrdersPerMinute,
		DrawdownCircuitBreakerPct: project.Trading.Caps.DrawdownCircuitBreakerPct,
		KillSwitch:                project.Trading.KillSwitch,
	}
	encoded, err := json.Marshal(caps)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// brokerURL returns the broker MCP base URL for this project,
// or "" when no broker server is configured. Trading panel is
// gated on this — see TradingPanel.Enabled.
func brokerURL(project *registry.Project) string {
	if project == nil {
		return ""
	}
	for _, srv := range project.MCP.Servers {
		if srv.Name == "broker" {
			return strings.TrimRight(srv.URL, "/")
		}
	}
	return ""
}

// buildTradingPanel populates a TradingPanel by hitting
// {brokerURL}/caps. Always returns a panel — Enabled=false when
// no broker MCP is configured, BrokerReachable=false when the
// fetch failed for any reason. Caller embeds the result in the
// project-detail template.
// tradingPanelPerfWindowDays is the trailing window the project-detail panel
// computes round-trip Performance over. 30 days matches the soak-rollup horizon
// and is long enough to be meaningful without dragging in stale history.
const tradingPanelPerfWindowDays = 30

func (s *Server) buildTradingPanel(ctx context.Context, project *registry.Project) TradingPanel {
	// Zero `since` preserves the project-detail panel's historical
	// behavior: the recent-orders/fills/safety lists are unbounded
	// (most-recent N of any age) and the safety headline counts the
	// fixed last-24h window. The Insight trading dashboard calls
	// buildTradingPanelWindow directly with a real window.
	panel := s.buildTradingPanelWindow(ctx, project, time.Time{})
	if !panel.Enabled {
		return panel
	}

	// Lead with realized round-trip P&L over a trailing window (mirrors the
	// dedicated /ui/trading page, which deliberately surfaces Performance
	// above broker caps). Reuses the dashboard's compute path; the SVG chart
	// geometry is discarded — the embed shows headline numbers only. Computed
	// here (not in the shared window-path) so the dashboard, which builds its
	// own windowed Perf into the page model, doesn't double-compute.
	now := time.Now().UTC()
	perf, _ := s.buildTradingPerformance(ctx, project.ID, now.AddDate(0, 0, -tradingPanelPerfWindowDays), now)
	panel.Perf = perf
	panel.PerfWindowDays = tradingPanelPerfWindowDays
	if perf.ProfitFactor != nil {
		panel.PerfProfitFactor = fmt.Sprintf("%.2f", *perf.ProfitFactor)
	} else {
		panel.PerfProfitFactor = "—"
	}
	return panel
}

// buildTradingPanelWindow is the window-aware core. `since` scopes the
// recent-orders, recent-fills, and safety-event lists plus the safety
// headline count to [since, now); a zero `since` leaves those lists
// unbounded and counts safety events over the fixed last-24h window
// (the legacy project-detail behavior). The account snapshot, caps, and
// 30d soak rollup are point-in-time / fixed-period and ignore `since`.
func (s *Server) buildTradingPanelWindow(ctx context.Context, project *registry.Project, since time.Time) TradingPanel {
	url := brokerURL(project)
	if url == "" {
		return TradingPanel{Enabled: false}
	}
	panel := TradingPanel{Enabled: true, FetchedAt: time.Now().UTC()}

	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	target := fmt.Sprintf("%s/caps?project=%s", url, project.ID)
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, target, nil)
	if err != nil {
		panel.BrokerError = fmt.Sprintf("build request: %v", err)
		return panel
	}
	// Echo the per-project cap overlay on /caps so the broker
	// returns the operator's YAML caps in `configured` rather
	// than its own env-var defaults. Same header the daemon's
	// MCP client attaches to place_order — the UI is just
	// reading what the broker would actually enforce.
	if hdr := projectCapsHeader(project); hdr != "" {
		req.Header.Set("X-Project-Caps", hdr)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panel.BrokerError = fmt.Sprintf("dial: %v", err)
		return panel
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		panel.BrokerError = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return panel
	}

	// Decode into a flexible shape — broker may add fields and
	// we shouldn't break the panel render on minor schema drift.
	var raw struct {
		Configured map[string]any `json:"configured"`
		State      map[string]any `json:"state"`
		Portfolio  map[string]any `json:"portfolio"`
		PortfolioE string         `json:"portfolio_error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		panel.BrokerError = fmt.Sprintf("decode: %v", err)
		return panel
	}

	panel.BrokerReachable = true
	panel.Mode = stringField(raw.Configured, "mode")
	panel.MaxPositionUSD = floatField(raw.Configured, "max_position_usd")
	panel.MaxDailyTurnoverUSD = floatField(raw.Configured, "max_daily_turnover_usd")
	panel.MaxOrdersPerHour = intField(raw.Configured, "max_orders_per_hour")
	panel.MaxOrdersPerMinute = intField(raw.Configured, "max_orders_per_minute")
	panel.DrawdownCircuitBreakerPct = floatField(raw.Configured, "drawdown_circuit_breaker_pct")
	panel.RequireStopLoss = boolField(raw.Configured, "require_stop_loss")
	panel.DefaultStopLossPct = floatField(raw.Configured, "default_stop_loss_pct")

	panel.KillSwitch = boolField(raw.State, "kill_switch")
	panel.HWMUSD = floatField(raw.State, "hwm_usd")
	panel.TodayUTC = stringField(raw.State, "today_utc")
	panel.TodayTurnoverUSD = floatField(raw.State, "today_turnover_usd")
	panel.OrdersInLastHour = intField(raw.State, "orders_in_last_hour")
	panel.OrdersInLastMinute = intField(raw.State, "orders_in_last_minute")

	if raw.Portfolio != nil {
		panel.PortfolioReachable = true
		panel.Account = stringField(raw.Portfolio, "account")
		panel.CashUSD = floatField(raw.Portfolio, "cash_usd")
		panel.EquityUSD = floatField(raw.Portfolio, "equity_usd")
		panel.BuyingPowerUSD = floatField(raw.Portfolio, "buying_power_usd")
		panel.UnrealisedPLUSD = floatField(raw.Portfolio, "unrealised_pl_usd")
		panel.RealisedPLDayUSD = floatField(raw.Portfolio, "realised_pl_day_usd")
		panel.OpenPositions = intField(raw.Portfolio, "open_positions")
		panel.DrawdownPct = floatField(raw.Portfolio, "drawdown_pct")
	} else {
		panel.PortfolioError = raw.PortfolioE
	}

	// Load equity snapshots over the soak window (30d). Best-
	// effort: a repo error here just leaves Soak zero-valued;
	// the live Portfolio block above is the load-bearing
	// info. snapshotRepo is nil in tests / minimal deployments.
	if s.tradingSnapshotRepo != nil {
		soakCtx, soakCancel := context.WithTimeout(ctx, 3*time.Second)
		defer soakCancel()
		since := time.Now().UTC().Add(-tradingSnapshotRetentionWindow)
		snaps, err := s.tradingSnapshotRepo.ListSince(soakCtx, project.ID, since, 0)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", project.ID).
				Msg("trading panel: snapshot fetch failed; soak metrics unavailable")
		} else {
			panel.Soak = computeSoakMetrics(snaps)
		}
	}

	// Safety event audit — Phase 2 of the broker→daemon audit
	// channel. Recent decisions (refusals, replay hits,
	// breaker trips) for the cross-component audit timeline.
	// 24h count surfaces as a headline; recent 20 land in
	// the inline list. Best-effort like the other repos.
	if s.tradingSafetyRepo != nil {
		seCtx, seCancel := context.WithTimeout(ctx, 3*time.Second)
		defer seCancel()
		pid := project.ID
		// Count window: the selected window when one is given
		// (dashboard), else the fixed last-24h headline (project panel).
		countSince := since
		if countSince.IsZero() {
			countSince = time.Now().UTC().Add(-24 * time.Hour)
		}
		if n, err := s.tradingSafetyRepo.Count(seCtx, persistence.TradingSafetyEventFilter{
			ProjectID: &pid,
			Since:     &countSince,
		}); err == nil {
			panel.SafetyEvents24hCount = n
		}
		listFilter := persistence.TradingSafetyEventFilter{ProjectID: &pid, PageSize: 20}
		if !since.IsZero() {
			listFilter.Since = &since
		}
		if rows, err := s.tradingSafetyRepo.List(seCtx, listFilter); err == nil {
			for _, ev := range rows {
				if ev == nil {
					continue
				}
				row := SafetyEventRow{
					Kind:          ev.Kind,
					Severity:      ev.Severity,
					RecordedAt:    ev.RecordedAt,
					SeverityClass: severityClass(ev.Severity),
				}
				if ev.Symbol != nil {
					row.Symbol = *ev.Symbol
				}
				if len(ev.Detail) > 0 {
					_ = json.Unmarshal(ev.Detail, &row.Detail)
				}
				panel.SafetyEvents = append(panel.SafetyEvents, row)
			}
		}
	}

	// Order audit metrics — Phase 1 of the broker→daemon audit
	// channel. Counts come from trading_orders, populated by
	// the broker's AuditWriter on every place_order /
	// place_bracket_order. Best-effort: a repo error or
	// missing repo leaves the tiles zeroed.
	if s.tradingOrderRepo != nil {
		ordCtx, ordCancel := context.WithTimeout(ctx, 3*time.Second)
		defer ordCancel()
		todayStart := time.Now().UTC().Truncate(24 * time.Hour)
		sevenStart := time.Now().UTC().Add(-7 * 24 * time.Hour)
		pid := project.ID
		// Counts: include refused orders so the operator sees
		// "broker rejected N today" alongside "broker placed M".
		if n, err := s.tradingOrderRepo.Count(ordCtx, persistence.TradingOrderFilter{
			ProjectID: &pid,
			Since:     &todayStart,
		}); err == nil {
			panel.Soak.OrdersToday = n
		}
		if n, err := s.tradingOrderRepo.Count(ordCtx, persistence.TradingOrderFilter{
			ProjectID: &pid,
			Since:     &sevenStart,
		}); err == nil {
			panel.Soak.Orders7d = n
		}
		// Volume: prefer the precise SUM(qty × fill_price) from
		// trading_fills when Phase-3 fill ingestion is wired
		// (tradingFillRepo != nil). Falls back to the legacy
		// trading_orders LMT-price estimate when fills aren't
		// available — that estimate over-counted because it
		// used the limit price the moment a row was submitted,
		// before the fill was confirmed (or cancelled). Once
		// fills are flowing, the estimate's rejected/cancelled
		// filter helped but couldn't compensate for partial
		// fills counted at full notional.
		if s.tradingFillRepo != nil {
			if v, err := s.tradingFillRepo.SumVolume(ordCtx, persistence.TradingFillFilter{
				ProjectID: &pid,
				Since:     &todayStart,
			}); err == nil {
				panel.Soak.VolumeTodayUSD = v
			}
			if v, err := s.tradingFillRepo.SumVolume(ordCtx, persistence.TradingFillFilter{
				ProjectID: &pid,
				Since:     &sevenStart,
			}); err == nil {
				panel.Soak.Volume7dUSD = v
			}
		} else if rows, err := s.tradingOrderRepo.List(ordCtx, persistence.TradingOrderFilter{
			ProjectID: &pid,
			Since:     &sevenStart,
			PageSize:  5000,
		}); err == nil {
			for _, o := range rows {
				if o == nil {
					continue
				}
				// Volume = orders that resulted in (or might
				// still result in) a fill at IBKR. Cancelled
				// and refused rows are attempts, not volume —
				// summing them inflates the gauge by the
				// notional of every revoked entry. Order
				// counts above include them so the operator
				// still sees "broker rejected N today"; the
				// volume gauge stays clean.
				switch o.Status {
				case "cancelled", "refused", "rejected":
					continue
				}
				px := orderEffectivePrice(o)
				if px == 0 {
					continue
				}
				v := o.Qty * px
				panel.Soak.Volume7dUSD += v
				if !o.SubmittedAt.Before(todayStart) {
					panel.Soak.VolumeTodayUSD += v
				}
			}
		}

		// Trading history — flatten the N most-recent orders for
		// the inline table. Repo.List returns rows newest first;
		// we just truncate to recentOrdersLimit. Best-effort: a
		// repo error leaves the history empty without failing the
		// page render.
		recentOrderFilter := persistence.TradingOrderFilter{ProjectID: &pid, PageSize: recentOrdersLimit}
		if !since.IsZero() {
			recentOrderFilter.Since = &since
		}
		if rows, err := s.tradingOrderRepo.List(ordCtx, recentOrderFilter); err == nil {
			panel.RecentOrders = make([]TradingOrderRow, 0, len(rows))
			for _, o := range rows {
				if o == nil {
					continue
				}
				panel.RecentOrders = append(panel.RecentOrders, flattenOrderRow(o))
			}
		}
	}
	if s.tradingFillRepo != nil {
		fillCtx, fillCancel := context.WithTimeout(ctx, 3*time.Second)
		defer fillCancel()
		pid := project.ID
		recentFillFilter := persistence.TradingFillFilter{ProjectID: &pid, PageSize: recentFillsLimit}
		if !since.IsZero() {
			recentFillFilter.Since = &since
		}
		if rows, err := s.tradingFillRepo.List(fillCtx, recentFillFilter); err == nil {
			panel.RecentFills = make([]TradingFillRow, 0, len(rows))
			for _, f := range rows {
				if f == nil {
					continue
				}
				panel.RecentFills = append(panel.RecentFills, flattenFillRow(f))
			}
		}
	}
	return panel
}

// flattenOrderRow projects a TradingOrder onto the template-friendly
// TradingOrderRow shape. Pointer fields collapse to 0 when unset;
// status / action drive their respective pill classes so the template
// can render without nil checks or a switch statement.
func flattenOrderRow(o *persistence.TradingOrder) TradingOrderRow {
	row := TradingOrderRow{
		ID:           o.ID,
		Symbol:       o.Symbol,
		Action:       o.Action,
		OrderType:    o.OrderType,
		Qty:          o.Qty,
		TimeInForce:  o.TimeInForce,
		Status:       o.Status,
		StatusReason: o.LastStatusReason,
		SubmittedAt:  o.SubmittedAt,
		StatusClass:  orderStatusClass(o.Status),
		ActionClass:  orderActionClass(o.Action),
	}
	if o.BrokerOrderID != nil {
		row.BrokerOrderID = *o.BrokerOrderID
	}
	if o.LimitPrice != nil {
		row.LimitPrice = *o.LimitPrice
	}
	if o.StopPrice != nil {
		row.StopPrice = *o.StopPrice
	}
	if o.TerminalAt != nil {
		row.TerminalAt = *o.TerminalAt
	}
	return row
}

// flattenFillRow projects a TradingFill onto the template row shape
// and precomputes notional so the template doesn't multiply.
func flattenFillRow(f *persistence.TradingFill) TradingFillRow {
	row := TradingFillRow{
		ID:       f.ID,
		OrderID:  f.OrderID,
		Symbol:   f.Symbol,
		Qty:      f.Qty,
		Price:    f.Price,
		Notional: f.Qty * f.Price,
		FilledAt: f.FilledAt,
	}
	if f.CommissionUSD != nil {
		row.CommissionUSD = *f.CommissionUSD
	}
	return row
}

// fillAccountFromSnapshot populates the account block of `panel` from
// the most recent trading_positions_snapshots row when the live broker
// portfolio fetch failed (PortfolioReachable == false). It is used by
// the Insight trading dashboard so the operator still sees cash / equity
// / P&L when IB Gateway is reconnecting; the project-detail panel keeps
// its live-only behavior and does not call this.
//
// Buying power is not persisted in the snapshot, so it is left zero and
// the template hides that tile in fallback mode. Returns (recordedAt,
// snapApplied) when a snapshot was applied; (zero, snapUnavailable) when the
// store is nil or the query errored (a vornik-storage problem); (zero,
// snapNone) when the store is readable but holds no snapshot in the retention
// window. The caller maps the kind onto a distinct UI hint.
func (s *Server) fillAccountFromSnapshot(ctx context.Context, projectID string, panel *TradingPanel) (time.Time, snapshotFallback) {
	if s.tradingSnapshotRepo == nil {
		return time.Time{}, snapUnavailable
	}
	snapCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	since := time.Now().UTC().Add(-tradingSnapshotRetentionWindow)
	snaps, err := s.tradingSnapshotRepo.ListSince(snapCtx, projectID, since, 0)
	if err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).
			Msg("trading dashboard: snapshot fallback fetch failed")
		return time.Time{}, snapUnavailable
	}
	if len(snaps) == 0 {
		return time.Time{}, snapNone
	}
	// ListSince returns oldest-first; the last element is newest.
	latest := snaps[len(snaps)-1]
	if latest == nil {
		return time.Time{}, snapNone
	}
	panel.Account = ""
	panel.CashUSD = latest.CashUSD
	panel.EquityUSD = latest.EquityUSD
	panel.UnrealisedPLUSD = latest.UnrealisedPLUSD
	panel.RealisedPLDayUSD = latest.RealisedPLDayUSD
	return latest.RecordedAt, snapApplied
}

// orderStatusClass maps the broker's trading_orders.status enum onto
// the same outcome-* CSS classes the safety-events row uses. Mirrors
// severityClass so the page has one visual vocabulary for "good /
// neutral / bad" across both panels.
func orderStatusClass(status string) string {
	switch status {
	case "filled":
		return "outcome-good"
	case "submitted", "partial":
		return "outcome-neutral"
	case "cancelled", "refused", "rejected":
		return "outcome-bad"
	default:
		// Unknown statuses surface as warn so a new broker-side
		// status string (e.g. "pending_routing") doesn't silently
		// look like a success.
		return "outcome-warn"
	}
}

// orderActionClass tints BUY rows green and SELL rows rose so an
// operator scanning the orders table can spot direction without
// reading the column.
func orderActionClass(action string) string {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "BUY":
		return "text-emerald-400"
	case "SELL":
		return "text-rose-400"
	default:
		return "text-gray-300"
	}
}

// severityClass maps a safety-event severity string to a
// CSS class the template uses for the pill colour. Mirrors
// the judgeVerdictCSSClass / outcome class helpers — kept
// here so the template stays dumb.
func severityClass(severity string) string {
	switch severity {
	case "critical":
		return "outcome-bad"
	case "warn":
		return "outcome-warn"
	default:
		return "outcome-neutral"
	}
}

// orderEffectivePrice picks a usable price for volume estimation
// from a trading_orders row. LMT orders carry the price the
// agent committed to; STP / STP_LMT carry their trigger
// price. Plain MKT orders without a fill leg yet return 0 —
// the volume calc skips them rather than guess. When fills
// land in trading_fills (Phase 3), volume becomes precise
// via SUM(qty * fill_price); this is the fast estimate.
func orderEffectivePrice(o *persistence.TradingOrder) float64 {
	if o == nil {
		return 0
	}
	if o.LimitPrice != nil && *o.LimitPrice > 0 {
		return *o.LimitPrice
	}
	if o.StopPrice != nil && *o.StopPrice > 0 {
		return *o.StopPrice
	}
	return 0
}

// stringField / floatField / intField / boolField are tolerant
// extractors for the typeless map shape returned by JSON
// decoding. Each returns the zero value of the target type when
// the key is missing or the underlying type doesn't match —
// keeps buildTradingPanel terse and resilient to broker schema
// additions.
func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func floatField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func boolField(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
