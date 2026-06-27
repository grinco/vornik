package ui

// Task Trends — UI Design Refresh Track D, sub-project 2.
// See https://docs.vornik.io
//
// Read-only daily throughput + success-rate time-series from the recent task
// sample, bucketed by created-date in the handler (no TaskRepository method →
// no fake ripple). summarizeTaskTrends is pure; the handler is a thin
// List → summarize → render wrapper.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

const (
	trendSampleCap = 4000 // most-recent tasks examined (List is created_at DESC)
	trendDays      = 14   // trailing window, daily buckets
)

// Trend chart geometry (px).
const (
	trendChartH   = 120
	trendChartTop = 10
	trendColW     = 34
	trendBarW     = 22
	trendLeftPad  = 4
)

// dayBucket is one day's task counts + bar geometry.
type dayBucket struct {
	Date       string
	Created    int
	Completed  int
	Failed     int
	SuccessPct int
	X          int
	BarY       int
	BarH       int
}

// trendStats is the summarized daily time-series.
type trendStats struct {
	Sample            int
	Days              int
	Buckets           []dayBucket
	TotCreated        int
	TotCompleted      int
	TotFailed         int
	OverallSuccessPct int
	SVGWidth          int
	SVGHeight         int
}

func truncateDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// summarizeTaskTrends buckets tasks by created-date over the trailing `days`
// window ending on `now`. Pure — `now` is injected for deterministic tests.
func summarizeTaskTrends(tasks []*persistence.Task, now time.Time, days int) trendStats {
	if days <= 0 {
		days = trendDays
	}
	s := trendStats{Sample: len(tasks), Days: days}
	today := truncateDate(now)
	start := today.AddDate(0, 0, -(days - 1))

	s.Buckets = make([]dayBucket, days)
	idx := make(map[string]int, days)
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		s.Buckets[i].Date = key
		idx[key] = i
	}

	for _, t := range tasks {
		if t == nil || t.CreatedAt.IsZero() {
			continue
		}
		key := truncateDate(t.CreatedAt).Format("2006-01-02")
		i, ok := idx[key]
		if !ok {
			continue // outside the window
		}
		s.Buckets[i].Created++
		s.TotCreated++
		switch t.Status {
		case persistence.TaskStatusCompleted:
			s.Buckets[i].Completed++
			s.TotCompleted++
		case persistence.TaskStatusFailed:
			s.Buckets[i].Failed++
			s.TotFailed++
		}
	}

	// Per-day success rate + bar geometry (scaled to the busiest day).
	maxCreated := 0
	for i := range s.Buckets {
		if term := s.Buckets[i].Completed + s.Buckets[i].Failed; term > 0 {
			s.Buckets[i].SuccessPct = s.Buckets[i].Completed * 100 / term
		}
		if s.Buckets[i].Created > maxCreated {
			maxCreated = s.Buckets[i].Created
		}
	}
	for i := range s.Buckets {
		s.Buckets[i].X = trendLeftPad + i*trendColW
		if maxCreated > 0 {
			s.Buckets[i].BarH = s.Buckets[i].Created * trendChartH / maxCreated
		}
		s.Buckets[i].BarY = trendChartTop + (trendChartH - s.Buckets[i].BarH)
	}
	if term := s.TotCompleted + s.TotFailed; term > 0 {
		s.OverallSuccessPct = s.TotCompleted * 100 / term
	}
	s.SVGWidth = trendLeftPad + days*trendColW
	s.SVGHeight = trendChartTop + trendChartH + 22 // room for x-axis labels
	return s
}

// verdictBucket is one day's judge-verdict counts + bar geometry.
type verdictBucket struct {
	Date       string
	Pass       int
	Fail       int
	Abstain    int
	Total      int
	AbstainPct int
	X          int
	BarY       int
	BarH       int
}

// verdictTrendStats is the summarized daily judge-verdict time-series.
type verdictTrendStats struct {
	HasData           bool
	Sample            int
	Days              int
	Buckets           []verdictBucket
	TotPass           int
	TotFail           int
	TotAbstain        int
	OverallAbstainPct int
	SVGWidth          int
	SVGHeight         int
}

// summarizeVerdictTrends buckets LLM-as-judge verdicts (pass/fail/abstain) by
// RecordedAt date over the trailing window. Pure; `now` injected for tests.
func summarizeVerdictTrends(verdicts []*persistence.TaskJudgeVerdict, now time.Time, days int) verdictTrendStats {
	if days <= 0 {
		days = trendDays
	}
	s := verdictTrendStats{Sample: len(verdicts), Days: days}
	today := truncateDate(now)
	start := today.AddDate(0, 0, -(days - 1))

	s.Buckets = make([]verdictBucket, days)
	idx := make(map[string]int, days)
	for i := 0; i < days; i++ {
		key := start.AddDate(0, 0, i).Format("2006-01-02")
		s.Buckets[i].Date = key
		idx[key] = i
	}

	for _, v := range verdicts {
		if v == nil || v.RecordedAt.IsZero() {
			continue
		}
		i, ok := idx[truncateDate(v.RecordedAt).Format("2006-01-02")]
		if !ok {
			continue
		}
		s.Buckets[i].Total++
		switch strings.ToLower(v.Verdict) {
		case "pass":
			s.Buckets[i].Pass++
			s.TotPass++
		case "fail":
			s.Buckets[i].Fail++
			s.TotFail++
		case "abstain":
			s.Buckets[i].Abstain++
			s.TotAbstain++
		}
	}

	maxTotal := 0
	for i := range s.Buckets {
		if s.Buckets[i].Total > 0 {
			s.Buckets[i].AbstainPct = s.Buckets[i].Abstain * 100 / s.Buckets[i].Total
		}
		if s.Buckets[i].Total > maxTotal {
			maxTotal = s.Buckets[i].Total
		}
	}
	for i := range s.Buckets {
		s.Buckets[i].X = trendLeftPad + i*trendColW
		if maxTotal > 0 {
			s.Buckets[i].BarH = s.Buckets[i].Total * trendChartH / maxTotal
		}
		s.Buckets[i].BarY = trendChartTop + (trendChartH - s.Buckets[i].BarH)
	}
	if tot := s.TotPass + s.TotFail + s.TotAbstain; tot > 0 {
		s.OverallAbstainPct = s.TotAbstain * 100 / tot
		s.HasData = true
	}
	s.SVGWidth = trendLeftPad + days*trendColW
	s.SVGHeight = trendChartTop + trendChartH + 22
	return s
}

// recoveryBucket is one day's recovery-event count + bar geometry.
type recoveryBucket struct {
	Date  string
	Count int
	X     int
	BarY  int
	BarH  int
}

// recoveryTrendStats is the summarized daily recovery-event time-series.
type recoveryTrendStats struct {
	HasData   bool
	Sample    int
	Days      int
	Buckets   []recoveryBucket
	Total     int
	SVGWidth  int
	SVGHeight int
}

// summarizeRecoveryTrends buckets recovery events by created-date over the
// trailing window. Pure; `now` injected for tests.
func summarizeRecoveryTrends(events []*persistence.RecoveryEvent, now time.Time, days int) recoveryTrendStats {
	if days <= 0 {
		days = trendDays
	}
	s := recoveryTrendStats{Sample: len(events), Days: days}
	today := truncateDate(now)
	start := today.AddDate(0, 0, -(days - 1))

	s.Buckets = make([]recoveryBucket, days)
	idx := make(map[string]int, days)
	for i := 0; i < days; i++ {
		key := start.AddDate(0, 0, i).Format("2006-01-02")
		s.Buckets[i].Date = key
		idx[key] = i
	}

	for _, e := range events {
		if e == nil || e.CreatedAt.IsZero() {
			continue
		}
		if i, ok := idx[truncateDate(e.CreatedAt).Format("2006-01-02")]; ok {
			s.Buckets[i].Count++
			s.Total++
		}
	}

	maxCount := 0
	for _, b := range s.Buckets {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}
	for i := range s.Buckets {
		s.Buckets[i].X = trendLeftPad + i*trendColW
		if maxCount > 0 {
			s.Buckets[i].BarH = s.Buckets[i].Count * trendChartH / maxCount
		}
		s.Buckets[i].BarY = trendChartTop + (trendChartH - s.Buckets[i].BarH)
	}
	s.HasData = s.Total > 0
	s.SVGWidth = trendLeftPad + days*trendColW
	s.SVGHeight = trendChartTop + trendChartH + 22
	return s
}

// spendBucket is one day's LLM spend + bar geometry.
type spendBucket struct {
	Date      string
	Cost      float64
	CostLabel string
	X         int
	BarY      int
	BarH      int
}

// spendTrendStats is the summarized daily LLM-spend time-series.
type spendTrendStats struct {
	HasData    bool
	Days       int
	Buckets    []spendBucket
	Total      float64
	TotalLabel string
	SVGWidth   int
	SVGHeight  int
}

// layoutSpendTrend builds the daily-spend chart from per-day cost totals
// (dayCosts[i] is the spend for dates[i]). Pure — the per-day SumCost I/O
// happens in the handler. Slices must be the same length.
func layoutSpendTrend(dayCosts []float64, dates []string) spendTrendStats {
	s := spendTrendStats{Days: len(dayCosts)}
	s.Buckets = make([]spendBucket, len(dayCosts))
	maxCost := 0.0
	for i, c := range dayCosts {
		s.Buckets[i].Date = dates[i]
		s.Buckets[i].Cost = c
		s.Buckets[i].CostLabel = fmt.Sprintf("$%.2f", c)
		s.Total += c
		if c > maxCost {
			maxCost = c
		}
	}
	for i := range s.Buckets {
		s.Buckets[i].X = trendLeftPad + i*trendColW
		if maxCost > 0 {
			s.Buckets[i].BarH = int(s.Buckets[i].Cost * float64(trendChartH) / maxCost)
		}
		s.Buckets[i].BarY = trendChartTop + (trendChartH - s.Buckets[i].BarH)
	}
	s.HasData = s.Total > 0
	s.TotalLabel = fmt.Sprintf("$%.2f", s.Total)
	s.SVGWidth = trendLeftPad + len(dayCosts)*trendColW
	s.SVGHeight = trendChartTop + trendChartH + 22
	return s
}

// TrendsData backs the trends template.
type TrendsData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	// AllowedProjects is the selector's options — the caller's allowed
	// projects (every project for an all-access caller). AllAccess marks
	// whether the "All" option means global (admin) vs "all mine"
	// (scoped). ProjectID "" = the All option selected.
	AllowedProjects []string
	AllAccess       bool
	Notice          string
	Stats           trendStats
	Verdicts        verdictTrendStats
	Recovery        recoveryTrendStats
	Spend           spendTrendStats
}

// InsightsTrends renders the read-only daily task-trends dashboard
// (GET /insights/trends). Track D sub-project 2.
func (s *Server) InsightsTrends(w http.ResponseWriter, r *http.Request) {
	data := TrendsData{
		Title:       "Insights — trends",
		CurrentPage: "trends",
		ProjectID:   r.URL.Query().Get("projectId"),
	}
	// Project scope: trends aggregate across whatever the query targets,
	// so a project-scoped session must never see instance-wide figures.
	// resolveProjectScope returns the projects to query + the selector
	// options. "" (the All option) = global for admins, the union of the
	// caller's projects for a scoped session.
	queryIDs, options, ok := s.resolveProjectScope(w, r, data.ProjectID)
	if !ok {
		return // 403 already written
	}
	data.AllowedProjects = options
	data.AllAccess = requestHasAllProjectAccess(r)

	if s.taskRepo == nil {
		data.Notice = "Task repository is not configured — no trend data to show."
		s.render(w, "insights_trends.html", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	tasks := s.listTasksForScope(ctx, queryIDs, persistence.TaskFilter{PageSize: trendSampleCap})
	data.Stats = summarizeTaskTrends(tasks, time.Now(), trendDays)
	if data.Stats.TotCreated == 0 {
		data.Notice = "No tasks in the recent window yet."
	}

	iter := projectsToIterate(queryIDs)

	// Judge-verdict trend (pass/fail/abstain). Merge across the scope.
	if s.judgeVerdictRepo != nil {
		var verdicts []*persistence.TaskJudgeVerdict
		for _, pid := range iter {
			if rows, vErr := s.judgeVerdictRepo.ListRecent(ctx, pid, trendSampleCap); vErr == nil {
				verdicts = append(verdicts, rows...)
			}
		}
		data.Verdicts = summarizeVerdictTrends(verdicts, time.Now(), trendDays)
	}

	// Recovery-event trend — daily graceful-recovery exits.
	if s.recoveryEventRepo != nil {
		var events []*persistence.RecoveryEvent
		for _, pid := range iter {
			if rows, rErr := s.recoveryEventRepo.ListRecent(ctx, pid, trendSampleCap); rErr == nil {
				events = append(events, rows...)
			}
		}
		data.Recovery = summarizeRecoveryTrends(events, time.Now(), trendDays)
	}

	// Spend trend — daily LLM cost, summed across the scope per day.
	if s.llmUsageRepo != nil {
		start := truncateDate(time.Now()).AddDate(0, 0, -(trendDays - 1))
		costs := make([]float64, trendDays)
		dates := make([]string, trendDays)
		for i := 0; i < trendDays; i++ {
			d := start.AddDate(0, 0, i)
			dates[i] = d.Format("2006-01-02")
			next := d.AddDate(0, 0, 1)
			var c float64
			if queryIDs == nil {
				c, _ = s.llmUsageRepo.SumCost(ctx, d, next) // global
			} else {
				for _, pid := range queryIDs {
					pc, _ := s.llmUsageRepo.SumCostByProject(ctx, pid, d, next)
					c += pc
				}
			}
			costs[i] = c
		}
		data.Spend = layoutSpendTrend(costs, dates)
	}
	s.render(w, "insights_trends.html", data)
}
