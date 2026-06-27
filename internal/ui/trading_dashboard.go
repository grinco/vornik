// Trading dashboard: a dedicated end-to-end trading overview under the
// Insight area. Where the project-detail page buries the per-project
// trading panel in a reference section, this page promotes it to a
// first-class, cross-project view with a trading-enabled-only project
// dropdown and a 24h/7d/30d window selector (mirroring /ui/spend).
//
// It adds no persistence and no broker endpoints — it reuses
// buildTradingPanelWindow (the same live /caps fetch + audit-repo reads
// the project panel uses) and falls back to the latest persisted
// positions snapshot for the account block when the broker is offline.

package ui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/tradingpnl"
)

// TradingDashboardData carries everything trading_dashboard.html needs.
// It embeds the reused per-project TradingPanel and adds the dashboard
// chrome (project dropdown, window selector, snapshot provenance).
type TradingDashboardData struct {
	Title       string
	CurrentPage string // "trading" → maps to the Insight nav area

	// Project filter — only trading-enabled, access-scoped projects.
	// ProjectID is the active selection; "" only in the empty state.
	ProjectID    string
	ProjectsList []string

	// Window selector, mirroring SpendData. WindowDays ∈ {1,7,30}.
	WindowDays  int
	WindowLabel string

	// AccountFromSnapshot is true when the live broker portfolio fetch
	// failed and the account block was populated from the latest
	// persisted snapshot instead; SnapshotAsOf is that row's timestamp.
	AccountFromSnapshot bool
	SnapshotAsOf        time.Time
	// AccountStatus is the structured provenance of the account block so the
	// operator can tell WHY data is (or isn't) live, not just "snapshot":
	//   "live"                 — fetched live from the broker.
	//   "snapshot_fresh"       — broker leg unavailable; a recent snapshot is shown.
	//   "snapshot_stale"       — snapshot shown but older than the soft staleness
	//                            bound (the sampler hasn't run → broker down a while).
	//   "snapshot_expired"     — snapshot older than the HARD bound (>24h): the
	//                            figures are no longer safe to act on.
	//   "no_snapshot"          — broker unreachable and no snapshot row exists.
	//   "snapshot_unavailable" — broker unreachable and the snapshot store itself
	//                            could not be read (nil/errored) — distinct from
	//                            "no snapshot exists" so the operator knows whether
	//                            to look at the broker or at vornik's own storage.
	AccountStatus string

	// HasTradingProjects distinguishes "no projects have trading
	// enabled" (empty state) from a normal render.
	HasTradingProjects bool

	// NoAccessibleTrading is true only in the empty state when the
	// deployment HAS trading-enabled projects but the caller can access
	// none of them — an access-scope problem, not a configuration gap.
	// Lets the empty state say "you can't see any" vs "none are configured".
	NoAccessibleTrading bool

	// Panel is the reused per-project trading payload (account, caps,
	// soak, recent orders/fills, safety events).
	Panel TradingPanel

	// Perf is the computed round-trip P&L performance for the selected
	// window. Populated when the fill and order repos are wired. Zero
	// value (Trades==0) renders the empty state.
	//
	// Section order on the page (top to bottom): status banner →
	// Trading Performance (this, vornik's own "are we winning?" metrics) →
	// Account snapshot (broker point-in-time) → caps → soak → recent
	// orders → fills → safety events. Performance is deliberately FIRST so
	// the operator sees realized P&L before the broker balance.
	Perf tradingpnl.Performance

	// PerfProfitFactor is the pre-formatted profit factor string: the
	// decimal value when ProfitFactor != nil, otherwise "—" (so the
	// template can render it without dereferencing a *float64).
	PerfProfitFactor string

	// PerfChart holds the server-rendered SVG geometry for the daily
	// P&L bar + equity-line chart in the Trading Performance section.
	PerfChart perfChartData

	// PerfChartBaseline is the Y coordinate of the zero-line in the SVG
	// (mid-point of the chart area). Pre-computed so the template need
	// not do arithmetic.
	PerfChartBaseline int
}

// Trading renders the Insight → Trading dashboard. Registered at
// /ui/trading (the /ui prefix + auth middleware are applied by the
// same subtree wrapper that protects /spend).
func (s *Server) Trading(w http.ResponseWriter, r *http.Request) {
	data := TradingDashboardData{
		Title:       "Trading",
		CurrentPage: "trading",
		WindowDays:  7,
		WindowLabel: "7 days",
		ProjectID:   r.URL.Query().Get("project"),
	}

	switch r.URL.Query().Get("window") {
	case "24h", "1d":
		data.WindowDays = 1
		data.WindowLabel = "24 hours"
	case "30d":
		data.WindowDays = 30
		data.WindowLabel = "30 days"
	case "7d":
		data.WindowDays = 7
		data.WindowLabel = "7 days"
	}

	// Build the dropdown: only projects that (a) have a `broker` MCP
	// server configured (trading enabled) and (b) the caller may access.
	tradingEnabledTotal := 0
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			if p == nil || brokerURL(p) == "" {
				continue
			}
			tradingEnabledTotal++
			if !api.RequestAllowsProject(r, p.ID) {
				continue
			}
			data.ProjectsList = append(data.ProjectsList, p.ID)
		}
	}
	data.HasTradingProjects = len(data.ProjectsList) > 0

	// Empty state: nothing the caller can see has trading enabled. Distinguish
	// "the deployment has trading projects but you can't access any" (a scope
	// problem) from "none are configured at all" (a setup gap).
	if !data.HasTradingProjects {
		data.ProjectID = ""
		data.NoAccessibleTrading = tradingEnabledTotal > 0
		s.render(w, "trading_dashboard.html", data)
		return
	}

	// Resolve the selection. An explicit ?project= must be both
	// accessible and trading-enabled; otherwise 403 (matches spend's
	// posture for explicitly-denied selections).
	if data.ProjectID != "" {
		if !api.RequestAllowsProject(r, data.ProjectID) {
			http.Error(w, "access denied to project", http.StatusForbidden)
			return
		}
		if !containsString(data.ProjectsList, data.ProjectID) {
			http.Error(w, "project does not have trading enabled", http.StatusForbidden)
			return
		}
	} else {
		data.ProjectID = data.ProjectsList[0]
	}

	project := s.lookupProject(data.ProjectID)
	if project == nil {
		// Listed a moment ago but gone now (config reload race). Render
		// the chrome without a panel rather than 500'ing.
		s.render(w, "trading_dashboard.html", data)
		return
	}

	now := time.Now().UTC()
	since := now.Add(-time.Duration(data.WindowDays) * 24 * time.Hour)
	data.Panel = s.buildTradingPanelWindow(r.Context(), project, since)

	// Live + DB fallback: when the broker is up but its portfolio leg
	// is unavailable, or the broker is unreachable entirely, fill the
	// account block from the most recent persisted snapshot.
	fallback := snapNone
	var snapAsOf time.Time
	if data.Panel.Enabled && !data.Panel.PortfolioReachable {
		snapAsOf, fallback = s.fillAccountFromSnapshot(r.Context(), project.ID, &data.Panel)
		if fallback == snapApplied {
			data.AccountFromSnapshot = true
			data.SnapshotAsOf = snapAsOf
		}
	}
	data.AccountStatus = tradingAccountStatus(data.Panel.Enabled, data.Panel.PortfolioReachable, fallback, data.SnapshotAsOf, now)

	// Trading Performance — compute realized round-trip P&L and equity
	// curve for the window. Best-effort; nil repos silently skip each step.
	data.Perf, data.PerfChart = s.buildTradingPerformance(r.Context(), project.ID, since, now)
	if data.Perf.ProfitFactor != nil {
		data.PerfProfitFactor = fmt.Sprintf("%.2f", *data.Perf.ProfitFactor)
	} else {
		data.PerfProfitFactor = "—"
	}
	data.PerfChartBaseline = perfChartTop + perfChartH/2

	s.render(w, "trading_dashboard.html", data)
}

// perfFillsPageSize is the page cap used when fetching fills for P&L
// computation. 10 000 covers ≈ 3 months of 100 trades/day at an aggressive
// pace; increase only if production trace shows truncation.
const perfFillsPageSize = 10_000

// buildTradingPerformance fetches fills + orders + snapshots for the project
// and computes the realized P&L performance + chart geometry.
//
// fillsSince = windowStart − 30 d grace, so cross-day open/close pairs match.
// Aggregate then filters closed trades to ExitAt ∈ [windowStart, now).
// All repo calls are nil-guarded so minimal / non-trading deployments render.
func (s *Server) buildTradingPerformance(
	ctx context.Context,
	projectID string,
	windowStart, now time.Time,
) (tradingpnl.Performance, perfChartData) {
	fillsSince := windowStart.Add(-30 * 24 * time.Hour) // open-position grace

	// Step 1: load fills with extended lookback.
	var fills []*persistence.TradingFill
	if s.tradingFillRepo != nil {
		fCtx, fCancel := context.WithTimeout(ctx, 4*time.Second)
		defer fCancel()
		pid := projectID
		since := fillsSince
		rows, err := s.tradingFillRepo.List(fCtx, persistence.TradingFillFilter{
			ProjectID: &pid,
			Since:     &since,
			PageSize:  perfFillsPageSize,
		})
		if err == nil {
			fills = rows
		}
	}

	// Step 2: build sideByOrderID from the order repo.
	sideByOrderID := make(map[string]string)
	if s.tradingOrderRepo != nil && len(fills) > 0 {
		oCtx, oCancel := context.WithTimeout(ctx, 4*time.Second)
		defer oCancel()
		pid := projectID
		since := fillsSince
		orders, err := s.tradingOrderRepo.List(oCtx, persistence.TradingOrderFilter{
			ProjectID: &pid,
			Since:     &since,
			PageSize:  perfFillsPageSize,
		})
		if err == nil {
			for _, o := range orders {
				if o != nil {
					sideByOrderID[o.ID] = o.Action
				}
			}
		}
	}

	// Step 3: pair fills into closed trades + aggregate metrics.
	trades := tradingpnl.PairRoundTrips(fills, sideByOrderID)
	perf := tradingpnl.Aggregate(trades, windowStart, now)

	// Step 4: equity curve from snapshots.
	var equityPts []tradingpnl.DailyPoint
	if s.tradingSnapshotRepo != nil {
		sCtx, sCancel := context.WithTimeout(ctx, 4*time.Second)
		defer sCancel()
		snaps, err := s.tradingSnapshotRepo.ListSince(sCtx, projectID, windowStart, 0)
		if err == nil {
			equityPts = tradingpnl.EquityCurve(snaps, windowStart, now)
		}
	}

	// Step 5: merge realized daily series + equity series → chart geometry.
	merged := mergePerfDaily(perf.Daily, equityPts)
	chart := layoutPerfChart(merged)

	return perf, chart
}

// tradingSnapshotStaleAfter — the SOFT bound. A position snapshot older than
// this means the equity sampler (≈60s cadence) hasn't refreshed it, i.e. the
// broker has been unreachable for a while; the figures are getting old.
const tradingSnapshotStaleAfter = 15 * time.Minute

// tradingSnapshotExpiredAfter — the HARD bound. Past this the snapshot is so
// old the account figures must not be acted on; the UI raises a loud warning
// distinct from the soft "stale" hint. Well inside the
// tradingSnapshotRetentionWindow, so an expired-but-present row is normal
// after a long broker outage (not a retention artefact).
const tradingSnapshotExpiredAfter = 24 * time.Hour

// tradingSnapshotRetentionWindow is the lookback the snapshot fallback query
// uses (and the documented retention SLO it cross-references): vornik keeps
// ~30 days of position snapshots at the default sampler cadence, so a fallback
// older than this window simply won't be found. Named so the query and the SLO
// can't drift apart silently.
const tradingSnapshotRetentionWindow = 30 * 24 * time.Hour

// snapshotFallback is the structured outcome of the snapshot fallback attempt,
// so the UI can distinguish "no snapshot exists yet" from "the snapshot store
// could not be read" instead of collapsing both into a bare false.
type snapshotFallback int

const (
	// snapNone — no snapshot was applied because none exists in the retention
	// window (or the fallback was not attempted). The zero value.
	snapNone snapshotFallback = iota
	// snapApplied — a snapshot row was found and applied to the panel.
	snapApplied
	// snapUnavailable — the snapshot store is nil or the query errored; we
	// can't tell whether a snapshot exists. A vornik-storage problem, not a
	// broker one.
	snapUnavailable
)

// tradingAccountStatus classifies the account block's provenance so the
// operator can distinguish a live read, the freshness tier of a fallback
// snapshot, and — when there's no usable snapshot — whether none exists vs the
// store was unreadable. Returns "" when trading isn't enabled (the
// disabled/empty state owns that case).
func tradingAccountStatus(enabled, reachable bool, fb snapshotFallback, asOf time.Time, now time.Time) string {
	switch {
	case !enabled:
		return ""
	case reachable:
		return "live"
	case fb == snapApplied:
		switch age := now.Sub(asOf); {
		case age > tradingSnapshotExpiredAfter:
			return "snapshot_expired"
		case age > tradingSnapshotStaleAfter:
			return "snapshot_stale"
		default:
			return "snapshot_fresh"
		}
	case fb == snapUnavailable:
		return "snapshot_unavailable"
	default:
		return "no_snapshot"
	}
}

// lookupProject resolves a project by ID via the registry, nil-safe.
func (s *Server) lookupProject(id string) *registry.Project {
	if s.projectReg == nil {
		return nil
	}
	return s.projectReg.GetProject(id)
}
