// Spend deep-dive: a dedicated dashboard for understanding what's
// driving LLM costs. Where the main dashboard shows headline spend
// + a top-10 model leaderboard, this page lets the operator slice
// the same data along multiple axes (project, source, task,
// role/model) and surface token-mix patterns (input ratio, calls
// per task) that point at root causes — context bloat, dispatcher
// overhead, runaway tool loops.

package ui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
)

// SpendData carries everything the spend.html template needs.
// Loaded fresh on every render — nothing is cached, so an operator
// running the page repeatedly during an investigation always sees
// the latest aggregates.
type SpendData struct {
	Title       string
	CurrentPage string

	// Window controls. WindowDays is the active selection (24h →
	// 1 day; 7d / 30d as named); operators set it via ?window=24h
	// / 7d / 30d in the URL. Default 7d.
	WindowDays  int
	WindowLabel string

	// Optional project filter. Empty = all projects.
	ProjectID    string
	ProjectsList []string // for the filter dropdown
	AllAccess    bool     // true = offer the global "All" option; scoped callers pick one project

	// Headline numbers.
	TotalUSD              float64
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	TotalCalls            int
	// InputRatioPct is prompt tokens as a percentage of total tokens.
	// High input ratio (>90%) is a context-bloat signal — most spend
	// is going to "model reads my history" rather than "model
	// produces output."
	InputRatioPct float64

	// Headline cache observability. CacheHitRatioPct is the
	// dashboard's flagship "are we benefiting from prompt caching"
	// number: cache_read / (cache_read + prompt_tokens) × 100,
	// summed across all (role, model) cohorts. SavedUSD is what
	// those cache reads would have cost at the full input rate —
	// the "$ saved by cache" tile.
	TotalCacheCreationTokens int64
	TotalCacheReadTokens     int64
	TotalCacheSavingsUSD     float64
	CacheHitRatioPct         float64

	// Embedding cache tile — separate from the chat-side
	// CacheHitRatio because the embedding cache is keyed on
	// (content_hash, model) not prompt prefixes. Populated when
	// the daemon has wired an EmbeddingCacheStatsSource AND
	// migration 41 has been applied. Zero / Available=false
	// renders an "embedding cache disabled" placeholder.
	EmbeddingCacheAvailable bool
	EmbeddingCacheRows      int64
	EmbeddingCacheBytes     int64
	EmbeddingCacheModels    int

	// Response cache tile — Phase E full-response memoisation
	// for the memory background trio (Titler / Classifier / KG
	// Extractor). Populated when the daemon has wired a
	// ResponseCacheStatsSource AND migration 47 has been
	// applied. Zero / Available=false renders a "disabled"
	// placeholder.
	ResponseCacheAvailable  bool
	ResponseCacheRows       int64
	ResponseCacheBytes      int64
	ResponseCachePurposes   int
	ResponseCacheHits       int64
	ResponseCacheSavingsUSD float64

	// Per-source breakdown (workflow_step vs dispatcher).
	BySource []SourceSpendRow

	// Per-project leaderboard.
	ByProject []ProjectSpendRow

	// Top tasks by cost. Row count capped by Limit (shared
	// page-size selector — see page_size.go); default 20.
	TopTasks []TaskSpendRow

	// Limit / LimitOptions back the shared "Show: 10 / 20 / 50 /
	// 100" selector. Applies to TopTasks (the most granular
	// list on this page); the other panels (BySource, ByProject,
	// TopRoleModels) have their own per-aggregation caps and
	// surface ≤12 rows on a busy deployment, so the operator-facing
	// row knob is wired to the one that benefits most from it.
	Limit        int
	LimitOptions []int

	// Top role+model combos. Reuses the existing aggregation but
	// renders in this page's layout for one-screen drill-down.
	TopRoleModels []RoleModelSpendRow

	// Phase 1 signal cohorts — one row per (worker role, model) seen
	// in step-outcome hallucination signals over the active window.
	// Distinct from HallucinationRollup, which keys on the judge's
	// (role, model). Operators use Phase 1 cohorts to spot "this
	// worker model produces the most claim-grounding failures" vs
	// the judge-side rollup which says "this judge model is
	// fail-happy".
	Phase1Cohorts []Phase1CohortRow

	// Phase 3 hallucination rollup — one row per (role, model) seen
	// in the verdict feed over the active window. Operators use
	// this to identify which model is hallucinating most often and
	// at what cost. Empty when the project hasn't enabled judging
	// or no terminal tasks have produced verdicts yet.
	HallucinationRollup []HallucinationRollupRow

	// Daily time-series — raw + svg sparkline.
	DailyMax     float64
	DailyMaxDay  time.Time // date of the peak bucket; rendered alongside DailyMax so the operator can spot-check which day is the peak without relying on hover-only tooltips
	DailyBuckets []DailyBucket
	Sparkline    string
	SparkFilled  string
}

// Phase1CohortRow is one (worker role, model) cohort in the
// Phase 1 signal aggregation — the upstream layer of
// hallucination detection that runs the rule-based grounding
// on every agent step. Step counts include outcomes that fired
// at least one signal; SignalsHigh / SignalsWarn split by
// severity so operators see "is this cohort failing on a few
// steps badly, or many steps mildly?".
type Phase1CohortRow struct {
	Role          string
	Model         string
	StepsAffected int
	SignalsHigh   int
	SignalsWarn   int
	SignalsInfo   int
	// SignalsByDetector counts findings per rule (url_not_fetched,
	// hallucinated_tool_format, etc.) so the dominant failure
	// mode for the cohort is visible without drilling into
	// individual outcomes.
	SignalsByDetector map[string]int
	LastRecordedAt    time.Time
}

// HallucinationRollupRow is one (role, model) cohort in the
// Phase 3 verdict aggregation. Pass rate is the headline
// quality signal; cost lets the operator compare how much
// they're paying per "good" answer across cohorts.
type HallucinationRollupRow struct {
	Role           string
	Model          string
	Total          int
	Pass           int
	Fail           int
	Abstain        int
	PassRatePct    float64
	MeanConfidence float64
	TotalCostUSD   float64
	CostPerTaskUSD float64
	LastRecordedAt time.Time
}

// SourceSpendRow is the display shape: thin wrapper over the repo
// type plus a derived percentage column.
type SourceSpendRow struct {
	Source           string
	Display          string // "workflow_step" → "Workflow steps" etc.
	CostUSD          float64
	CallCount        int
	PromptTokens     int64
	CompletionTokens int64
	PctOfTotal       float64
	// Cache observability per source. CacheHitRatioPct is
	// cache_read / (cache_read + prompt_tokens) × 100; zero when
	// the source never produced cache reads (the common case for
	// non-Anthropic providers).
	CacheCreationTokens int64
	CacheReadTokens     int64
	CacheHitRatioPct    float64
}

// ProjectSpendRow is the per-project drill-down row.
type ProjectSpendRow struct {
	ProjectID        string
	CostUSD          float64
	StepCount        int
	TaskCount        int
	PromptTokens     int64
	CompletionTokens int64
	CostPerTaskUSD   float64
	InputRatioPct    float64
	// Cache observability per project.
	CacheCreationTokens int64
	CacheReadTokens     int64
	CacheHitRatioPct    float64
}

// TaskSpendRow is one row in the top-tasks table. ID display +
// shortening is delegated to the `shortID` template helper for
// consistency with /ui/tasks and the dashboard's active-tasks tile.
type TaskSpendRow struct {
	TaskID            string
	ProjectID         string
	Status            string
	CostUSD           float64
	StepCount         int
	Iterations        int
	IterationsPerStep float64
	InputRatioPct     float64
	DurationDisplay   string
}

// RoleModelSpendRow displays the leaderboard with input-ratio +
// drift columns added. High input ratio means the role's prompt
// is too big; high drift ratio means cost/success has regressed
// against the baseline window.
type RoleModelSpendRow struct {
	Role             string
	Model            string
	CostUSD          float64
	StepCount        int
	PromptTokens     int64
	CompletionTokens int64
	CostPerStepUSD   float64
	InputRatioPct    float64
	// DriftRatio is current_24h_$_per_ok / baseline_7d_$_per_ok
	// for this (role, model) pair scoped to the active project
	// filter (or globally when no filter). 0 means "no drift data
	// yet" — either insufficient baseline successes or current
	// spend below the floor. The template renders the column as
	// a colored badge: green <1, gray ≈1, amber 1–2, rose >2.
	DriftRatio float64
	// DriftHasBaseline distinguishes "insufficient baseline data"
	// (false) from "ratio is exactly zero" (false but with current
	// data) so the template can render "—" rather than a misleading
	// "0×" badge. ComputeDrift always sets this to true when the
	// pair has met both the spend floor and baseline-oks threshold.
	DriftHasBaseline bool
	// Cache observability per (role, model). Sourced from the
	// aggregation query; ratio = cache_read / (cache_read +
	// prompt_tokens) × 100.
	CacheCreationTokens int64
	CacheReadTokens     int64
	CacheHitRatioPct    float64
}

// DailyBucket is one column of the time-series chart.
type DailyBucket struct {
	Day       time.Time
	CostUSD   float64
	CallCount int
	HeightPct float64 // 0..100, used by the bar template directly
}

// Spend renders /ui/spend.
func (s *Server) Spend(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug().Str("path", r.URL.Path).Msg("rendering spend dashboard")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	data := SpendData{
		Title:        "Spend",
		CurrentPage:  "spend",
		WindowDays:   7,
		WindowLabel:  "7 days",
		ProjectID:    r.URL.Query().Get("project"),
		Limit:        parsePageSize(r.URL.Query().Get("limit")),
		LimitOptions: PageSizeOptions,
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

	// Project scope. The "All" option is global for an admin and the
	// union of the caller's projects for a scoped session — consistent
	// with trends/tool-budget. queryIDs: nil = global single query; else
	// the set to query-and-merge. iter expands nil → [""] (one global
	// call) so every series below shares one loop-and-merge path.
	queryIDs, options, ok := s.resolveProjectScope(w, r, data.ProjectID)
	if !ok {
		return // 403 already written
	}
	data.ProjectsList = options
	data.AllAccess = requestHasAllProjectAccess(r)
	if !data.AllAccess && len(options) == 0 {
		s.render(w, "spend.html", data) // awaiting-access: nothing to show
		return
	}
	iter := projectsToIterate(queryIDs)

	if s.llmUsageRepo == nil {
		// Repo not wired — render a degraded page rather than
		// 500'ing. The headline shows zeros + an explanatory
		// note in the template.
		s.render(w, "spend.html", data)
		return
	}

	now := time.Now().UTC()
	since := now.Add(-time.Duration(data.WindowDays) * 24 * time.Hour)

	// Source split is the first thing we want — it answers the
	// operator's headline question "is the dispatcher itself
	// running up the bill?"
	{
		rows := s.sourceSpendForScope(ctx, iter, since)
		var total float64
		for _, r := range rows {
			total += r.CostUSD
		}
		for _, r := range rows {
			row := SourceSpendRow{
				Source:              r.Source,
				Display:             displaySource(r.Source),
				CostUSD:             r.CostUSD,
				CallCount:           r.CallCount,
				PromptTokens:        r.PromptTokens,
				CompletionTokens:    r.CompletionTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				CacheHitRatioPct:    decorateCacheRatioPct(r.PromptTokens, r.CacheReadTokens),
			}
			if total > 0 {
				row.PctOfTotal = r.CostUSD / total * 100
			}
			data.BySource = append(data.BySource, row)
			data.TotalUSD += r.CostUSD
			data.TotalPromptTokens += r.PromptTokens
			data.TotalCompletionTokens += r.CompletionTokens
			data.TotalCalls += r.CallCount
		}
		if total := data.TotalPromptTokens + data.TotalCompletionTokens; total > 0 {
			data.InputRatioPct = float64(data.TotalPromptTokens) / float64(total) * 100
		}
	}

	if rows, err := s.llmUsageRepo.AggregateByProject(ctx, since, time.Time{}, 25); err == nil {
		for _, spend := range rows {
			if !api.RequestAllowsProject(r, spend.ProjectID) {
				continue
			}
			if data.ProjectID != "" && spend.ProjectID != data.ProjectID {
				continue
			}
			row := ProjectSpendRow{
				ProjectID:           spend.ProjectID,
				CostUSD:             spend.CostUSD,
				StepCount:           spend.StepCount,
				TaskCount:           spend.TaskCount,
				PromptTokens:        spend.PromptTokens,
				CompletionTokens:    spend.CompletionTokens,
				CacheCreationTokens: spend.CacheCreationTokens,
				CacheReadTokens:     spend.CacheReadTokens,
				CacheHitRatioPct:    decorateCacheRatioPct(spend.PromptTokens, spend.CacheReadTokens),
			}
			if spend.TaskCount > 0 {
				row.CostPerTaskUSD = spend.CostUSD / float64(spend.TaskCount)
			}
			if total := spend.PromptTokens + spend.CompletionTokens; total > 0 {
				row.InputRatioPct = float64(spend.PromptTokens) / float64(total) * 100
			}
			data.ByProject = append(data.ByProject, row)
		}
	} else {
		s.logger.Warn().Err(err).Msg("spend: failed to aggregate by project")
	}

	{
		rows := s.topTasksForScope(ctx, iter, since, data.Limit)
		for _, r := range rows {
			row := TaskSpendRow{
				TaskID:     r.TaskID,
				ProjectID:  r.ProjectID,
				Status:     r.Status,
				CostUSD:    r.CostUSD,
				StepCount:  r.StepCount,
				Iterations: r.Iterations,
			}
			if r.StepCount > 0 {
				row.IterationsPerStep = float64(r.Iterations) / float64(r.StepCount)
			}
			if total := r.PromptTokens + r.CompletionTokens; total > 0 {
				row.InputRatioPct = float64(r.PromptTokens) / float64(total) * 100
			}
			if !r.FirstStepAt.IsZero() && !r.LastStepAt.IsZero() {
				row.DurationDisplay = humanizeDuration(r.LastStepAt.Sub(r.FirstStepAt))
			}
			data.TopTasks = append(data.TopTasks, row)
		}
	}

	{
		rows := s.roleModelSpendForScope(ctx, iter, since)
		// Compute drift first so the leaderboard rows can join
		// against it. Drift uses its own current/baseline windows
		// (24h vs 7d) regardless of the page's selected window —
		// "is this combo behaving worse than usual" is a fixed
		// definition; varying the windows would change the
		// signal's meaning between page refreshes.
		// Drift is a per-project ratio that can't be meaningfully summed
		// across projects, so it's only computed for a single scope
		// (len(iter)==1: one global "" call for admins, or one specific
		// project). The "All my projects" union omits the drift column.
		driftByKey := map[string]budget.DriftRow{}
		if s.outcomeRepo != nil && len(iter) == 1 {
			if drift, derr := budget.ComputeDrift(ctx, s.llmUsageRepo, s.outcomeRepo, iter[0], budget.DefaultDriftConfig(), now); derr == nil {
				for _, d := range drift {
					driftByKey[d.Role+"|"+d.Model] = d
				}
			} else {
				s.logger.Warn().Err(derr).Msg("spend: drift compute failed (leaderboard renders without drift column)")
			}
		}

		for _, r := range rows {
			row := RoleModelSpendRow{
				Role:                displayRole(r.Role),
				Model:               r.Model,
				CostUSD:             r.CostUSD,
				StepCount:           r.StepCount,
				PromptTokens:        r.PromptTokens,
				CompletionTokens:    r.CompletionTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
			}
			if r.StepCount > 0 {
				row.CostPerStepUSD = r.CostUSD / float64(r.StepCount)
			}
			if total := r.PromptTokens + r.CompletionTokens; total > 0 {
				row.InputRatioPct = float64(r.PromptTokens) / float64(total) * 100
			}
			if d, ok := driftByKey[r.Role+"|"+r.Model]; ok {
				row.DriftHasBaseline = d.HasBaseline
				if d.HasBaseline {
					row.DriftRatio = d.Ratio
				}
			}
			// Pricing-driven cache savings on the row use the raw
			// (un-displayed) model name so the pricing lookup hits;
			// the displayed role label has already been mapped above.
			if s.assistantPricing != nil {
				data.TotalCacheSavingsUSD += s.assistantPricing.CacheSavingsUSD(r.Model, int(r.CacheReadTokens))
			}
			data.TopRoleModels = append(data.TopRoleModels, row)
		}
		// Decorate each row's CacheHitRatioPct and pull the headline
		// totals + global ratio out of the leaderboard rows. Same
		// math as the per-row column so the tile + table are
		// consistent.
		data.TopRoleModels, data.TotalCacheCreationTokens, data.TotalCacheReadTokens, data.CacheHitRatioPct =
			computeCacheHeadline(data.TopRoleModels)
	}

	// Phase 1 cohorts — walk recent step outcomes for the active window,
	// concatenated across the scope, then aggregate by (role, model).
	if s.outcomeRepo != nil {
		var rows []*persistence.ExecutionStepOutcome
		for _, pid := range iter {
			filter := persistence.ExecutionStepOutcomeFilter{Since: &since, PageSize: 5000}
			if pid != "" {
				p := pid
				filter.ProjectID = &p
			}
			r, err := s.outcomeRepo.List(ctx, filter)
			if err != nil {
				s.logger.Warn().Err(err).Str("project_id", pid).Msg("spend: failed to load step outcomes for Phase 1 cohorts")
				continue
			}
			rows = append(rows, r...)
		}
		data.Phase1Cohorts = aggregatePhase1Cohorts(rows)
	}

	// Phase 3 hallucination rollup — per-(role, model) cohort
	// scores from the judge verdict feed. Best-effort: a missing
	// repo / query error degrades to an empty rollup rather than
	// blocking the rest of the spend page.
	if s.judgeVerdictRepo != nil {
		// 5000-row cap is generous: judge fires once per
		// terminal task, so even a project doing 1000
		// tasks/week needs only ~30 days of headroom. Walking
		// the result set in Go to filter by window keeps the
		// repo surface narrow (no need for a second SQL query
		// shape) at the cost of one extra string compare per
		// row, which is negligible.
		var verdicts []*persistence.TaskJudgeVerdict
		for _, pid := range iter {
			v, err := s.judgeVerdictRepo.ListRecent(ctx, pid, 5000)
			if err != nil {
				s.logger.Warn().Err(err).Str("project_id", pid).Msg("spend: failed to load judge verdicts for hallucination rollup")
				continue
			}
			verdicts = append(verdicts, v...)
		}
		data.HallucinationRollup = aggregateHallucinationVerdicts(verdicts, since)
	}

	// Daily time-series. For the 24h window the day-bucket query
	// produces 1-2 rows which renders awkwardly; the dashboard's
	// hourly sparkline already covers that case, so we just skip
	// the per-day chart for the 1d window.
	if data.WindowDays >= 7 {
		rows := s.dailySpendForScope(ctx, iter, since)
		data.DailyBuckets, data.DailyMax, data.DailyMaxDay = buildDailyBuckets(rows, since, now, data.WindowDays)
		values := make([]float64, len(data.DailyBuckets))
		for i, b := range data.DailyBuckets {
			values[i] = b.CostUSD
		}
		data.Sparkline, data.SparkFilled = sparklinePoints(values, 600, 80)
	}

	// Embedding cache stats — orthogonal to the chat-prompt cache
	// surfaced above. Cheap query (COUNT(*) on a small table +
	// pg_total_relation_size) but still best-effort: a missing
	// table (older deployment without migration 41) returns
	// zero values without an error so the panel just shows
	// "disabled" rather than 500ing the whole page.
	if s.embeddingCacheStats != nil {
		statsCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		stats, err := s.embeddingCacheStats.CacheStats(statsCtx)
		cancel()
		if err == nil && stats.RowCount > 0 {
			data.EmbeddingCacheAvailable = true
			data.EmbeddingCacheRows = stats.RowCount
			data.EmbeddingCacheBytes = stats.ApproxBytes
			data.EmbeddingCacheModels = stats.DistinctModels
		}
	}

	// Phase E response cache stats — same shape + same "disabled
	// placeholder when migration 47 hasn't run" guarantee.
	if s.responseCacheStats != nil {
		statsCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		stats, err := s.responseCacheStats.CacheStats(statsCtx)
		cancel()
		if err == nil && stats.RowCount > 0 {
			data.ResponseCacheAvailable = true
			data.ResponseCacheRows = stats.RowCount
			data.ResponseCacheBytes = stats.ApproxBytes
			data.ResponseCachePurposes = stats.DistinctPurposes
			data.ResponseCacheHits = stats.TotalHits
			data.ResponseCacheSavingsUSD = stats.TotalSavingsUSD
		}
	}

	s.render(w, "spend.html", data)
}

// computeCacheHeadline decorates each leaderboard row in place with its
// CacheHitRatioPct and returns aggregate cache totals + the global hit
// ratio (cache_read / (cache_read + prompt_tokens) × 100). Returns zero
// instead of NaN when no cache or prompt traffic exists in the window —
// the template would otherwise render "NaN%".
//
// The role-model leaderboard is the canonical aggregation: the same
// rows feed the headline tile (sum across cohorts) and the per-row Cache
// column. Pulling the math into one function keeps the page consistent.
func computeCacheHeadline(rows []RoleModelSpendRow) (decorated []RoleModelSpendRow, totalCreation, totalRead int64, hitRatioPct float64) {
	var totalPrompt int64
	for i := range rows {
		totalCreation += rows[i].CacheCreationTokens
		totalRead += rows[i].CacheReadTokens
		totalPrompt += rows[i].PromptTokens

		if denom := rows[i].PromptTokens + rows[i].CacheReadTokens; denom > 0 {
			rows[i].CacheHitRatioPct = float64(rows[i].CacheReadTokens) / float64(denom) * 100
		}
	}
	if denom := totalPrompt + totalRead; denom > 0 {
		hitRatioPct = float64(totalRead) / float64(denom) * 100
	}
	return rows, totalCreation, totalRead, hitRatioPct
}

// decorateCacheRatioPct fills the CacheHitRatioPct on a per-source /
// per-project row given prompt + cache_read counts. Same divide-by-zero
// guard as computeCacheHeadline.
func decorateCacheRatioPct(promptTokens, cacheReadTokens int64) float64 {
	denom := promptTokens + cacheReadTokens
	if denom <= 0 {
		return 0
	}
	return float64(cacheReadTokens) / float64(denom) * 100
}

// buildDailyBuckets fills missing days with zero-cost rows so the
// chart renders a continuous timeline rather than collapsing
// quiet days. Returns the rows + the max cost for HeightPct
// scaling + the date of that peak bucket so the operator can
// scaling. Operators reading the chart get the actual gap visible
// instead of three bars crammed together with no time context.
func buildDailyBuckets(rows []persistence.DailySpend, since, now time.Time, days int) ([]DailyBucket, float64, time.Time) {
	if days <= 0 {
		return nil, 0, time.Time{}
	}
	byDay := make(map[string]persistence.DailySpend, len(rows))
	for _, r := range rows {
		key := r.Day.UTC().Format("2006-01-02")
		byDay[key] = r
	}
	out := make([]DailyBucket, 0, days)
	var max float64
	var maxDay time.Time
	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
	if !since.IsZero() {
		since = since.UTC()
		earliest := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
		if earliest.After(dayStart) {
			dayStart = earliest
		}
	}
	for d := 0; d < days; d++ {
		day := dayStart.Add(time.Duration(d) * 24 * time.Hour)
		key := day.Format("2006-01-02")
		bucket := DailyBucket{Day: day}
		if r, ok := byDay[key]; ok {
			bucket.CostUSD = r.CostUSD
			bucket.CallCount = r.CallCount
		}
		if bucket.CostUSD > max {
			max = bucket.CostUSD
			maxDay = day
		}
		out = append(out, bucket)
	}
	for i := range out {
		if max > 0 {
			out[i].HeightPct = out[i].CostUSD / max * 100
		}
	}
	return out, max, maxDay
}

// displaySource maps the raw source column to a friendly label.
// The raw values come from persistence.TaskLLMUsageSource* —
// kept short (workflow_step, dispatcher, judge, post_mortem)
// because they're column data; this helper renders the
// operator-facing version on the spend dashboard.
func displaySource(s string) string {
	switch s {
	case "workflow_step":
		return "Workflow steps"
	case "dispatcher":
		return "Chat dispatcher"
	case "judge":
		return "Hallucination judge"
	case "post_mortem":
		return "Post-mortem explainer"
	case "kg_extraction":
		return "Knowledge graph extraction"
	case "memory_titler":
		return "Memory topic labels"
	case "memory_classifier":
		return "Memory content classification"
	case "_authoring":
		// Web-authoring assistant — "AI Assist" buttons on
		// the swarm / workflow / brief editors. Underscore
		// prefix in the raw column distinguishes it from
		// task-bound sources.
		return "Config assistant"
	case "external_api":
		// Calls hitting the OpenAI- and Ollama-compatible
		// proxy surfaces from third-party tools (Continue,
		// claude-code, custom scripts).
		return "3rd-party requests"
	case "":
		return "Unattributed"
	default:
		return s
	}
}

// displayRole maps the raw role column to an operator-facing
// label. Raw values stay short identifiers (workflow role names,
// "judge", "kg_extractor", etc.) because they're filter/group
// keys; this helper renders the polished version on dashboards.
// Roles not in the switch fall through unchanged — that covers
// every workflow role (researcher, coder, lead, planner, etc.)
// where the raw name is already operator-friendly.
func displayRole(s string) string {
	switch s {
	case "kg_extractor":
		return "Memory · Entity Extractor"
	case "kg_resolver":
		return "Memory · Entity Resolver"
	case "kg_relationship":
		return "Memory · Relationship Extractor"
	case "kg_validator":
		return "Memory · Faithfulness Validator"
	case "memory_titler":
		return "Memory · Topic Titler"
	case "memory_classifier":
		return "Memory · Content Classifier"
	case "judge":
		return "Hallucination Judge"
	default:
		return s
	}
}

// humanizeDuration formats a duration as "Xs", "Xm Ys", or "Xh Ym"
// — the wall-clock-time column on the top-tasks table reads as
// rough magnitude, not stopwatch precision.
func humanizeDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// aggregateHallucinationVerdicts groups verdicts by (role,
// model) over the active window and computes pass-rate /
// mean confidence / total cost per cohort. Returns rows
// sorted by Total descending — operators care most about the
// cohort that fired most often, then secondarily about the
// cohort with the worst pass rate.
//
// since filters in Go (the verdict repo doesn't take a since
// param). 5000-row cap upstream means worst-case ~5K
// comparisons; negligible. Empty input returns nil so the
// template renders the empty-state hint instead of a blank
// table.
func aggregateHallucinationVerdicts(verdicts []*persistence.TaskJudgeVerdict, since time.Time) []HallucinationRollupRow {
	if len(verdicts) == 0 {
		return nil
	}
	type key struct {
		role, model string
	}
	type agg struct {
		total          int
		pass, fail, ab int
		conf           float64
		cost           float64
		last           time.Time
	}
	buckets := map[key]*agg{}
	for _, v := range verdicts {
		if v == nil || v.RecordedAt.Before(since) {
			continue
		}
		k := key{role: v.Role, model: v.Model}
		b := buckets[k]
		if b == nil {
			b = &agg{}
			buckets[k] = b
		}
		b.total++
		switch v.Verdict {
		case persistence.JudgeVerdictPass:
			b.pass++
		case persistence.JudgeVerdictFail:
			b.fail++
		case persistence.JudgeVerdictAbstain:
			b.ab++
		}
		b.conf += v.Confidence
		b.cost += v.CostUSD
		if v.RecordedAt.After(b.last) {
			b.last = v.RecordedAt
		}
	}
	out := make([]HallucinationRollupRow, 0, len(buckets))
	for k, b := range buckets {
		row := HallucinationRollupRow{
			Role:           displayRole(k.role),
			Model:          k.model,
			Total:          b.total,
			Pass:           b.pass,
			Fail:           b.fail,
			Abstain:        b.ab,
			TotalCostUSD:   b.cost,
			LastRecordedAt: b.last,
		}
		if b.total > 0 {
			row.PassRatePct = float64(b.pass) / float64(b.total) * 100
			row.MeanConfidence = b.conf / float64(b.total)
			row.CostPerTaskUSD = b.cost / float64(b.total)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		// Tiebreak on pass rate ascending (worst surfaces
		// first when volumes are equal) and then on the role
		// name so the order is stable.
		if out[i].PassRatePct != out[j].PassRatePct {
			return out[i].PassRatePct < out[j].PassRatePct
		}
		return out[i].Role < out[j].Role
	})
	// Cap at 12 rows — beyond that the panel becomes a wall
	// of badges. Top-12 covers the common deployment shape
	// (3-5 swarm roles × 2-3 models max).
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// aggregatePhase1Cohorts groups step outcomes by (role, model)
// and counts hallucination signals by severity + detector.
// Skips rows with no signals so cohorts that ran cleanly
// don't pad the table; cohorts that NEVER fire a signal are
// invisible by design (operators care about failure modes,
// not "everything is fine"). Sort: SignalsHigh desc → total
// signals desc → role asc, so the loudest failure mode lands
// at the top.
//
// Caps at 12 cohorts for the same reason the Phase 3 rollup
// does — beyond that the panel becomes a wall of badges.
func aggregatePhase1Cohorts(rows []*persistence.ExecutionStepOutcome) []Phase1CohortRow {
	if len(rows) == 0 {
		return nil
	}
	type key struct {
		role, model string
	}
	type agg struct {
		stepsAffected    int
		high, warn, info int
		byDetector       map[string]int
		last             time.Time
	}
	buckets := map[key]*agg{}
	for _, r := range rows {
		if r == nil || len(r.HallucinationSignals) == 0 {
			continue
		}
		signals := parseHallucinationSignalsForUI(r.HallucinationSignals)
		if len(signals) == 0 {
			continue
		}
		k := key{role: r.Role, model: r.Model}
		b := buckets[k]
		if b == nil {
			b = &agg{byDetector: map[string]int{}}
			buckets[k] = b
		}
		b.stepsAffected++
		if r.RecordedAt.After(b.last) {
			b.last = r.RecordedAt
		}
		for _, sig := range signals {
			b.byDetector[sig.Detector]++
			switch sig.Severity {
			case "high":
				b.high++
			case "warn":
				b.warn++
			case "info":
				b.info++
			}
		}
	}
	out := make([]Phase1CohortRow, 0, len(buckets))
	for k, b := range buckets {
		out = append(out, Phase1CohortRow{
			Role:              displayRole(k.role),
			Model:             k.model,
			StepsAffected:     b.stepsAffected,
			SignalsHigh:       b.high,
			SignalsWarn:       b.warn,
			SignalsInfo:       b.info,
			SignalsByDetector: b.byDetector,
			LastRecordedAt:    b.last,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SignalsHigh != out[j].SignalsHigh {
			return out[i].SignalsHigh > out[j].SignalsHigh
		}
		ti := out[i].SignalsHigh + out[i].SignalsWarn + out[i].SignalsInfo
		tj := out[j].SignalsHigh + out[j].SignalsWarn + out[j].SignalsInfo
		if ti != tj {
			return ti > tj
		}
		return out[i].Role < out[j].Role
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// --- scoped spend aggregation (cross-project visibility audit 2026-06-22) ---
// Each helper fetches a per-project series for every id in `iter`
// (iter[0]=="" means a single global query) and merges. Keyed series sum
// by their natural key; top-tasks concatenate then re-rank. This lets the
// "All my projects" view union a scoped caller's projects without ever
// issuing a global (cross-tenant) query.

func (s *Server) sourceSpendForScope(ctx context.Context, iter []string, since time.Time) []persistence.SourceSpend {
	m := map[string]*persistence.SourceSpend{}
	var order []string
	for _, pid := range iter {
		rows, err := s.llmUsageRepo.AggregateBySource(ctx, since, time.Time{}, pid)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", pid).Msg("spend: aggregate by source failed")
			continue
		}
		for _, r := range rows {
			if e := m[r.Source]; e != nil {
				e.CostUSD += r.CostUSD
				e.CallCount += r.CallCount
				e.PromptTokens += r.PromptTokens
				e.CompletionTokens += r.CompletionTokens
				e.CacheCreationTokens += r.CacheCreationTokens
				e.CacheReadTokens += r.CacheReadTokens
			} else {
				cp := r
				m[r.Source] = &cp
				order = append(order, r.Source)
			}
		}
	}
	out := make([]persistence.SourceSpend, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	return out
}

func (s *Server) roleModelSpendForScope(ctx context.Context, iter []string, since time.Time) []persistence.RoleModelSpend {
	m := map[string]*persistence.RoleModelSpend{}
	var order []string
	for _, pid := range iter {
		rows, err := s.llmUsageRepo.AggregateByRoleModel(ctx, since, time.Time{}, 20, pid)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", pid).Msg("spend: role/model leaderboard failed")
			continue
		}
		for _, r := range rows {
			k := r.Role + "|" + r.Model
			if e := m[k]; e != nil {
				e.CostUSD += r.CostUSD
				e.StepCount += r.StepCount
				e.PromptTokens += r.PromptTokens
				e.CompletionTokens += r.CompletionTokens
				e.CacheCreationTokens += r.CacheCreationTokens
				e.CacheReadTokens += r.CacheReadTokens
			} else {
				cp := r
				m[k] = &cp
				order = append(order, k)
			}
		}
	}
	out := make([]persistence.RoleModelSpend, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func (s *Server) topTasksForScope(ctx context.Context, iter []string, since time.Time, limit int) []persistence.TaskSpend {
	var all []persistence.TaskSpend
	for _, pid := range iter {
		rows, err := s.llmUsageRepo.TopTasks(ctx, since, time.Time{}, limit, pid)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", pid).Msg("spend: top tasks failed")
			continue
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CostUSD > all[j].CostUSD })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (s *Server) dailySpendForScope(ctx context.Context, iter []string, since time.Time) []persistence.DailySpend {
	m := map[string]*persistence.DailySpend{}
	var order []string
	for _, pid := range iter {
		rows, err := s.llmUsageRepo.TimeSeriesByDay(ctx, since, time.Time{}, pid)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", pid).Msg("spend: daily time-series failed")
			continue
		}
		for _, r := range rows {
			k := r.Day.Format("2006-01-02")
			if e := m[k]; e != nil {
				e.CostUSD += r.CostUSD
				e.CallCount += r.CallCount
			} else {
				cp := r
				m[k] = &cp
				order = append(order, k)
			}
		}
	}
	out := make([]persistence.DailySpend, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	return out
}
