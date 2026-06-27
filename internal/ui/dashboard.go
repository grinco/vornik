// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// DashboardData holds data for the dashboard template.
type DashboardData struct {
	Title        string
	CurrentPage  string
	ProjectCount int
	TaskCounts   map[persistence.TaskStatus]int64
	// 2026.4.11+ financial + model-performance summary. Zero values
	// when task_llm_usage is empty or the repo isn't wired (old deployments).
	Spend     DashboardSpend
	TopModels []persistence.RoleModelSpend // top 10 by 30d cost

	// Landing-page tiles (2026-04-30). Pulls in-flight signals into
	// one front door so operators don't have to bookmark per-page
	// URLs to see "what's happening right now".
	ActiveTasks     []*persistence.Task // status in QUEUED/LEASED/RUNNING, capped at Limit
	ActiveTaskCount int
	ActiveChatCount int                  // count of active Telegram conversations
	AutonomyETAs    []AutonomyProjectETA // per-project next-eval projection, sorted by soonest
	// Limit / LimitOptions drive the shared page-size selector on
	// the landing page's Active Tasks tile. Defaults to 20 like the
	// audit page; the operator picks from 10/20/50/100 via ?limit=N.
	Limit        int
	LimitOptions []int

	// 2026-05-12: surface newer subsystems on the landing page so the
	// operator doesn't have to drill into /ui/memory or per-project
	// pages to see "is the pipeline healthy / are there safety events
	// / how's the judge doing". All three structs zero-value cleanly
	// when the underlying repos aren't wired.
	Memory  DashboardMemory  // KG extraction + ingest queue snapshot
	Trading DashboardTrading // safety-envelope rollup across projects
	Judge   DashboardJudge   // hallucination judge rollup over 24h

	// 2026-05-22: cache effectiveness tile so the operator sees
	// Phase E paying off from the front door. Available=false hides
	// the tile entirely on installs where the cache is disabled or
	// has no hits yet — avoids a confusing "$0 saved" empty state.
	CacheSavings DashboardCacheSavings

	// OperatorQueue (Upgrade #4 — 2026-05-26) is the action-oriented
	// rail rendered at the top of the dashboard. Each card answers
	// "what needs me?" rather than "how many?". Replaces the implicit
	// "operators figure out where to look" pattern. Empty struct =
	// the dashboard is healthy and no card renders.
	OperatorQueue DashboardOperatorQueue

	// IsSession is true when the request authenticated via a browser
	// session cookie (github-login phase 3). Drives the nav logout
	// button — api-key callers have no session to revoke. Read by the
	// hasSessionFlag template helper.
	IsSession bool
}

// DashboardOperatorQueue is the action-oriented queue rail rendered
// at the top of the dashboard (Upgrade #4 — 2026-05-26). Each field
// represents one queue card; the card renders only when its count is
// non-zero, so a quiet dashboard collapses to a single "Healthy"
// indicator rather than a sea of zero-tiles.
//
// Counts are loaded best-effort — a DB blip on any one of them leaves
// that field at zero and the card hides. Operators always see the
// pieces that DID load.
type DashboardOperatorQueue struct {
	// AwaitingInput is the operator-input-blocked count. Duplicates
	// the existing inbox tile's source but rendered as part of the
	// rail so the queue feels uniform. The inbox tile above the rail
	// stays as a more prominent surface; this is the rail-shaped
	// echo so the queue's visual weight is consistent.
	AwaitingInput int64
	// AwaitingApproval is the count of autonomy tasks parked in
	// AWAITING_APPROVAL (created under requireApproval) waiting for the
	// operator to approve/reject. Surfaced as its own tile so the
	// distinct call to action ("approve me") reads separately from
	// "answer a checkpoint". See
	// https://docs.vornik.io
	AwaitingApproval int64
	// FailedRecently is the count of tasks that reached FAILED in the
	// last 24h. Drives the operator-attention card with class-
	// breakdown link.
	FailedRecently int64
	// StuckTotal is the count of tasks stuck in WAITING_FOR_CHILDREN
	// — the most common "non-terminal but no events flowing" state.
	// Lighter heuristic than scanning every non-terminal task for
	// last-event-age; the startup sweep (2026-05-26) now handles the
	// historical class.
	StuckTotal int64
	// SpendSpike is true when today's 24h spend > 2x the 7d average
	// daily rate. Visible budget-alert affordance.
	SpendSpike bool
	// SpendSpikeRatio is the multiplier today/avg (e.g. 2.4 means
	// today is 2.4x the rolling average). Drives the card detail
	// text. Zero when SpendSpike is false.
	SpendSpikeRatio float64
}

// AutonomyProjectETA is one project's autonomy state for the
// landing page. Provides "when is this project's lead going to fire
// again" so the operator can predict spend bursts and sanity-check
// that autonomy is healthy.
type AutonomyProjectETA struct {
	ProjectID  string
	Interval   time.Duration
	LastEvalAt time.Time
	NextEvalAt time.Time
	// LastOutcome is the most recent evaluation's outcome string
	// (CREATED / NO_ACTION / RATE_LIMITED / ...). Lets operators
	// glance at the page and see whether the loop is firing
	// productively or just churning.
	LastOutcome string
	// AgoLabel is a pre-rendered "3m 24s ago" string for LastEvalAt.
	// Computed handler-side because Go templates can't subtract
	// times cleanly without a helper funcmap entry.
	AgoLabel string
	// EtaLabel is "in 1m 36s" or "due now" for NextEvalAt — the
	// initial server-render value. The dashboard's client-side
	// ticker re-derives this every second from NextEvalAtISO so
	// the countdown is live; without that ticker the label froze
	// at page-load time and never moved (operator-reported
	// 2026-05-05).
	EtaLabel string
	// NextEvalAtISO is NextEvalAt rendered as RFC3339 for the
	// client-side ticker to parse. Zero-time projects emit empty
	// string; the ticker leaves their label alone.
	NextEvalAtISO string
}

// DashboardMemory is the cross-project memory-pipeline snapshot
// shown on the landing page. KG fields mirror persistence.KGStats
// for the global extraction worker; IngestBacklog is the sum of
// `queued+processing` rows across every project, which spikes
// when ingestion is starved (the broker is publishing faster than
// the worker can drain).
type DashboardMemory struct {
	Enabled        bool // chunkGraph repo wired
	ChunksPending  int
	ChunksDone     int
	ChunksTotal    int
	PercentDone    float64
	Entities       int
	Edges          int
	Mentions       int
	IngestBacklog  int // sum across all projects (queued+processing rows)
	IngestProjects int // number of projects with any backlog
}

// DashboardTrading is the cross-project safety-envelope rollup
// shown on the landing page. Sourced from project YAML (Mode /
// KillSwitch) + the safety-event audit channel (24h count). Zero-
// values render an empty-state tile when no project has trading
// enabled at all.
type DashboardTrading struct {
	EnabledCount    int   // projects with any non-empty Trading.Mode
	LiveCount       int   // subset that are mode=live
	PaperCount      int   // subset that are mode=paper
	KillSwitchCount int   // subset with KillSwitch=true
	SafetyEvents24h int64 // tradingSafetyRepo.Count over 24h
	BreakerTrips24h int64 // safety events of kind=breaker_trip in 24h
}

// DashboardJudge is the LLM-as-judge verdict rollup shown on the
// landing page. Pulled from task_judge_verdicts over the last 24h
// (or the most recent N rows if the window has no traffic). Zero
// when judgeVerdictRepo isn't wired or the project has no
// verdicts yet.
type DashboardJudge struct {
	Enabled     bool // judgeVerdictRepo wired
	WindowHours int  // window the counts cover (24h by default)
	Pass        int
	Fail        int
	Abstain     int
	Total       int
	PercentPass float64 // Pass / Total * 100, 0 when Total == 0
}

// DashboardSpend is the headline $ summary shown on the dashboard.
type DashboardSpend struct {
	Day24hUSD      float64
	Day7USD        float64
	Day30USD       float64
	MonthToDateUSD float64
	// Sparkline is a pre-computed SVG polyline points string for the
	// 24h hourly spend chart. Empty string means "don't render" — repo
	// not wired, no data, or all hours zero.
	Sparkline       string
	SparklineFilled string // closed polyline for the filled-area underlay
}

// DashboardCacheSavings is the at-a-glance LLM-cache effectiveness
// tile on the landing page. Pulls the response cache's lifetime
// hits + $ saved so operators see the feature paying off without
// drilling into /ui/spend. Available=false hides the tile (cache
// disabled or no hits yet — a "$0 saved" tile is more confusing
// than an absent one on fresh installs).
type DashboardCacheSavings struct {
	Available       bool
	TotalSavingsUSD float64
	TotalHits       int64
}

// Dashboard renders the main dashboard page.
func (s *Server) Dashboard(w http.ResponseWriter, r *http.Request) {
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		// Project-scoped users get the operator dashboard's instance-wide
		// aggregates gated away, so land them on Tasks (their day-to-day
		// surface) rather than an empty dashboard or the Projects list.
		http.Redirect(w, r, "/ui/tasks", http.StatusFound)
		return
	}
	s.logger.Debug().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msg("rendering dashboard")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := parsePageSize(r.URL.Query().Get("limit"))
	data := DashboardData{
		Title:        "Dashboard",
		CurrentPage:  "dashboard",
		TaskCounts:   make(map[persistence.TaskStatus]int64),
		Limit:        limit,
		LimitOptions: PageSizeOptions,
		// Show the logout button for browser-session logins.
		IsSession: api.SessionRoleFromContext(r.Context()) != "",
	}

	// Get project count
	if s.projectReg != nil {
		stats := s.projectReg.GetStats()
		data.ProjectCount = stats.ProjectCount
	}

	// Get task counts by status
	if s.taskRepo != nil {
		// Get counts across all projects by passing empty string
		counts, err := s.taskRepo.CountByStatus(ctx, "")
		if err == nil {
			data.TaskCounts = counts
		} else {
			s.logger.Warn().Err(err).Msg("failed to load task counts for dashboard")
		}

	}

	// Financial summary: 4 rolling windows + top-10 role+model leaderboard
	// by 30-day spend. INSTANCE-WIDE (SumCost/AggregateByRoleModel are not
	// project-scoped here), so gated to all-access callers — a project-
	// scoped session must not see cross-project spend; it uses
	// /ui/spend?project=<id> (project-scoped) instead. Silent degrade when
	// the repo isn't wired or no rows match — UI renders $0.00.
	if s.llmUsageRepo != nil && requestHasAllProjectAccess(r) {
		now := time.Now().UTC()
		windows := []struct {
			setter func(float64)
			since  time.Time
		}{
			{func(v float64) { data.Spend.Day24hUSD = v }, now.Add(-24 * time.Hour)},
			{func(v float64) { data.Spend.Day7USD = v }, now.Add(-7 * 24 * time.Hour)},
			{func(v float64) { data.Spend.Day30USD = v }, now.Add(-30 * 24 * time.Hour)},
			{func(v float64) { data.Spend.MonthToDateUSD = v }, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)},
		}
		for _, w := range windows {
			if v, err := s.llmUsageRepo.SumCost(ctx, w.since, time.Time{}); err == nil {
				w.setter(v)
			}
		}
		if rows, err := s.llmUsageRepo.AggregateByRoleModel(ctx, now.Add(-30*24*time.Hour), time.Time{}, 10, ""); err == nil {
			// Replace raw role identifiers (kg_extractor, judge, ...)
			// with operator-facing labels for display. The DB rows
			// stay raw — this is a render-side transform only.
			for i := range rows {
				rows[i].Role = displayRole(rows[i].Role)
			}
			data.TopModels = rows
		} else {
			s.logger.Warn().Err(err).Msg("failed to load model leaderboard for dashboard")
		}

		// Hourly buckets for the 24h sparkline. We bucket client-side
		// from the raw rows because the repo doesn't expose a bucketed
		// query. PageSize=5000 caps per-request cost — in busy
		// deployments older hours within the window may be undercounted,
		// which is acceptable for a sparkline (the headline number above
		// uses SumCost so it's still accurate).
		since24 := now.Add(-24 * time.Hour)
		usage, err := s.llmUsageRepo.List(ctx, persistence.TaskLLMUsageFilter{
			Since:    &since24,
			PageSize: 5000,
		})
		if err == nil {
			data.Spend.Sparkline, data.Spend.SparklineFilled = sparklinePoints(bucketHourly(usage, now), 100, 24)
		} else {
			s.logger.Warn().Err(err).Msg("failed to load hourly usage for sparkline")
		}
	}

	// Cache savings tile (Phase E). Tight 2s timeout — the landing
	// page must render even if the cache stats query stalls. Hides
	// itself when no hits yet so a fresh install isn't dominated by
	// a "$0.00 saved" placeholder.
	if s.responseCacheStats != nil {
		csCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		stats, err := s.responseCacheStats.CacheStats(csCtx)
		cancel()
		if err == nil && stats.TotalHits > 0 {
			data.CacheSavings = DashboardCacheSavings{
				Available:       true,
				TotalSavingsUSD: stats.TotalSavingsUSD,
				TotalHits:       stats.TotalHits,
			}
		}
	}

	// Active tasks tile: focused list of in-flight work. Selects
	// QUEUED / LEASED / RUNNING separately and concatenates so a
	// dashboard that shows mostly "queued, queued, running" reads
	// at a glance. Row cap is `limit` (?limit=N — same shared
	// validator as the rest of the UI; default 20). The legacy
	// fixed cap of 8 was raised to the audit-default 20 so the
	// landing page matches the rest of the list views.
	if s.taskRepo != nil {
		nowActive := time.Now().UTC()
		_ = nowActive
		active := make([]*persistence.Task, 0, limit)
		for _, status := range []persistence.TaskStatus{
			persistence.TaskStatusRunning,
			persistence.TaskStatusLeased,
			persistence.TaskStatusQueued,
		} {
			st := status
			rows, err := s.taskRepo.List(ctx, persistence.TaskFilter{
				Status:   &st,
				PageSize: limit,
			})
			if err != nil {
				continue
			}
			for _, t := range rows {
				active = append(active, t)
				if len(active) >= limit {
					break
				}
			}
			if len(active) >= limit {
				break
			}
		}
		data.ActiveTasks = active
		// Total counts come from TaskCounts (already populated above);
		// expose a single-int summary for the tile header.
		data.ActiveTaskCount = int(data.TaskCounts[persistence.TaskStatusRunning] +
			data.TaskCounts[persistence.TaskStatusLeased] +
			data.TaskCounts[persistence.TaskStatusQueued])
	}

	// Active chats tile.
	if s.activeChatSource != nil {
		data.ActiveChatCount = s.activeChatSource.ActiveChatCount()
	}

	// Operator queue (Upgrade #4 — 2026-05-26). Each card is best-
	// effort; the cards render only when their counts are non-zero,
	// so the queue rail collapses to a "Healthy" indicator when
	// nothing demands attention.
	data.OperatorQueue.AwaitingInput = data.TaskCounts[persistence.TaskStatusAwaitingInput]
	data.OperatorQueue.AwaitingApproval = data.TaskCounts[persistence.TaskStatusAwaitingApproval]
	data.OperatorQueue.StuckTotal = data.TaskCounts[persistence.TaskStatusWaitingForChildren]
	if s.taskRepo != nil && s.projectReg != nil {
		since := time.Now().Add(-24 * time.Hour)
		var failedSum int
		for _, p := range s.projectReg.ListProjects() {
			if p == nil {
				continue
			}
			n, ferr := s.taskRepo.CountRecentFailures(ctx, p.ID, nil, since)
			if ferr == nil {
				failedSum += n
			}
		}
		data.OperatorQueue.FailedRecently = int64(failedSum)
	}
	// Spend-spike heuristic: today's 24h > 2x the 7d-average daily
	// rate. Day7USD is the rolling-7-day spend already loaded above;
	// dividing by 7 gives the comparable daily average. Avoid the
	// false-positive case where Day7USD is near-zero (fresh install)
	// by requiring a $1+ floor on the comparison baseline.
	if data.Spend.Day7USD > 1.0 {
		avgDaily := data.Spend.Day7USD / 7.0
		if avgDaily > 0 {
			ratio := data.Spend.Day24hUSD / avgDaily
			if ratio >= 2.0 {
				data.OperatorQueue.SpendSpike = true
				data.OperatorQueue.SpendSpikeRatio = ratio
			}
		}
	}

	// Autonomy ETAs tile: per-project (last_eval_at + poll_interval)
	// projection. Only shows projects with autonomy enabled. Sorted
	// soonest-first so the operator sees the next thing about to
	// fire at the top.
	if s.projectReg != nil && s.autonomyEvalRepo != nil {
		nowETA := time.Now().UTC()
		var etas []AutonomyProjectETA
		for _, p := range s.projectReg.ListProjects() {
			if p == nil || !p.Autonomy.Enabled {
				continue
			}
			interval := 5 * time.Minute
			if p.Autonomy.PollInterval != "" {
				if d, perr := time.ParseDuration(p.Autonomy.PollInterval); perr == nil && d > 0 {
					interval = d
				}
			}
			pid := p.ID
			rows, err := s.autonomyEvalRepo.List(ctx, persistence.AutonomyEvaluationFilter{
				ProjectID: &pid,
				PageSize:  1,
			})
			eta := AutonomyProjectETA{
				ProjectID: pid,
				Interval:  interval,
			}
			if err == nil && len(rows) > 0 {
				last := rows[0]
				eta.LastEvalAt = last.CreatedAt
				eta.LastOutcome = last.Outcome
				eta.NextEvalAt = last.CreatedAt.Add(interval)
				eta.AgoLabel = humanizeAgo(nowETA.Sub(last.CreatedAt))
				eta.EtaLabel = humanizeEta(eta.NextEvalAt.Sub(nowETA))
				eta.NextEvalAtISO = eta.NextEvalAt.UTC().Format(time.RFC3339)
			} else {
				// No evaluation yet — the loop will tick within `interval`
				// of daemon start. We don't know start time here; show
				// the interval as the ETA so the operator sees "soon".
				eta.EtaLabel = "≤ " + humanizeAgo(interval)
			}
			etas = append(etas, eta)
		}
		// Stable sort by NextEvalAt ascending; zero-time (no eval yet)
		// floats to the bottom by convention.
		for i := 1; i < len(etas); i++ {
			for j := i; j > 0; j-- {
				prev := etas[j-1].NextEvalAt
				cur := etas[j].NextEvalAt
				prevZero := prev.IsZero()
				curZero := cur.IsZero()
				if curZero && !prevZero {
					break
				}
				if !curZero && (prevZero || cur.Before(prev)) {
					etas[j], etas[j-1] = etas[j-1], etas[j]
					continue
				}
				break
			}
		}
		data.AutonomyETAs = etas
	}

	// Memory pipeline tile (2026-05-12). Global KG snapshot + cross-
	// project ingest backlog — INSTANCE-WIDE (Stats has no project
	// filter), so gated to all-access callers; a project-scoped session
	// must not see cross-project totals (same leak fixed on /ui/memory).
	// Nil-safe — the template hides the tile when Enabled is false.
	if s.chunkGraph != nil && requestHasAllProjectAccess(r) {
		if stats, err := s.chunkGraph.Stats(ctx); err == nil && stats != nil {
			data.Memory.Enabled = true
			data.Memory.ChunksPending = stats.ChunksPending
			data.Memory.ChunksDone = stats.ChunksDone
			data.Memory.ChunksTotal = stats.ChunksPending + stats.ChunksDone
			if data.Memory.ChunksTotal > 0 {
				data.Memory.PercentDone = 100.0 * float64(stats.ChunksDone) / float64(data.Memory.ChunksTotal)
			}
			data.Memory.Entities = stats.Entities
			data.Memory.Edges = stats.Edges
			data.Memory.Mentions = stats.Mentions
		} else if err != nil {
			s.logger.Warn().Err(err).Msg("failed to load KG stats for dashboard")
		}
	}
	if s.ingestQueue != nil && s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			if p == nil {
				continue
			}
			if d, err := s.ingestQueue.QueueDepth(ctx, p.ID); err == nil && d > 0 {
				data.Memory.IngestBacklog += d
				data.Memory.IngestProjects++
			}
		}
	}

	// Trading safety tile (2026-05-12). Rolls per-project YAML (Mode /
	// KillSwitch) into a daemon-wide count + counts safety events
	// over the last 24h. Surfaces "any kill-switch tripped right now"
	// without the operator opening each project page.
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			if p == nil || p.Trading.Mode == "" {
				continue
			}
			data.Trading.EnabledCount++
			switch p.Trading.Mode {
			case "live":
				data.Trading.LiveCount++
			case "paper":
				data.Trading.PaperCount++
			}
			if p.Trading.KillSwitch {
				data.Trading.KillSwitchCount++
			}
		}
	}
	if s.tradingSafetyRepo != nil {
		since24 := time.Now().UTC().Add(-24 * time.Hour)
		if n, err := s.tradingSafetyRepo.Count(ctx, persistence.TradingSafetyEventFilter{
			Since: &since24,
		}); err == nil {
			data.Trading.SafetyEvents24h = n
		}
		breakerKind := persistence.TradingSafetyKindBreakerTrip
		if n, err := s.tradingSafetyRepo.Count(ctx, persistence.TradingSafetyEventFilter{
			Since: &since24,
			Kind:  &breakerKind,
		}); err == nil {
			data.Trading.BreakerTrips24h = n
		}
	}

	// Judge verdict tile (2026-05-12). Pulls the most recent 200
	// verdicts across all projects and counts pass/fail/abstain over
	// the last 24h. The repo doesn't expose a windowed Count, so we
	// filter the row slice in Go — cheap at 200 rows and avoids a
	// schema change for the dashboard.
	if s.judgeVerdictRepo != nil {
		data.Judge.Enabled = true
		data.Judge.WindowHours = 24
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		rows, err := s.judgeVerdictRepo.ListRecent(ctx, "", 200)
		if err == nil {
			for _, v := range rows {
				if v == nil || v.RecordedAt.Before(cutoff) {
					continue
				}
				data.Judge.Total++
				switch strings.ToLower(v.Verdict) {
				case "pass":
					data.Judge.Pass++
				case "fail":
					data.Judge.Fail++
				case "abstain":
					data.Judge.Abstain++
				}
			}
			if data.Judge.Total > 0 {
				data.Judge.PercentPass = 100.0 * float64(data.Judge.Pass) / float64(data.Judge.Total)
			}
		}
	}

	s.render(w, "dashboard.html", data)
}

// humanizeAgo formats a positive duration as "3m 24s ago"-style
// text. Negative or zero returns "just now". Pre-rendered handler-
// side because Go templates can't subtract times without a funcmap
// helper.
func humanizeAgo(d time.Duration) string {
	if d <= 0 {
		return "just now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
}

// humanizeEta formats a future-relative duration. Past or zero
// returns "due now"; positive values render as "in 1m 36s".
func humanizeEta(d time.Duration) string {
	if d <= 0 {
		return "due now"
	}
	if d < time.Minute {
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("in %dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("in %dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// bucketHourly groups usage rows into 24 hourly buckets ending at `now`.
// Index 0 is the bucket from 23..22 hours ago; index 23 is the most
// recent hour. Rows outside the window are silently dropped.
func bucketHourly(rows []*persistence.TaskLLMUsage, now time.Time) []float64 {
	out := make([]float64, 24)
	cutoff := now.Add(-24 * time.Hour)
	for _, r := range rows {
		if r == nil || r.RecordedAt.Before(cutoff) || r.RecordedAt.After(now) {
			continue
		}
		// Hour offset from now: 0 = current hour, 23 = oldest. Flip so
		// the slice reads left-to-right as time moves forward.
		ago := int(now.Sub(r.RecordedAt).Hours())
		if ago < 0 || ago >= 24 {
			continue
		}
		out[23-ago] += r.CostUSD
	}
	return out
}

// sparklinePoints scales a value series to fit a width × height SVG
// viewBox and returns two SVG `points` strings:
//
//   - line: the polyline through the data points
//   - filled: the same path closed at the bottom corners, for an area fill
//
// Returns "", "" when there's nothing to draw (empty series, single point,
// or all zeros). Computing this in Go avoids fragile template arithmetic.
func sparklinePoints(values []float64, width, height int) (line, filled string) {
	if len(values) < 2 {
		return "", ""
	}
	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return "", ""
	}
	step := float64(width) / float64(len(values)-1)
	var lb, fb strings.Builder
	for i, v := range values {
		x := float64(i) * step
		y := float64(height) - (v/max)*float64(height)
		if i > 0 {
			lb.WriteByte(' ')
			fb.WriteByte(' ')
		}
		fmt.Fprintf(&lb, "%.1f,%.1f", x, y)
		fmt.Fprintf(&fb, "%.1f,%.1f", x, y)
	}
	// Close the filled-area polyline along the baseline so it paints as
	// a filled region instead of a stroke.
	fmt.Fprintf(&fb, " %d,%d 0,%d", width, height, height)
	return lb.String(), fb.String()
}
