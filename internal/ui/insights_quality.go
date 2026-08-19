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
	// AwaitingEvidence is MissingContract+InvalidEvidence: executions whose
	// workflow DOES declare a scoring policy but whose agents produced no
	// usable evidence for it. Kept out of MeanPercent on purpose — folding a
	// contract failure into the mean reports a low quality score for runs
	// that were never measured.
	AwaitingEvidence int
}

// QualityDiagnosticCount is one evidence-gap reason and how often it occurred.
type QualityDiagnosticCount struct {
	Diagnostic string
	Count      int
}

// QualityCoverage answers "what is this page actually able to measure" before
// it shows any number.
type QualityCoverage struct {
	Workflows        int
	Declaring        int
	Scored           int
	AwaitingEvidence int
	NotApplicable    int
}

// QualityView is the computed read model behind the three page states.
type QualityView struct {
	Coverage QualityCoverage
	// Declaring holds workflows that produced at least one row a scoring
	// policy applied to; Silent holds the ones whose every row was
	// not_applicable.
	Declaring []QualityWorkflowSummary
	Silent    []QualityWorkflowSummary
	// EvidenceGaps counts the diagnostics behind AwaitingEvidence, commonest
	// first. This is the actionable half of the page.
	EvidenceGaps []QualityDiagnosticCount
	Attention    []*persistence.ExecutionQualityScore
	// NoPolicyInScope is true when rows exist and NONE of them was scorable.
	// Distinct from "no rows": one is a configuration fact worth stating,
	// the other is an empty window.
	NoPolicyInScope bool
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
	View            QualityView
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
	data.View = summarizeExecutionQuality(rows)
	if len(rows) == 0 {
		data.Notice = "No terminal executions in the recent window yet."
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

// summarizeExecutionQuality folds score rows into the three states the page
// distinguishes: nothing in scope declares a scoring policy, executions were
// scored, and a policy is declared but the evidence never arrived.
//
// Hiding the page when the numbers are empty was the failure mode this
// replaces. On 2026-08-17 every one of 7599 rows was not_applicable and the
// page said only "no rows" — a message that reads as missing data when the
// truth was that no workflow declared a contract. The same silence would have
// concealed the opposite problem, a declared contract producing nothing, which
// is a defect rather than a configuration choice.
func summarizeExecutionQuality(rows []*persistence.ExecutionQualityScore) QualityView {
	byWorkflow := map[string]*qualityAccumulator{}
	diagnostics := map[string]int{}
	view := QualityView{}
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
					view.Attention = append(view.Attention, row)
				}
			}
		case "missing_contract":
			a.MissingContract++
			countDiagnostic(diagnostics, row.Diagnostic, "missing_contract")
			view.Attention = append(view.Attention, row)
		case "invalid_evidence":
			a.InvalidEvidence++
			countDiagnostic(diagnostics, row.Diagnostic, "invalid_evidence")
			view.Attention = append(view.Attention, row)
		case "not_applicable":
			a.NotApplicable++
		}
	}

	for _, a := range byWorkflow {
		if a.Scored > 0 {
			a.MeanPercent = int(math.Round(a.sum / float64(a.Scored) * 100))
		}
		a.AwaitingEvidence = a.MissingContract + a.InvalidEvidence
		view.Coverage.Workflows++
		view.Coverage.Scored += a.Scored
		view.Coverage.AwaitingEvidence += a.AwaitingEvidence
		view.Coverage.NotApplicable += a.NotApplicable
		if a.Scored > 0 || a.AwaitingEvidence > 0 {
			view.Coverage.Declaring++
			view.Declaring = append(view.Declaring, a.QualityWorkflowSummary)
			continue
		}
		view.Silent = append(view.Silent, a.QualityWorkflowSummary)
	}
	sort.Slice(view.Declaring, func(i, j int) bool { return view.Declaring[i].WorkflowID < view.Declaring[j].WorkflowID })
	sort.Slice(view.Silent, func(i, j int) bool { return view.Silent[i].WorkflowID < view.Silent[j].WorkflowID })

	for diagnostic, count := range diagnostics {
		view.EvidenceGaps = append(view.EvidenceGaps, QualityDiagnosticCount{Diagnostic: diagnostic, Count: count})
	}
	// Commonest first, then by name so equal counts render deterministically.
	sort.Slice(view.EvidenceGaps, func(i, j int) bool {
		if view.EvidenceGaps[i].Count != view.EvidenceGaps[j].Count {
			return view.EvidenceGaps[i].Count > view.EvidenceGaps[j].Count
		}
		return view.EvidenceGaps[i].Diagnostic < view.EvidenceGaps[j].Diagnostic
	})

	view.NoPolicyInScope = len(rows) > 0 && view.Coverage.Declaring == 0
	if len(view.Attention) > 50 {
		view.Attention = view.Attention[:50]
	}
	return view
}

// countDiagnostic buckets a row by its diagnostic, falling back to the status
// when the scorer recorded none — a gap with no reason is still a gap, and
// dropping it would understate the total the coverage panel reports.
func countDiagnostic(into map[string]int, diagnostic, status string) {
	if diagnostic == "" {
		diagnostic = status
	}
	into[diagnostic]++
}
