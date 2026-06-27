package ui

import (
	"context"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// executionStatusOrder is the filter-palette order for the Executions list.
var executionStatusOrder = []persistence.ExecutionStatus{
	persistence.ExecutionStatusRunning,
	persistence.ExecutionStatusPending,
	persistence.ExecutionStatusPaused,
	persistence.ExecutionStatusCompleted,
	persistence.ExecutionStatusFailed,
	persistence.ExecutionStatusCancelled,
}

// ExecutionsData backs the cross-task Executions list page (IA completion).
type ExecutionsData struct {
	Title        string
	CurrentPage  string
	Executions   []ExecutionRow
	Status       string
	ProjectID    string
	Limit        int
	LimitOptions []int
	BaseURL      string
	// Palette is the pre-built status-filter row (non-zero statuses only),
	// so the template ranges a slice instead of calling the TaskStatus-typed
	// `index` func on an ExecutionStatus map.
	Palette    []ExecStatusPill
	TotalCount int64
	// HasLiveRows is true when at least one rendered execution is in a
	// non-terminal state. The template gates the HTMX auto-refresh on it, so
	// once a poll returns an all-terminal page the re-rendered content carries
	// no trigger and polling stops by itself.
	HasLiveRows bool
}

// ExecStatusPill is one status-filter pill on the executions palette.
type ExecStatusPill struct {
	Status persistence.ExecutionStatus
	Count  int64
}

// buildExecutionPalette returns the non-zero status pills in display order.
func buildExecutionPalette(order []persistence.ExecutionStatus, counts map[persistence.ExecutionStatus]int64) []ExecStatusPill {
	pills := make([]ExecStatusPill, 0, len(order))
	for _, st := range order {
		if n := counts[st]; n > 0 {
			pills = append(pills, ExecStatusPill{Status: st, Count: n})
		}
	}
	return pills
}

// ExecutionRow is one pre-formatted row in the executions table.
type ExecutionRow struct {
	ID          string
	TaskID      string
	ProjectID   string
	WorkflowID  string
	CurrentStep string
	Status      persistence.ExecutionStatus
	StartedAgo  string
	Duration    string
}

// buildExecutionsData formats a page of executions for the template. Pure;
// `now` bounds the duration of still-running executions so the result is
// deterministic and unit-testable.
func buildExecutionsData(execs []*persistence.Execution, now time.Time) ExecutionsData {
	rows := make([]ExecutionRow, 0, len(execs))
	hasLive := false
	for _, e := range execs {
		if e == nil {
			continue
		}
		if e.Status.IsLive() {
			hasLive = true
		}
		step := "—"
		if e.CurrentStepID != nil && *e.CurrentStepID != "" {
			step = *e.CurrentStepID
		}
		startedAgo := ""
		if e.StartedAt != nil {
			startedAgo = humanAgo(*e.StartedAt)
		}
		rows = append(rows, ExecutionRow{
			ID:          e.ID,
			TaskID:      e.TaskID,
			ProjectID:   e.ProjectID,
			WorkflowID:  e.WorkflowID,
			CurrentStep: step,
			Status:      e.Status,
			StartedAgo:  startedAgo,
			Duration:    execDuration(e, now),
		})
	}
	return ExecutionsData{
		Title:       "Executions",
		CurrentPage: "executions",
		Executions:  rows,
		HasLiveRows: hasLive,
	}
}

// execDuration renders how long an execution ran: completed→(completed-started),
// still-running→(now-started), unstarted→"". Rounded to the second.
func execDuration(e *persistence.Execution, now time.Time) string {
	if e.StartedAt == nil {
		return ""
	}
	end := now
	if e.CompletedAt != nil {
		end = *e.CompletedAt
	}
	d := end.Sub(*e.StartedAt)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// countExecutionStatuses derives palette counts from a page slice — the
// restricted-auth fallback (mirrors tasks.go countStatuses).
func countExecutionStatuses(execs []*persistence.Execution) map[persistence.ExecutionStatus]int64 {
	out := map[persistence.ExecutionStatus]int64{}
	for _, e := range execs {
		if e == nil {
			continue
		}
		out[e.Status]++
	}
	return out
}

// listExecutionsScoped queries each allowed project with the base
// filter and merges (newest first, capped to the page size) — the
// executions counterpart to listTasksScoped, for a project-scoped
// session with no explicit project filter.
func (s *Server) listExecutionsScoped(ctx context.Context, projects []string, base persistence.ExecutionFilter) []*persistence.Execution {
	var merged []*persistence.Execution
	for _, p := range projects {
		f := base
		pid := p
		f.ProjectID = &pid
		rows, err := s.execRepo.List(ctx, f)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", p).Msg("scoped execution list failed for project")
			continue
		}
		merged = append(merged, rows...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i] == nil || merged[j] == nil {
			return merged[j] == nil && merged[i] != nil
		}
		return merged[i].CreatedAt.After(merged[j].CreatedAt)
	})
	if base.PageSize > 0 && len(merged) > base.PageSize {
		merged = merged[:base.PageSize]
	}
	return merged
}

// listExecutionsForScope returns the execution sample for a resolved
// scope: nil queryIDs = a single global query; otherwise the per-project
// merge. The executions counterpart to listTasksForScope.
func (s *Server) listExecutionsForScope(ctx context.Context, queryIDs []string, filter persistence.ExecutionFilter) []*persistence.Execution {
	if queryIDs == nil {
		rows, err := s.execRepo.List(ctx, filter)
		if err != nil {
			s.logger.Warn().Err(err).Msg("scope execution list failed")
			return nil
		}
		return rows
	}
	return s.listExecutionsScoped(ctx, queryIDs, filter)
}

// ExecutionsList renders the cross-task Executions list page.
func (s *Server) ExecutionsList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		projectID = r.URL.Query().Get("project")
	}
	limit := parsePageSize(r.URL.Query().Get("limit"))

	filter := persistence.ExecutionFilter{PageSize: limit}
	if status != "" {
		st := persistence.ExecutionStatus(status)
		filter.Status = &st
	}
	if projectID != "" {
		if !api.RequestAllowsProject(r, projectID) {
			http.Error(w, "access denied to project", http.StatusForbidden)
			return
		}
		filter.ProjectID = &projectID
	}

	var execs []*persistence.Execution
	if s.execRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var err error
		if scoped, isScoped := api.RequestScopedProjects(r); isScoped && projectID == "" {
			// Scoped session, no explicit project: query per allowed
			// project and merge so the page isn't a global latest-N slice
			// dominated by other tenants (the "only 3 executions" bug).
			execs = s.listExecutionsScoped(ctx, scoped, filter)
		} else {
			execs, err = s.execRepo.List(ctx, filter)
			if err != nil {
				s.logger.Warn().Err(err).Str("status", status).Str("project_id", projectID).
					Msg("failed to load executions for UI")
			}
		}
	} else {
		s.logger.Warn().Msg("execution repository is not configured for UI")
	}

	// Auth-filter the page when the operator lacks all-project access.
	if !requestHasAllProjectAccess(r) {
		kept := execs[:0]
		for _, e := range execs {
			if e != nil && api.RequestAllowsProject(r, e.ProjectID) {
				kept = append(kept, e)
			}
		}
		execs = kept
	}

	data := buildExecutionsData(execs, time.Now())
	data.Status = status
	data.ProjectID = projectID
	data.Limit = limit
	data.LimitOptions = PageSizeOptions
	data.BaseURL = sortBaseURL(r)

	// Palette counts: accurate per-status Count() when the operator can see
	// the whole (optionally project-scoped) set; otherwise derive from the
	// auth-filtered page. Mirrors tasks.go.
	var counts map[persistence.ExecutionStatus]int64
	if s.execRepo != nil && (projectID != "" || requestHasAllProjectAccess(r)) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		counts = map[persistence.ExecutionStatus]int64{}
		for _, st := range executionStatusOrder {
			stCopy := st
			cf := persistence.ExecutionFilter{Status: &stCopy}
			if projectID != "" {
				cf.ProjectID = &projectID
			}
			if n, err := s.execRepo.Count(ctx, cf); err == nil {
				counts[st] = n
			}
		}
	} else {
		counts = countExecutionStatuses(execs)
	}
	data.Palette = buildExecutionPalette(executionStatusOrder, counts)
	for _, n := range counts {
		data.TotalCount += n
	}

	s.render(w, "executions.html", data)
}
