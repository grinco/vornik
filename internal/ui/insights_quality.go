package ui

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

const qualityInsightsSampleCap = 500

// QualityWorkflowSummary aggregates recent execution scores for one workflow.
type QualityWorkflowSummary struct {
	WorkflowID      string
	MeanPercent     int
	Scored          int
	MissingContract int
	InvalidEvidence int
	NotApplicable   int
}

// QualityInsightsData is the operator quality-insights page model.
type QualityInsightsData struct {
	Title           string
	CurrentPage     string
	ProjectID       string
	AllowedProjects []string
	AllAccess       bool
	WindowLabel     string
	Notice          string
	Summaries       []QualityWorkflowSummary
	Attention       []*persistence.ExecutionQualityScore
	Publication     persistence.ExecutionQualityPendingStats
}

// InsightsQuality renders project-scoped production score trends and failures.
func (s *Server) InsightsQuality(w http.ResponseWriter, r *http.Request) {
	data := QualityInsightsData{
		Title: "Insights — execution quality", CurrentPage: "quality",
		ProjectID: r.URL.Query().Get("projectId"), WindowLabel: "last 7 days",
	}
	queryIDs, options, ok := s.resolveProjectScope(w, r, data.ProjectID)
	if !ok {
		return
	}
	data.AllowedProjects = options
	data.AllAccess = requestHasAllProjectAccess(r)
	if s.executionQualityRepo == nil {
		data.Notice = "Execution quality scoring is not configured on this deployment."
		s.render(w, "insights_quality.html", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	since := time.Now().Add(-7 * 24 * time.Hour)
	rows, err := s.executionQualityRepo.List(ctx, persistence.ExecutionQualityScoreFilter{
		ProjectIDs: queryIDs, Since: &since, PageSize: qualityInsightsSampleCap,
	})
	if err != nil {
		data.Notice = "Execution quality data is temporarily unavailable."
		s.render(w, "insights_quality.html", data)
		return
	}
	data.Summaries, data.Attention = summarizeExecutionQuality(rows)
	if len(rows) == 0 {
		data.Notice = "No execution quality rows in the recent window yet."
	}
	if stats, statsErr := s.executionQualityRepo.PendingTerminalStats(ctx, queryIDs); statsErr == nil {
		data.Publication = stats
	}
	s.render(w, "insights_quality.html", data)
}

type qualityAccumulator struct {
	QualityWorkflowSummary
	sum float64
}

func summarizeExecutionQuality(rows []*persistence.ExecutionQualityScore) ([]QualityWorkflowSummary, []*persistence.ExecutionQualityScore) {
	byWorkflow := map[string]*qualityAccumulator{}
	var attention []*persistence.ExecutionQualityScore
	for _, row := range rows {
		if row == nil {
			continue
		}
		workflow := row.WorkflowID
		if workflow == "" {
			workflow = "(unknown)"
		}
		a := byWorkflow[workflow]
		if a == nil {
			a = &qualityAccumulator{QualityWorkflowSummary: QualityWorkflowSummary{WorkflowID: workflow}}
			byWorkflow[workflow] = a
		}
		switch row.Status {
		case "scored":
			if row.Score != nil {
				a.Scored++
				a.sum += *row.Score
				if *row.Score <= 0.5 {
					attention = append(attention, row)
				}
			}
		case "missing_contract":
			a.MissingContract++
			attention = append(attention, row)
		case "invalid_evidence":
			a.InvalidEvidence++
			attention = append(attention, row)
		case "not_applicable":
			a.NotApplicable++
		}
	}
	out := make([]QualityWorkflowSummary, 0, len(byWorkflow))
	for _, a := range byWorkflow {
		if a.Scored > 0 {
			a.MeanPercent = int(math.Round(a.sum / float64(a.Scored) * 100))
		}
		out = append(out, a.QualityWorkflowSummary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkflowID < out[j].WorkflowID })
	if len(attention) > 50 {
		attention = attention[:50]
	}
	return out, attention
}
