package ui

// Tool-Budget Insights — UI Design Refresh Track D, sub-project 1.
// See https://docs.vornik.io
//
// Read-only panel: the distribution of actual tool-calls per execution
// (from tool_audit_log) against the complexity-tier budget reference. The
// per-execution tier isn't persisted, so this shows actual usage + the tier
// reference rather than a per-run join. summarizeToolCalls is pure and the
// unit-test core; the handler is a thin List → summarize → render wrapper.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// toolBudgetSampleCap bounds the recent tool-call sample the panel reads.
// List orders created_at DESC, so this is the most-recent N records.
const toolBudgetSampleCap = 5000

// histBucket is one bar of the per-execution tool-call histogram. Geometry
// (Y / BarW / CountX) is precomputed so the SVG template stays arithmetic-free.
type histBucket struct {
	Label  string
	Count  int
	Pct    int // bar width %, relative to the largest bucket
	Y      int // row top (px)
	BarW   int // bar width (px)
	CountX int // x for the count label (px)
}

// toolBudgetStats is the summarized actual-usage distribution.
type toolBudgetStats struct {
	Sample     int // tool-call records examined
	Executions int
	TotalCalls int
	Mean       float64
	P50        int
	P95        int
	Max        int
	Buckets    []histBucket
	SVGHeight  int
}

// Histogram geometry constants (px).
const (
	histRowH    = 28
	histTop     = 6
	histBarX    = 70
	histMaxBarW = 300
)

// bucketEdges defines the fixed histogram buckets by upper bound (inclusive);
// the last bucket (max 0) is the "100+" catch-all.
var bucketEdges = []struct {
	label string
	max   int
}{
	{"1", 1}, {"2-5", 5}, {"6-10", 10}, {"11-25", 25},
	{"26-50", 50}, {"51-100", 100}, {"100+", 0},
}

// summarizeToolCalls groups audit entries by execution id and computes the
// per-execution tool-call distribution. Pure — no I/O.
func summarizeToolCalls(entries []*persistence.ToolAuditEntry) toolBudgetStats {
	s := toolBudgetStats{Sample: len(entries)}
	s.Buckets = make([]histBucket, len(bucketEdges))
	for i, e := range bucketEdges {
		s.Buckets[i].Label = e.label
	}

	perExec := map[string]int{}
	for _, e := range entries {
		perExec[e.ExecutionID]++
	}
	if len(perExec) == 0 {
		return s
	}

	counts := make([]int, 0, len(perExec))
	for _, c := range perExec {
		counts = append(counts, c)
		s.TotalCalls += c
		bucketFor(c, s.Buckets)
	}
	sort.Ints(counts)

	s.Executions = len(counts)
	s.Mean = float64(s.TotalCalls) / float64(s.Executions)
	s.P50 = percentile(counts, 50)
	s.P95 = percentile(counts, 95)
	s.Max = counts[len(counts)-1]

	// Bar widths relative to the largest bucket.
	maxBucket := 0
	for _, b := range s.Buckets {
		if b.Count > maxBucket {
			maxBucket = b.Count
		}
	}
	for i := range s.Buckets {
		if maxBucket > 0 {
			s.Buckets[i].Pct = s.Buckets[i].Count * 100 / maxBucket
		}
		s.Buckets[i].BarW = s.Buckets[i].Pct * histMaxBarW / 100
		s.Buckets[i].Y = histTop + i*histRowH
		s.Buckets[i].CountX = histBarX + s.Buckets[i].BarW + 6
	}
	s.SVGHeight = histTop*2 + len(s.Buckets)*histRowH
	return s
}

// bucketFor increments the bucket matching a per-execution count.
func bucketFor(count int, buckets []histBucket) {
	for i, e := range bucketEdges {
		if e.max == 0 || count <= e.max {
			buckets[i].Count++
			return
		}
	}
}

// percentile returns the nearest-rank percentile of an ascending-sorted slice.
func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	// 1-indexed nearest rank: ceil(p/100 * n).
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// tierRef is one row of the complexity-tier budget reference.
type tierRef struct {
	Tier   string
	Factor string
	Note   string
}

// tierReference is the canonical complexity-tier → budget-factor reference
// (dynamic-tool-budget LLD). Qualitative + the two known anchors; live
// per-deployment factor values are a follow-on.
func tierReference() []tierRef {
	return []tierRef{
		{"trivial", "tight", "quick edits — fewest iterations"},
		{"standard", "0.5×", "default when the planner omits a tier"},
		{"complex", "1.0×", "the role's configured baseline (anchor)"},
		{"open_ended", "high", "deep/iterative work — most iterations"},
	}
}

// budgetAdvisoryRow is one role's over/under provisioning flag for the
// "Learned provisioning flags" advisory block on the tool-budget insights page.
// Direction is "over_provisioned" or "under_provisioned".
type budgetAdvisoryRow struct {
	Role         string
	Direction    string
	Confidence   string // "0.74"
	SupportCount int
	Label        string // human-friendly direction label
}

// budgetAdvisoryBlock is the full advisory section passed to the template.
// Rows are sorted by role then direction for stable rendering.
type budgetAdvisoryBlock struct {
	Rows []budgetAdvisoryRow
}

// budgetTrigger is the minimal trigger shape used for budget-domain instincts.
// Parsed from the Trigger JSON stored in the persistence row.
type budgetTrigger struct {
	Role   string `json:"role"`
	Signal string `json:"signal"`
}

// loadBudgetAdvisory reads active/promoted budget-domain instincts from the
// repo and returns the advisory block. Returns nil when the repo is nil, when
// there are no matching rows, or on error (fail-soft). projectID may be "" to
// read all projects.
func loadBudgetAdvisory(ctx context.Context, repo persistence.InstinctRepository, projectID string) *budgetAdvisoryBlock {
	if repo == nil {
		return nil
	}
	domain := persistence.InstinctDomainBudget
	// Read active instincts.
	activeStatus := persistence.InstinctStatusActive
	filterActive := persistence.InstinctFilter{
		Domain: &domain,
		Status: &activeStatus,
	}
	if projectID != "" {
		filterActive.ProjectID = &projectID
	}
	activeRows, err := repo.List(ctx, filterActive)
	if err != nil {
		// fail-soft: return nil so the page renders without the block
		return nil
	}
	// Read promoted instincts.
	promotedStatus := persistence.InstinctStatusPromoted
	filterPromoted := persistence.InstinctFilter{
		Domain: &domain,
		Status: &promotedStatus,
	}
	if projectID != "" {
		filterPromoted.ProjectID = &projectID
	}
	promotedRows, err := repo.List(ctx, filterPromoted)
	if err != nil {
		return nil
	}

	activeRows = append(activeRows, promotedRows...)
	if len(activeRows) == 0 {
		return nil
	}

	rows := make([]budgetAdvisoryRow, 0, len(activeRows))
	for _, in := range activeRows {
		if in == nil {
			continue
		}
		var trig budgetTrigger
		if len(in.Trigger) > 0 {
			if jerr := json.Unmarshal(in.Trigger, &trig); jerr != nil {
				continue
			}
		}
		if trig.Role == "" || trig.Signal == "" {
			continue
		}
		label := directionLabel(trig.Signal)
		rows = append(rows, budgetAdvisoryRow{
			Role:         trig.Role,
			Direction:    trig.Signal,
			Confidence:   fmt.Sprintf("%.2f", in.Confidence),
			SupportCount: in.SupportCount,
			Label:        label,
		})
	}

	if len(rows) == 0 {
		return nil
	}

	// Sort by role then direction for stable rendering.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Role != rows[j].Role {
			return rows[i].Role < rows[j].Role
		}
		return rows[i].Direction < rows[j].Direction
	})
	return &budgetAdvisoryBlock{Rows: rows}
}

// directionLabel returns a human-friendly label for an over/under signal.
func directionLabel(signal string) string {
	switch signal {
	case "over_provisioned":
		return "over-provisioned"
	case "under_provisioned":
		return "under-provisioned"
	default:
		return signal
	}
}

// ToolBudgetInsightsData backs the insights template.
type ToolBudgetInsightsData struct {
	Title           string
	CurrentPage     string
	ProjectID       string
	AllowedProjects []string // selector options (caller's allowed projects)
	AllAccess       bool     // "All" = global (admin) vs "all mine" (scoped)
	Notice          string
	Stats           toolBudgetStats
	Tiers           []tierRef
	// Advisory is the learned provisioning flags block; nil when no budget
	// instincts are present or the instinct repo is not wired.
	Advisory *budgetAdvisoryBlock
}

// InsightsToolBudget renders the read-only tool-usage-vs-budget panel
// (GET /insights/tool-budget). Track D sub-project 1.
func (s *Server) InsightsToolBudget(w http.ResponseWriter, r *http.Request) {
	data := ToolBudgetInsightsData{
		Title:       "Insights — tool budget",
		CurrentPage: "insights",
		ProjectID:   r.URL.Query().Get("projectId"),
		Tiers:       tierReference(),
	}
	// Project scope (see InsightsTrends): the "All" option means global
	// for admins, the union of the caller's projects for a scoped session
	// — never a global sample to a scoped user.
	queryIDs, options, ok := s.resolveProjectScope(w, r, data.ProjectID)
	if !ok {
		return // 403 already written
	}
	data.AllowedProjects = options
	data.AllAccess = requestHasAllProjectAccess(r)

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	if s.auditRepo == nil {
		data.Notice = "Tool-audit repository is not configured — no usage data to show."
	} else {
		var entries []*persistence.ToolAuditEntry
		var listErr error
		if queryIDs == nil {
			entries, listErr = s.auditRepo.List(ctx, persistence.ToolAuditFilter{PageSize: toolBudgetSampleCap})
		} else {
			for _, pid := range queryIDs {
				p := pid
				rows, e := s.auditRepo.List(ctx, persistence.ToolAuditFilter{ProjectID: &p, PageSize: toolBudgetSampleCap})
				if e != nil {
					listErr = e
					break
				}
				entries = append(entries, rows...)
			}
		}
		if listErr != nil {
			data.Notice = "Failed to read tool-audit data: " + listErr.Error()
		} else {
			data.Stats = summarizeToolCalls(entries)
			if data.Stats.Executions == 0 {
				data.Notice = "No tool-call audit data yet."
			}
		}
	}

	// Advisory block: populated whenever the instinct repo is wired,
	// regardless of whether tool-audit data is available. Nil-guarded
	// inside loadBudgetAdvisory. Uses the explicit project (advisory is
	// per-project; for the All view it stays daemon/global as before).
	data.Advisory = loadBudgetAdvisory(ctx, s.instinctRepo, data.ProjectID)

	s.render(w, "insights_tool_budget.html", data)
}
