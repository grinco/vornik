// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// ProjectDetailData holds data for the project detail template.
type ProjectDetailData struct {
	Title       string
	CurrentPage string
	Project     *registry.Project
	Swarm       *registry.Swarm
	Workflow    *registry.Workflow
	Tasks       []*persistence.Task
	TaskCounts  map[persistence.TaskStatus]int64
	// RoleQuality is per-role output quality stats over the last 30 days,
	// keyed by role name. Missing entries mean "no data for that role in
	// the window" — the template shows "no runs yet".
	RoleQuality map[string]*persistence.RoleQuality
	// Spend summarises LLM cost for this project. Zero values mean "no
	// data" — either the project hasn't run anything billable yet, or the
	// llm_usage repo isn't wired (deployment predates 2026.4.11).
	Spend ProjectSpendSummary
	// SpendSort/SpendDir/SpendBaseURL drive the sortable header on the
	// per-role spend table. Clicking a column flips dir; clicking another
	// switches keys with the table's default direction.
	SpendSort    string
	SpendDir     string
	SpendBaseURL string
	// Drift is the per-(role, model) effective-cost drift summary
	// scoped to this project. Each row is the 24h cost-per-success
	// against the 7-day baseline; >2× is a regression worth
	// investigating, <0.7× is an improvement worth keeping. Empty
	// when the project has no successful steps in the baseline
	// window yet (typical for fresh projects).
	Drift []DriftPanelRow
	// DriftRegressionCount + DriftImprovementCount drive the panel
	// header summary so an operator scanning the project list sees
	// at-a-glance whether something needs attention.
	DriftRegressionCount  int
	DriftImprovementCount int
	// Hallucination is the per-project rollup of Phase 1 signals
	// (rule-based claim grounding) + Phase 3 LLM-as-judge
	// verdicts plus the configured layer state. Always populated;
	// when the project has no signals/verdicts, the template
	// renders the panel with the config state and zero counts so
	// the operator sees "the layer IS active" vs "nothing's
	// happened yet".
	Hallucination ProjectHallucinationSummary
	// Trading is populated only when the project has a `broker`
	// MCP server configured. Always present on the struct (zero
	// value when not enabled) so the template doesn't have to
	// nil-check; the panel itself gates on Trading.Enabled.
	Trading TradingPanel
	// TaskLimit / TaskLimitOptions / TaskTotal drive the task-
	// list page-size selector. TaskLimit is what's currently
	// rendered (validated against TaskLimitOptions; default 20);
	// TaskTotal is the project-wide task count so an operator
	// can see "showing 20 of 312" rather than just "20".
	TaskLimit        int
	TaskLimitOptions []int
	TaskTotal        int64
	// Per-project homepage autonomy header (2026.6.0 SaaS-readiness,
	// F4 → post-cleanup). Populated when the project has autonomy
	// enabled AND an eval row exists. Rendered inside the Tasks
	// panel's header bar so the operator question "when does
	// something next happen" sits with the task list.
	NextAutonomyTickAt   time.Time
	NextAutonomyTickETA  string // pre-rendered "in 1m 36s" / "due now"
	NextAutonomyTickISO  string // RFC3339 for the client-side ticker
	LastAutonomyOutcome  string // most-recent eval outcome string
	LastAutonomyAgoLabel string // "3m 24s ago" — pre-rendered

	// Retention is the effective (resolved) per-project pruning
	// window — defaults from daemon config + project YAML overrides
	// applied. 2026.7.0 F9 surface: operators want to see "how
	// long do I keep cost data / audit / artifacts" without
	// reading the YAML. Always populated (zero-value when the
	// policy resolver isn't wired).
	Retention RetentionPanel

	// RetentionPreview is the dry-run estimate of what the next
	// retention sweep would prune. Populated when the daemon has
	// wired a RetentionPreviewer; Available=false means the
	// estimate hasn't been computed (no previewer wired or the
	// probe errored). Operators read this AFTER the Retention
	// panel — same place where they tune the windows, so the
	// estimate sits next to the lever that moves it.
	RetentionPreview          RetentionPreviewCounts
	RetentionPreviewAvailable bool
	RetentionPreviewError     string

	// RateLimits surfaces the per-API-key throttle configuration
	// for active keys on this project. Empty slice when the
	// project has no rate-limited keys (or no keys at all). Pairs
	// with the vornik_ratelimit_decisions_total Prometheus counter
	// — operators check the panel to confirm a key's nominal
	// limit, then read the counter for live decision outcomes.
	RateLimits []RateLimitKeyRow

	// RateLimitWarning drives the homepage "approaching limit"
	// banner. Empty (Active=false) means the banner is hidden.
	// Populated from the shared ratelimit.Metrics ring + the
	// per-project limiter snapshot.
	RateLimitWarning RateLimitWarning

	// Reminders — upcoming pending reminders for this project,
	// rendered in a small tile on the project page. Empty slice
	// when none / unwired.
	Reminders []ReminderTileRow

	// Archival lifecycle — populated from Project.Lifecycle so
	// the template can render the archived banner + countdown
	// without recomputing on every render. Active projects keep
	// every field at its zero value (template renders the
	// "Danger zone" panel instead of the banner).
	Lifecycle ProjectLifecyclePanel
	// FlashArchived / FlashUnarchived show a one-shot success
	// toast above the page after the archive / unarchive POST
	// redirects back. Driven by the ?archived=1 / ?unarchived=1
	// query params.
	FlashArchived   bool
	FlashUnarchived bool

	// GitAccess is populated when the project has Git.Enabled=true.
	// When Git.Enabled is false the panel is hidden and this is the
	// zero value. Derived at render time — the clone URL is never stored.
	GitAccess GitAccessPanel
}

// GitAccessPanel holds the pre-rendered data for the Git-access panel on
// the project-detail page. The panel is rendered in BOTH states (so the
// feature is discoverable when off); Enabled selects which arm shows.
type GitAccessPanel struct {
	// Show gates whether the panel container renders at all. The handler
	// always sets it true; it exists so direct-data render tests can opt
	// the panel in/out without it appearing on every unrelated page test.
	Show bool
	// Enabled mirrors project.Git.Enabled — true selects the enabled arm
	// (clone URL + Disable control), false the disabled arm (Enable control).
	Enabled bool
	// CloneURL is the full git clone command when publicBaseURL is set, e.g.
	// "https://<key-name>@vornik.example.com/api/v1/git/<projectID>.git".
	// When publicBaseURL is empty this is empty; RelativePath is set instead.
	CloneURL string
	// RelativePath is the path portion of the clone URL, always set:
	// "/api/v1/git/<projectID>.git".
	RelativePath string
	// BaseURLMissing is true when the operator hasn't set
	// server.public_base_url, meaning we can only show the relative path.
	BaseURLMissing bool
	// IsForgeBacked is true when the project has a forge provider
	// configured (Forge.Provider != "" or legacy GitHub creds). Drives
	// the forge-specific caveat in the panel.
	IsForgeBacked bool
	// AuthDisabled is true when the daemon is running with auth off
	// (auth_enabled=false). Drives the "anyone can clone/push" warning.
	AuthDisabled bool
}

// ProjectLifecyclePanel is the template-friendly view of
// registry.ProjectLifecycle. All counts pre-formatted so the
// template stays declarative — the banner just renders strings.
type ProjectLifecyclePanel struct {
	// IsArchived gates whether the banner appears (true) or the
	// Danger zone panel (false).
	IsArchived bool
	// ArchivedAt / ScheduledDeleteAt are pre-formatted display
	// timestamps. ISO RFC3339 ones live in the JS-friendly
	// fields below.
	ArchivedAt        string
	ScheduledDeleteAt string
	// ScheduledDeleteAtISO is the raw RFC3339 string for any
	// client-side ticker that wants to recompute "in 3d 5h"
	// without round-tripping.
	ScheduledDeleteAtISO string
	// Countdown is pre-rendered "in 6d 14h" / "due now" so the
	// banner doesn't need its own ticker JS.
	Countdown string
	// Overdue is true when ScheduledDeleteAt is in the past
	// (sweeper hasn't picked it up yet, or sweeper isn't wired).
	Overdue    bool
	Reason     string
	ArchivedBy string
	// GracePresets feeds the archive form's "grace period"
	// dropdown — fixed list so operators don't have to type a
	// duration.
	GracePresets []ArchiveGracePreset
}

// RateLimitKeyRow is one active API key's nominal throttle. nil
// rps/burst on the persisted row renders as "unlimited" — operators
// can scan the panel and immediately see which keys WOULD throttle.
type RateLimitKeyRow struct {
	KeyID          string // internal only; metrics lookup key, not rendered
	Name           string // operator-supplied label from APIKey.Name
	KeyPrefix      string // first few chars of the key (e.g. "sk-vornik-foo.ab12")
	RateLimitRPS   int    // 0 means "no limit configured"
	RateLimitBurst int    // 0 means "no limit configured"
}

// RateLimitWarning drives the project-homepage "approaching limit"
// banner. Active is the truthiness gate the template uses; zero
// value renders nothing. Populated from the shared
// ratelimit.Metrics ring + the per-project limiter snapshot so the
// banner stays consistent with the
// /api/v1/projects/{id}/ratelimit-status endpoint.
type RateLimitWarning struct {
	// Active is true when there is at least one warn-or-block event
	// in the trailing StatusWindow across the project scope or any
	// of its active API keys.
	Active bool
	// RecentWarns is the count of warn events (≥80% bucket consumed
	// OR over-cap task creates) over the trailing StatusWindow.
	RecentWarns int
	// RecentBlocks is the count of 429-generating events over the
	// trailing StatusWindow. The banner CTA is stronger when this
	// is non-zero: degradation has already crossed into refusal.
	RecentBlocks int
	// LastBlockAt is the most recent 429 timestamp inside the
	// window, pre-rendered as "3m 24s ago" for the template.
	LastBlockAt string
	// WindowLabel is "5m" — the pre-rendered window duration so the
	// template stays dumb.
	WindowLabel string
}

// RetentionPanel mirrors retention.Policy in shape, plus a
// per-field flag indicating whether the value came from the
// per-project YAML override (true) or the daemon-wide default
// (false). Lets the template render "override" badges on
// fields the operator has tuned vs the inherited defaults.
type RetentionPanel struct {
	TaskLLMUsageDays       int
	ToolAuditDays          int
	TasksDays              int
	ExecutionsDays         int
	ArtifactsDays          int
	TaskLLMUsageIsOverride bool
	ToolAuditIsOverride    bool
	TasksIsOverride        bool
	ExecutionsIsOverride   bool
	ArtifactsIsOverride    bool
}

// ProjectHallucinationSummary is the per-project hallucination
// panel's payload. Mirrors the shape of the Spend / Drift
// panels: a few headline numbers + a small recent-rows list.
type ProjectHallucinationSummary struct {
	// JudgeEnabled / JudgeModel come from the project YAML.
	// JudgeEnabled is the only signal an operator needs to see
	// "the layer is wired"; JudgeModel surfaces the chosen
	// model so a quick glance catches misconfigurations.
	JudgeEnabled bool
	JudgeModel   string
	// VerifierCount is the number of declarative Phase 2
	// verifiers configured for this project. Zero means
	// "nothing declared"; non-zero gates step success on those
	// invariants.
	VerifierCount int
	// Phase 1 signal counts over the rolling window
	// (HallucinationWindow). Grouped by severity so the panel
	// header can render "5 high / 12 warn" without the template
	// summing the slice.
	SignalsHigh int
	SignalsWarn int
	SignalsInfo int
	// SignalsByDetector groups the same signals by rule name so
	// the operator sees "url_not_fetched fired 8× this week" —
	// the typical drill-down after spotting a count spike.
	SignalsByDetector map[string]int
	// Phase 3 verdict distribution over the same window.
	VerdictsPass    int
	VerdictsFail    int
	VerdictsAbstain int
	// LatestVerdicts is the N most recent verdicts (newest
	// first) for inline display. Capped at 5 so the panel
	// stays scannable; full history lives on the task detail
	// pages.
	LatestVerdicts []ProjectVerdictRow
	// VerdictTotalCostUSD is total $ spent on judge calls over
	// the window — answers "what's the layer costing me?"
	// without requiring the operator to drill into spend.
	VerdictTotalCostUSD float64
	// HasRecentOutcomes is true when the project produced at
	// least one step outcome in the panel window. Drives the
	// "no verdicts" empty state copy: with outcomes present
	// (i.e. tasks DID run) but zero verdicts, the message
	// should suggest "judge may be misconfigured / failing"
	// rather than the default "wait a minute".
	HasRecentOutcomes bool
}

// ReminderTileRow is the template-friendly view of one upcoming
// reminder row. Pre-formatted fire time + countdown so the
// template stays declarative.
type ReminderTileRow struct {
	ID         string
	Content    string
	FireAt     string // "2026-05-24 09:00:00 MST"
	Countdown  string // "in 6h 12m" / "due now"
	OperatorID string
}

// buildProjectLifecyclePanel renders the template-friendly view
// of a project's lifecycle state. Always returns a populated
// struct — GracePresets is always set so the Danger-zone form
// has its dropdown options even on a brand-new active project.
func buildProjectLifecyclePanel(p *registry.Project) ProjectLifecyclePanel {
	panel := ProjectLifecyclePanel{
		GracePresets: archiveGracePresets,
	}
	if p == nil || !p.IsArchived() {
		return panel
	}
	panel.IsArchived = true
	panel.Reason = p.Lifecycle.Reason
	panel.ArchivedBy = p.Lifecycle.ArchivedBy

	now := time.Now()
	if t, ok := p.ArchivedAtTime(); ok {
		panel.ArchivedAt = t.Format("2006-01-02 15:04:05 MST")
	}
	if t, ok := p.ScheduledDeletion(); ok {
		panel.ScheduledDeleteAt = t.Format("2006-01-02 15:04:05 MST")
		panel.ScheduledDeleteAtISO = t.Format(time.RFC3339)
		remaining := t.Sub(now)
		if remaining <= 0 {
			panel.Overdue = true
			panel.Countdown = "due now (sweeper will pick up on next tick)"
		} else {
			panel.Countdown = "in " + humanDuration(remaining)
		}
	}
	return panel
}

// ProjectVerdictRow projects a TaskJudgeVerdict for inline
// rendering on the project page. Pre-formats VerdictClass so
// the template stays dumb, and truncates Summary so a
// long-winded judge summary doesn't blow the panel layout.
type ProjectVerdictRow struct {
	TaskID       string
	Verdict      string
	VerdictClass string
	Confidence   float64
	Summary      string
	Model        string
	RecordedAt   time.Time
}

// hallucinationPanelWindow is the rolling lookback for the
// project hallucination summary. 7 days mirrors the drift
// panel's baseline so operators reading both at once aren't
// comparing different windows.
const hallucinationPanelWindow = 7 * 24 * time.Hour

// DriftPanelRow is one row in the per-project drift panel.
// Mirrors budget.DriftRow but with formatted display fields and
// a per-row insight string the template surfaces directly.
type DriftPanelRow struct {
	Role             string
	Model            string
	CurrentSpendUSD  float64
	CurrentOks       int64
	CurrentUSDPerOk  float64
	BaselineUSDPerOk float64
	Ratio            float64
	HasBaseline      bool
	// Insight is a one-line operator-friendly diagnosis the panel
	// renders next to the ratio. Empty when no notable signal —
	// "1.05× over baseline" doesn't get an insight line.
	Insight string
}

// ProjectSpendSummary is the project-detail UI's cost panel payload.
type ProjectSpendSummary struct {
	Day24hUSD            float64
	Day7USD              float64
	Day30USD             float64
	ByRole24h            []RoleSpendRow
	BudgetDailyHardUSD   float64
	BudgetDailySoftUSD   float64
	BudgetMonthlyHardUSD float64
	BudgetMonthlySoftUSD float64
	MonthToDateUSD       float64
	// Source breakdown over the 24h window. Splits this project's
	// spend into "workflow_step" (executor-driven task work) vs
	// "dispatcher" (chat overhead landing on this project as the
	// active chat). High dispatcher share on a project with no
	// automation = chat-pinning issue; pin the dispatcher to a
	// dedicated assistant project via telegram.dispatcher_project_id.
	WorkflowUSD24h   float64
	DispatcherUSD24h float64
	// DispatcherShareOfTotal is dispatcher / (dispatcher + workflow),
	// 0..1. The template uses this to color the dispatcher line
	// red when chat is dominating cost on a no-task project.
	DispatcherShareOfTotal float64
}

// RoleSpendRow is one row in the "spend by role" breakdown.
//
// LastSeen lets the UI distinguish a currently-active model from a
// legacy one that's still inside the 24h window because it was used
// earlier today — without dropping the historical cost totals. The
// panel sorts by CostUSD desc so the biggest spenders surface first.
type RoleSpendRow struct {
	Role     string
	Model    string
	CostUSD  float64
	LastSeen time.Time
}

// roleSpendColumns maps each sort key to a comparator. sortBy handles
// direction and fallback. Default key is "cost" — biggest spenders first.
var roleSpendColumns = map[string]func(a, b RoleSpendRow) int{
	"role":  func(a, b RoleSpendRow) int { return cmp.Compare(a.Role, b.Role) },
	"model": func(a, b RoleSpendRow) int { return cmp.Compare(a.Model, b.Model) },
	"cost":  func(a, b RoleSpendRow) int { return cmp.Compare(a.CostUSD, b.CostUSD) },
	"last":  func(a, b RoleSpendRow) int { return a.LastSeen.Compare(b.LastSeen) },
}

// ProjectDetail renders a single project detail page.
func (s *Server) ProjectDetail(w http.ResponseWriter, r *http.Request) {
	// Extract project ID from path (remove "/projects/" prefix)
	projectID := r.URL.Path[len("/projects/"):]
	s.logger.Debug().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("project_id", projectID).
		Msg("rendering project detail")

	if projectID == "" || projectID == "list" {
		s.Projects(w, r)
		return
	}

	var project *registry.Project
	if s.projectReg != nil {
		project = s.projectReg.GetProject(projectID)
	}

	if project == nil {
		s.logger.Warn().Str("project_id", projectID).Msg("project not found for UI")
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "project_not_found.html", struct {
			Title       string
			CurrentPage string
			ProjectID   string
		}{
			Title:       "Project Not Found",
			CurrentPage: "projects",
			ProjectID:   projectID,
		})
		return
	}

	spendSort, spendDir := sortParams(r, []string{"role", "model", "cost", "last"}, "cost", "desc")
	data := ProjectDetailData{
		Title:           "Project: " + projectID,
		CurrentPage:     "projects",
		Project:         project,
		TaskCounts:      make(map[persistence.TaskStatus]int64),
		SpendSort:       spendSort,
		SpendDir:        spendDir,
		SpendBaseURL:    sortBaseURL(r),
		Lifecycle:       buildProjectLifecyclePanel(project),
		FlashArchived:   r.URL.Query().Get("archived") == "1",
		FlashUnarchived: r.URL.Query().Get("unarchived") == "1",
	}

	// Fetch related swarm and workflow definitions
	if s.projectReg != nil {
		data.Swarm = s.projectReg.GetSwarm(project.SwarmID)
		data.Workflow = s.projectReg.GetWorkflow(project.DefaultWorkflowID)
	}

	// Trading panel — populated when the project has a `broker`
	// MCP server. Bounded by a 3s fetch timeout inside the
	// builder so an unreachable broker doesn't slow the page.
	data.Trading = s.buildTradingPanel(r.Context(), project)

	// Upcoming reminders tile — pending rows scoped to this
	// project, ordered by fire time. Best-effort; the page still
	// renders if the lookup errors.
	if s.reminderRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		rows, err := s.reminderRepo.List(ctx, persistence.ReminderListFilter{
			ProjectID: projectID,
			Status:    persistence.ReminderStatusPending,
			PageSize:  10,
		})
		cancel()
		if err == nil {
			now := time.Now()
			for _, rem := range rows {
				if rem == nil {
					continue
				}
				row := ReminderTileRow{
					ID:         rem.ID,
					Content:    rem.Content,
					FireAt:     rem.FireAt.Local().Format("2006-01-02 15:04:05 MST"),
					OperatorID: rem.OperatorID,
				}
				remaining := rem.FireAt.Sub(now)
				if remaining <= 0 {
					row.Countdown = "due now"
				} else {
					row.Countdown = "in " + humanDuration(remaining)
				}
				data.Reminders = append(data.Reminders, row)
			}
		} else {
			s.logger.Warn().Err(err).Str("project_id", projectID).Msg("project detail: reminders list failed")
		}
	}

	// Get tasks for this project. Page size comes from ?limit=N
	// (shared validator — see page_size.go) so an operator can
	// tighten the view to 10 rows for a fast scan or expand to 100
	// for a deep dive without leaving the page. Default 20 matches
	// the audit page (the canonical source of the UX pattern).
	data.TaskLimitOptions = PageSizeOptions
	data.TaskLimit = parsePageSize(r.URL.Query().Get("limit"))
	if s.taskRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		tasks, err := s.taskRepo.List(ctx, persistence.TaskFilter{
			ProjectID: &projectID,
			PageSize:  data.TaskLimit,
		})
		if err == nil {
			data.Tasks = tasks
		} else {
			s.logger.Warn().Err(err).Str("project_id", projectID).Msg("failed to load project tasks for UI")
		}

		counts, err := s.taskRepo.CountByStatus(ctx, projectID)
		if err == nil {
			data.TaskCounts = counts
			// Sum the per-status counts to surface "showing N
			// of total" — Count(filter) would do this in one
			// query but the per-status counts are already loaded
			// for the status-tile row above, so reuse them.
			for _, n := range counts {
				data.TaskTotal += n
			}
		} else {
			s.logger.Warn().Err(err).Str("project_id", projectID).Msg("failed to load project task counts for UI")
		}
	}

	// Per-project homepage autonomy header (2026.6.0 F4 → post-cleanup).
	// The "next tick" countdown + last-outcome label live in the
	// Tasks panel header (see template). The standalone activity
	// strip was removed — the Tasks list below carries per-row
	// origin badges that distinguish autonomy / manual / delegated
	// creates, which subsumes what the activity strip showed.
	s.populateProjectHomepageAutonomy(r.Context(), project, projectID, &data)

	// Retention panel — applies the daemon defaults + project
	// overrides so the operator can see what's kept and for how
	// long. Pure-Go resolution; no DB / IO cost on the page.
	data.Retention = buildRetentionPanel(project, s.retentionDefaults)

	// Retention preview — what the next sweep WOULD prune. Bound
	// at a short timeout because the COUNT queries hit per-table
	// indexes; on a very large DB the preview can take seconds.
	// Best-effort: a failed probe surfaces the error inline but
	// doesn't block the rest of the page.
	if s.retentionPreviewer != nil {
		previewCtx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		preview, err := s.retentionPreviewer.Preview(previewCtx, projectID)
		cancel()
		if err == nil {
			data.RetentionPreview = preview
			data.RetentionPreviewAvailable = true
		} else {
			data.RetentionPreviewError = err.Error()
		}
	}

	// Rate-limit panel — surfaces the nominal throttle config
	// per active API key for this project. Pairs with the
	// /metrics vornik_ratelimit_decisions_total counter for live
	// outcome data. Best-effort — repo error or no keys leaves
	// the slice empty and the template skips the panel.
	if s.apiKeyRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		data.RateLimits = buildRateLimitPanel(ctx, s.apiKeyRepo, projectID, s.logger)
	}

	// Rate-limit warning banner. Lights up when the trailing
	// StatusWindow shows at least one warn or block event on the
	// project scope or on any of its API keys. Aggregates over
	// (project, key1, key2, …) so a homepage operator sees one
	// banner per project regardless of which key is degrading.
	data.RateLimitWarning = buildRateLimitWarning(
		s.rateLimitMetrics, data.RateLimits, projectID, time.Now())

	// Git-access panel — always rendered so the feature is discoverable in
	// both states (the disabled arm offers an Enable control). The clone URL
	// is DERIVED at render time from publicBaseURL; it is never stored.
	// Forge-backed and auth-off states are detected here so the template
	// stays purely declarative.
	data.GitAccess = buildGitAccessPanel(project, projectID, s.publicBaseURL, r)

	// Per-role quality stats over the last 30 days. Shown in the swarm
	// section as success-rate / run-count / avg-duration badges on each
	// role card. Best-effort — if the query fails or the repository isn't
	// wired, the section degrades to "no data".
	if s.execRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rq, err := s.execRepo.GetRoleQuality(ctx, projectID, 30*24*time.Hour)
		if err == nil {
			data.RoleQuality = rq
		} else {
			s.logger.Warn().Err(err).Str("project_id", projectID).Msg("failed to load per-role quality stats for UI")
		}
	}

	// LLM spend summary: 24h / 7d / 30d rolling + month-to-date + per-role
	// 24h breakdown. Budget fields come from project YAML; zero means "no
	// cap". Best-effort — if the repo isn't wired, the panel shows zeros.
	if s.llmUsageRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		now := time.Now().UTC()
		windows := []struct {
			label string
			since time.Time
		}{
			{"24h", now.Add(-24 * time.Hour)},
			{"7d", now.Add(-7 * 24 * time.Hour)},
			{"30d", now.Add(-30 * 24 * time.Hour)},
		}
		for _, w := range windows {
			total, err := s.llmUsageRepo.SumCostByProject(ctx, projectID, w.since, time.Time{})
			if err != nil {
				s.logger.Warn().Err(err).Str("window", w.label).Msg("llm usage: sum failed")
				continue
			}
			switch w.label {
			case "24h":
				data.Spend.Day24hUSD = total
			case "7d":
				data.Spend.Day7USD = total
			case "30d":
				data.Spend.Day30USD = total
			}
		}
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if mtd, err := s.llmUsageRepo.SumCostByProject(ctx, projectID, monthStart, time.Time{}); err == nil {
			data.Spend.MonthToDateUSD = mtd
		}

		// Per-role breakdown over the 24h window. Aggregate rows grouped
		// by (role, model). Keep the list bounded — the dispatcher's
		// swarm catalogues rarely exceed a dozen role+model combos, so a
		// cap is precautionary, not a real scaling concern.
		rows, err := s.llmUsageRepo.List(ctx, persistence.TaskLLMUsageFilter{
			ProjectID: &projectID,
			Since:     &windows[0].since,
			PageSize:  5000,
		})
		if err == nil {
			agg := make(map[string]*RoleSpendRow)
			for _, row := range rows {
				key := row.Role + "\x00" + row.Model
				if _, ok := agg[key]; !ok {
					agg[key] = &RoleSpendRow{Role: displayRole(row.Role), Model: row.Model}
				}
				agg[key].CostUSD += row.CostUSD
				if row.RecordedAt.After(agg[key].LastSeen) {
					agg[key].LastSeen = row.RecordedAt
				}
				// Source attribution piggybacks on the same scan.
				// "dispatcher" rows are chat overhead landing on
				// this project; "workflow_step" rows are real
				// task work. An empty source string is rare
				// (legacy rows pre-2026.4.x) — bucket them
				// alongside workflow_step since that's what they
				// would have been tagged as today.
				switch row.Source {
				case persistence.TaskLLMUsageSourceDispatcher:
					data.Spend.DispatcherUSD24h += row.CostUSD
				default:
					data.Spend.WorkflowUSD24h += row.CostUSD
				}
			}
			if total := data.Spend.WorkflowUSD24h + data.Spend.DispatcherUSD24h; total > 0 {
				data.Spend.DispatcherShareOfTotal = data.Spend.DispatcherUSD24h / total
			}
			out := make([]RoleSpendRow, 0, len(agg))
			for _, v := range agg {
				out = append(out, *v)
			}
			sortBy(out, roleSpendColumns, data.SpendSort, data.SpendDir, "cost")
			data.Spend.ByRole24h = out
		} else {
			s.logger.Warn().Err(err).Msg("llm usage: per-role 24h list failed")
		}
	}
	// Budget caps come from the YAML config regardless of repo wiring.
	if project != nil {
		data.Spend.BudgetDailySoftUSD = project.Budget.DailySoftUSD
		data.Spend.BudgetDailyHardUSD = project.Budget.DailyHardUSD
		data.Spend.BudgetMonthlySoftUSD = project.Budget.MonthlySoftUSD
		data.Spend.BudgetMonthlyHardUSD = project.Budget.MonthlyHardUSD
	}

	// Effective-cost drift panel — replaces the Telegram alert that
	// used to fire at >2× ratio. Surfaces a per-(role, model)
	// regression/improvement table so operators can see at a glance
	// which combos are getting worse.
	if s.llmUsageRepo != nil && s.outcomeRepo != nil {
		drift, derr := budget.ComputeDrift(r.Context(), s.llmUsageRepo, s.outcomeRepo, projectID, budget.DefaultDriftConfig(), time.Now().UTC())
		if derr != nil {
			s.logger.Warn().Err(derr).Str("project_id", projectID).Msg("project detail: drift compute failed")
		} else {
			data.Drift = make([]DriftPanelRow, 0, len(drift))
			for _, d := range drift {
				row := DriftPanelRow{
					Role:             d.Role,
					Model:            d.Model,
					CurrentSpendUSD:  d.CurrentSpendUSD,
					CurrentOks:       d.CurrentOks,
					CurrentUSDPerOk:  d.CurrentUSDPerOk,
					BaselineUSDPerOk: d.BaselineUSDPerOk,
					Ratio:            d.Ratio,
					HasBaseline:      d.HasBaseline,
				}
				if d.HasBaseline {
					row.Insight = driftInsight(d)
					if d.Ratio > 2.0 {
						data.DriftRegressionCount++
					} else if d.Ratio < 0.7 {
						data.DriftImprovementCount++
					}
				}
				data.Drift = append(data.Drift, row)
			}
			// Sort regressions first (descending ratio), then
			// no-baseline rows by current spend desc.
			sort.SliceStable(data.Drift, func(i, j int) bool {
				if data.Drift[i].HasBaseline != data.Drift[j].HasBaseline {
					return data.Drift[i].HasBaseline
				}
				if data.Drift[i].HasBaseline {
					return data.Drift[i].Ratio > data.Drift[j].Ratio
				}
				return data.Drift[i].CurrentSpendUSD > data.Drift[j].CurrentSpendUSD
			})
		}
	}

	// Hallucination panel: configured layer state + rolling
	// Phase 1 signal counts + Phase 3 verdict distribution.
	// Always populated so the operator sees "judge model X is
	// configured, no verdicts yet" rather than missing the panel
	// entirely on a fresh project.
	data.Hallucination = s.computeHallucinationSummary(r.Context(), projectID, project)

	// CSV / JSON export of the per-role spend table. Triggered by
	// ?format=csv|json on the project detail URL — operators can drop
	// the breakdown into a spreadsheet without an extra API path.
	switch exportFormat(r) {
	case "csv":
		header := []string{"role", "model", "cost_usd", "last_seen"}
		out := [][]string{header}
		for _, r := range data.Spend.ByRole24h {
			out = append(out, []string{
				r.Role, r.Model,
				strconv.FormatFloat(r.CostUSD, 'f', 6, 64),
				r.LastSeen.UTC().Format(time.RFC3339),
			})
		}
		writeCSV(w, "spend-"+projectID+".csv", out)
		return
	case "json":
		writeJSON(w, "spend-"+projectID+".json", map[string]any{
			"project_id":    projectID,
			"day_24h_usd":   data.Spend.Day24hUSD,
			"day_7_usd":     data.Spend.Day7USD,
			"day_30_usd":    data.Spend.Day30USD,
			"month_to_date": data.Spend.MonthToDateUSD,
			"by_role_24h":   data.Spend.ByRole24h,
		})
		return
	}

	s.render(w, "project_detail.html", data)
}

// driftInsight produces a one-line operator-friendly diagnosis for
// a drift row. Empty when the ratio is in the boring band
// (0.85–1.5×). Insights pair the ratio with a likely cause based
// on whether the change is driven by current spend or by current
// success rate.
func driftInsight(d budget.DriftRow) string {
	if !d.HasBaseline {
		return ""
	}
	switch {
	case d.Ratio > 2.0:
		// Regression. Compare success-rate vs spend-rate movement
		// to suggest the likely lever.
		spendRatio := 0.0
		oksRatio := 0.0
		baselineSpendPerDay := d.BaselineSpendUSD / 7.0
		if baselineSpendPerDay > 0 {
			spendRatio = d.CurrentSpendUSD / baselineSpendPerDay
		}
		baselineOksPerDay := float64(d.BaselineOks) / 7.0
		if baselineOksPerDay > 0 {
			oksRatio = float64(d.CurrentOks) / baselineOksPerDay
		}
		if oksRatio > 0 && oksRatio < 0.6 {
			return fmt.Sprintf("Cost/success up %.1f× — successes dropped to %.0f%% of baseline; investigate failures, not spend.", d.Ratio, oksRatio*100)
		}
		if spendRatio > 1.5 {
			return fmt.Sprintf("Cost/success up %.1f× — daily spend at %.1f× baseline; check for prompt bloat or model upgrade.", d.Ratio, spendRatio)
		}
		return fmt.Sprintf("Cost/success up %.1f× vs the 7-day baseline.", d.Ratio)
	case d.Ratio > 1.5:
		return fmt.Sprintf("Cost/success up %.1f× — watch this combo; another bad day flips it to alert.", d.Ratio)
	case d.Ratio < 0.7:
		return fmt.Sprintf("Cost/success down %.0f%% — this combo is becoming more efficient.", (1.0-d.Ratio)*100)
	}
	return ""
}

// computeHallucinationSummary builds the per-project rollup for
// the Hallucination panel. Reads from three sources:
//
//   - registry.Project for layer config (JudgeEnabled, model,
//     verifier count).
//   - judgeVerdictRepo.ListRecent for Phase 3 verdict
//     distribution + latest rows.
//   - outcomeRepo.List for Phase 1 signal counts (walk client-
//     side; bounded by PageSize so this doesn't fan out into
//     unbounded scans on long-lived projects).
//
// Best-effort: any repo error degrades to an empty section
// rather than blocking the page, with a warn log line.
func (s *Server) computeHallucinationSummary(ctx context.Context, projectID string, project *registry.Project) ProjectHallucinationSummary {
	summary := ProjectHallucinationSummary{
		SignalsByDetector: map[string]int{},
	}
	if project != nil {
		summary.JudgeEnabled = project.HallucinationJudge.Enabled
		summary.JudgeModel = project.HallucinationJudge.Model
		summary.VerifierCount = len(project.Verifiers)
	}

	since := time.Now().UTC().Add(-hallucinationPanelWindow)

	// Phase 3 verdicts: pull the recent slice and count by
	// decision. ListRecent is newest-first, so the first 5 are
	// also the LatestVerdicts table.
	if s.judgeVerdictRepo != nil {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		verdicts, err := s.judgeVerdictRepo.ListRecent(rctx, projectID, 200)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", projectID).
				Msg("hallucination panel: verdict list failed")
		} else {
			for _, v := range verdicts {
				if v == nil || v.RecordedAt.Before(since) {
					continue
				}
				switch v.Verdict {
				case persistence.JudgeVerdictPass:
					summary.VerdictsPass++
				case persistence.JudgeVerdictFail:
					summary.VerdictsFail++
				case persistence.JudgeVerdictAbstain:
					summary.VerdictsAbstain++
				}
				summary.VerdictTotalCostUSD += v.CostUSD
				if len(summary.LatestVerdicts) < 5 {
					summary.LatestVerdicts = append(summary.LatestVerdicts, ProjectVerdictRow{
						TaskID:       v.TaskID,
						Verdict:      v.Verdict,
						VerdictClass: judgeVerdictCSSClass(v.Verdict),
						Confidence:   v.Confidence,
						Summary:      truncateSummary(v.Summary, 200),
						Model:        v.Model,
						RecordedAt:   v.RecordedAt,
					})
				}
			}
		}
	}

	// Phase 1 signals: walk recent step outcomes for this
	// project, count signals by severity + detector. Bounded
	// at 500 rows — a project that's running so hot it
	// produces >500 step outcomes in 7d will have its panel
	// undercount, but that's a rare extreme and the spend
	// panel above already flags such projects.
	if s.outcomeRepo != nil {
		octx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rows, err := s.outcomeRepo.List(octx, persistence.ExecutionStepOutcomeFilter{
			ProjectID: &projectID,
			Since:     &since,
			PageSize:  500,
		})
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", projectID).
				Msg("hallucination panel: outcome list failed")
		} else {
			// Any non-nil row signals "the project did run work in
			// this window" — load-bearing for the empty-state
			// distinction below.
			if len(rows) > 0 {
				summary.HasRecentOutcomes = true
			}
			for _, r := range rows {
				if r == nil || len(r.HallucinationSignals) == 0 {
					continue
				}
				for _, sig := range parseHallucinationSignalsForUI(r.HallucinationSignals) {
					summary.SignalsByDetector[sig.Detector]++
					switch sig.Severity {
					case "high":
						summary.SignalsHigh++
					case "warn":
						summary.SignalsWarn++
					case "info":
						summary.SignalsInfo++
					}
				}
			}
		}
	}
	return summary
}

// truncateSummary clips a long judge summary so it fits the
// project-page row without scrolling. The full text remains on
// the task detail page's verdict panel.
func truncateSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseTaskLimit reads the ?limit=N query string and returns the
// validated value, falling back to fallback when the input is
// missing, malformed, or not in the allowed set. Defending
// against arbitrary integers matters because the value flows
// directly into TaskFilter.PageSize — a hostile caller could
// otherwise ask for a 10M-row scan and stall the page.
func parseTaskLimit(raw string, allowed []int, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	for _, ok := range allowed {
		if n == ok {
			return n
		}
	}
	return fallback
}

// buildRateLimitWarning aggregates the recent-warn / recent-block
// counts across the project scope and every active API-key scope
// for this project. Returns the zero RateLimitWarning when nothing
// has degraded inside the StatusWindow — the template suppresses
// the banner on Active=false.
//
// Note: rows come from buildRateLimitPanel which already filtered
// out revoked + expired keys. KeyID is intentionally internal to the
// data model: the template renders only name + prefix, while this
// helper uses KeyID to aggregate the exact API-key scope that
// AuthMiddleware passes to metrics.Observe.
//
// metrics nil ⇒ zero warning (Active=false). projectID empty ⇒
// same. The function is pure beyond the metrics read, so its
// behaviour is fully unit-testable.
func buildRateLimitWarning(metrics *ratelimit.Metrics, keys []RateLimitKeyRow, projectID string, now time.Time) RateLimitWarning {
	if metrics == nil || projectID == "" {
		return RateLimitWarning{}
	}
	out := RateLimitWarning{
		WindowLabel: shortDuration(ratelimit.StatusWindow),
	}
	// Project-scope summary — covers the per-project task-creation
	// limiter blocks (Limiter.Check via ObserveProject).
	projSummary := metrics.StatusFor(ratelimit.ScopeProject, projectID)
	out.RecentWarns = projSummary.RecentWarns
	out.RecentBlocks = projSummary.RecentBlocks
	lastBlock := projSummary.LastBlockAt
	if !projSummary.LastBlockAt.IsZero() {
		out.LastBlockAt = humaniseAgo(now.Sub(projSummary.LastBlockAt))
	}
	for _, key := range keys {
		if key.KeyID == "" {
			continue
		}
		keySummary := metrics.StatusFor(ratelimit.ScopeAPIKey, key.KeyID)
		out.RecentWarns += keySummary.RecentWarns
		out.RecentBlocks += keySummary.RecentBlocks
		if keySummary.LastBlockAt.After(lastBlock) {
			lastBlock = keySummary.LastBlockAt
			out.LastBlockAt = humaniseAgo(now.Sub(keySummary.LastBlockAt))
		}
	}
	out.Active = out.RecentWarns > 0 || out.RecentBlocks > 0
	return out
}

// shortDuration renders a Go time.Duration as a compact label
// ("5m", "1h", "45s") for the banner copy. Drops trailing zeros
// the standard library's d.String() preserves.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

// humaniseAgo renders "3m 24s ago" / "12s ago" for the banner's
// "Last 429" line. Sub-second deltas round up to "just now" so
// the operator sees a meaningful label even on a fresh event.
func humaniseAgo(d time.Duration) string {
	if d <= 0 {
		return "just now"
	}
	if d < time.Second {
		return "just now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	}
	if d < time.Hour {
		mins := int(d / time.Minute)
		secs := int((d % time.Minute) / time.Second)
		if secs == 0 {
			return fmt.Sprintf("%dm ago", mins)
		}
		return fmt.Sprintf("%dm %ds ago", mins, secs)
	}
	hours := int(d / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	if mins == 0 {
		return fmt.Sprintf("%dh ago", hours)
	}
	return fmt.Sprintf("%dh %dm ago", hours, mins)
}

// buildRateLimitPanel queries the project's active (non-revoked,
// non-expired) API keys and returns one row per key showing its
// configured throttle. Keys without rate limits configured still
// appear so operators can see at a glance which keys WOULD throttle.
// Best-effort — repo errors degrade to an empty slice.
func buildRateLimitPanel(ctx context.Context, repo persistence.APIKeyRepository, projectID string, logger zerolog.Logger) []RateLimitKeyRow {
	if repo == nil || projectID == "" {
		return nil
	}
	keys, err := repo.ListByProject(ctx, projectID)
	if err != nil {
		logger.Warn().Err(err).Str("project_id", projectID).Msg("rate-limit panel: ListByProject failed")
		return nil
	}
	out := make([]RateLimitKeyRow, 0, len(keys))
	now := time.Now()
	for _, k := range keys {
		if k == nil {
			continue
		}
		if k.RevokedAt != nil {
			continue
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
			continue
		}
		row := RateLimitKeyRow{
			KeyID:     k.ID,
			Name:      k.Name,
			KeyPrefix: k.KeyPrefix,
		}
		if k.RateLimitRPS != nil {
			row.RateLimitRPS = *k.RateLimitRPS
		}
		if k.RateLimitBurst != nil {
			row.RateLimitBurst = *k.RateLimitBurst
		}
		out = append(out, row)
	}
	return out
}

// buildRetentionPanel resolves the effective retention policy for
// a project — applying the per-project YAML overrides on top of
// the daemon's defaults — and packages it for the template. Pure
// helper; no DB / IO. Each per-field IsOverride flag is true
// when the project YAML carried a non-zero value, false when the
// effective value fell through to the daemon default.
//
// Mirrors retention.Resolve's precedence but doesn't import the
// retention package so the UI layer doesn't pull retention's
// pq/sql deps. The duplication is small and well-tested.
func buildRetentionPanel(project *registry.Project, defaults registry.ProjectRetention) RetentionPanel {
	const (
		defaultTaskLLMUsageDays = 90
		defaultToolAuditDays    = 30
		defaultTasksDays        = 60
		defaultExecutionsDays   = 60
		defaultArtifactsDays    = 60
	)
	var pr registry.ProjectRetention
	if project != nil {
		pr = project.Retention
	}
	pick := func(perProject, def, hardDefault int) (int, bool) {
		if perProject > 0 {
			return perProject, true
		}
		if def > 0 {
			return def, false
		}
		return hardDefault, false
	}
	v, o := pick(pr.TaskLLMUsageDays, defaults.TaskLLMUsageDays, defaultTaskLLMUsageDays)
	p := RetentionPanel{TaskLLMUsageDays: v, TaskLLMUsageIsOverride: o}
	v, o = pick(pr.ToolAuditDays, defaults.ToolAuditDays, defaultToolAuditDays)
	p.ToolAuditDays, p.ToolAuditIsOverride = v, o
	v, o = pick(pr.TasksDays, defaults.TasksDays, defaultTasksDays)
	p.TasksDays, p.TasksIsOverride = v, o
	v, o = pick(pr.ExecutionsDays, defaults.ExecutionsDays, defaultExecutionsDays)
	p.ExecutionsDays, p.ExecutionsIsOverride = v, o
	v, o = pick(pr.ArtifactsDays, defaults.ArtifactsDays, defaultArtifactsDays)
	p.ArtifactsDays, p.ArtifactsIsOverride = v, o
	return p
}

// populateProjectHomepageAutonomy fills the per-project homepage
// hero's "next autonomy tick" countdown fields when the project
// has autonomy enabled AND at least one evaluation row recorded.
// Extracted from ProjectDetail so the contract — including the
// PollInterval parse + the humanise-from-CreatedAt math — can be
// unit-tested without spinning up the full handler.
//
// Best-effort: any error short-circuits without mutating `data`,
// so the hero block's {{if .NextAutonomyTickETA}} gate skips the
// badge cleanly.
func (s *Server) populateProjectHomepageAutonomy(ctx context.Context, project *registry.Project, projectID string, data *ProjectDetailData) {
	if s.autonomyEvalRepo == nil || project == nil || !project.Autonomy.Enabled {
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pid := projectID
	evals, err := s.autonomyEvalRepo.List(listCtx, persistence.AutonomyEvaluationFilter{
		ProjectID: &pid,
		PageSize:  1,
	})
	if err != nil {
		s.logger.Debug().Err(err).Str("project_id", projectID).
			Msg("project homepage: autonomy eval lookup failed; hero hides the next-tick badge")
		return
	}
	if len(evals) == 0 {
		return
	}
	last := evals[0]
	interval := 5 * time.Minute // matches the daemon's default
	if d, perr := time.ParseDuration(project.Autonomy.PollInterval); perr == nil && d > 0 {
		interval = d
	}
	data.NextAutonomyTickAt = last.CreatedAt.Add(interval)
	now := time.Now().UTC()
	data.NextAutonomyTickETA = humanizeEta(data.NextAutonomyTickAt.Sub(now))
	data.NextAutonomyTickISO = data.NextAutonomyTickAt.Format(time.RFC3339)
	data.LastAutonomyOutcome = last.Outcome
	data.LastAutonomyAgoLabel = humanizeAgo(now.Sub(last.CreatedAt))
}

// buildGitAccessPanel constructs the GitAccessPanel for the project-detail
// page. Called only when project.Git.Enabled is true; the caller gates on
// that before invoking.
//
// Clone URL derivation (from the brief):
//   - Full URL: <publicBaseURL>/api/v1/git/<projectID>.git — shown when
//     publicBaseURL is non-empty.
//   - Relative path only: /api/v1/git/<projectID>.git — shown with a
//     "set server.public_base_url" hint when publicBaseURL is empty.
//
// Forge detection uses (*registry.Project).ResolveForge() which handles
// both the explicit forge: block and the legacy top-level github: creds.
//
// Auth-off state is read from the request context via
// api.IsAuthEnabledFromContext — same signal uiRequireAdminMutation uses.
func buildGitAccessPanel(project *registry.Project, projectID, publicBaseURL string, r *http.Request) GitAccessPanel {
	relativePath := "/api/v1/git/" + projectID + ".git"
	panel := GitAccessPanel{
		Show:         true,
		Enabled:      project.Git.Enabled,
		RelativePath: relativePath,
	}
	if publicBaseURL != "" {
		// FIX 3: trim any trailing slash so a base like "https://host/" doesn't
		// produce a double slash ("https://...@host//api/v1/git/...").
		publicBaseURL = strings.TrimRight(publicBaseURL, "/")
		// Derive the display clone URL: insert <key-name>@ after the scheme.
		// publicBaseURL is "https://host[:port]" or "http://host[:port]".
		// Result: "https://<key-name>@host[:port]/api/v1/git/<id>.git"
		const httpsPrefix = "https://"
		const httpPrefix = "http://"
		switch {
		case len(publicBaseURL) > len(httpsPrefix) && publicBaseURL[:len(httpsPrefix)] == httpsPrefix:
			panel.CloneURL = httpsPrefix + "<key-name>@" + publicBaseURL[len(httpsPrefix):] + relativePath
		case len(publicBaseURL) > len(httpPrefix) && publicBaseURL[:len(httpPrefix)] == httpPrefix:
			panel.CloneURL = httpPrefix + "<key-name>@" + publicBaseURL[len(httpPrefix):] + relativePath
		default:
			// Fallback: treat as opaque base URL.
			panel.CloneURL = publicBaseURL + relativePath
		}
	} else {
		panel.BaseURLMissing = true
	}
	_, panel.IsForgeBacked = project.ResolveForge()
	panel.AuthDisabled = !api.IsAuthEnabledFromContext(r.Context())
	return panel
}
