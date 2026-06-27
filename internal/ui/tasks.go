// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"cmp"
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

type TasksData struct {
	Title        string
	CurrentPage  string
	Tasks        []*persistence.Task
	Status       string
	ProjectID    string
	Limit        int
	LimitOptions []int
	// Search query (?q=) — matches against task ID, project ID, and the
	// extracted prompt/payload text. Empty means "no filter".
	Query string
	// Sort/Dir/BaseURL drive the sortable headers on the tasks table.
	Sort    string
	Dir     string
	BaseURL string
	// StatusCounts is the per-status row-count snapshot the template
	// uses to decide which filter pills to render. States with 0
	// rows are hidden (operator-noted UX cleanup 2026-05-08); the
	// transient-only states LEASED / PENDING / WAITING_FOR_CHILDREN
	// only show up when a worker is mid-dispatch / mid-shutdown /
	// mid-delegation, so quiet days collapse the pill row.
	// Exception: COMPLETED and FAILED ALWAYS render even at 0 so
	// the operator can re-check those filters mid-day without
	// hunting for them.
	StatusCounts map[persistence.TaskStatus]int64
	// TotalCount is the sum of StatusCounts — backs the "All" cell in
	// the redesigned filter palette so the operator sees the total
	// task count at a glance without having to add the eight numbers
	// themselves. Computed once in the handler.
	TotalCount int64
	// Hierarchy keyed by Task.ID — depth in the parent chain and
	// direct-child count. Empty (Depth=0, ChildCount=0, Indent="")
	// for tasks rendered in flat mode or with no parent/children.
	// Drives the indented list view + "Subtasks (N)" pill.
	Hierarchy map[string]TaskHierarchyMeta
	// Flat is true when the operator opted out of indented rendering
	// via ?flat=1. Template uses it to switch column header /
	// suppress the indent.
	Flat bool
	// NestedURL / FlatURL are radio-style direct links: each pins
	// the corresponding view regardless of the current state. The
	// previously-shipped FlatToggleURL was a single toggle URL,
	// which made clicking the inactive button do the right thing
	// but clicking the *active* button silently flip you to the
	// other mode — confusing as a two-button selector.
	NestedURL string
	FlatURL   string
}

// TaskHierarchyMeta carries the per-row hierarchy fields the list
// template needs. Depth is the number of ancestors *visible on the
// current page* (so a child whose parent is paginated away still
// renders at depth 0 — we don't fetch off-page ancestors just for
// indentation; users who care about cross-page lineage have the
// detail-page breadcrumb).
type TaskHierarchyMeta struct {
	Depth      int
	ChildCount int
}

// Tasks renders the tasks list page.
func (s *Server) Tasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	// Accept both ?project_id= (canonical) and ?project= (backward-compat
	// for older links). project_id wins when both are present. The
	// project-detail status tiles historically emitted ?project=, which the
	// handler silently ignored — clicking "5 RUNNING" showed every project's
	// running tasks. See https://docs.vornik.io finding #6.
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		projectID = r.URL.Query().Get("project")
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	// Same page-size options as /ui/audit for consistency — see
	// page_size.go for the shared validator + allowlist. Default 20
	// matches the audit page.
	limit := parsePageSize(r.URL.Query().Get("limit"))
	sortKey, sortDir := sortParams(r,
		[]string{"id", "project", "status", "priority", "attempt", "created"},
		"created", "desc")
	s.logger.Debug().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("status", status).
		Str("project_id", projectID).
		Str("q", query).
		Int("limit", limit).
		Msg("rendering tasks page")

	filter := persistence.TaskFilter{
		PageSize: limit,
	}

	if status != "" {
		st := persistence.TaskStatus(status)
		filter.Status = &st
	}

	if projectID != "" {
		if !api.RequestAllowsProject(r, projectID) {
			http.Error(w, "access denied to project", http.StatusForbidden)
			return
		}
		filter.ProjectID = &projectID
	}

	var tasks []*persistence.Task
	if s.taskRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var err error
		if scoped, isScoped := api.RequestScopedProjects(r); isScoped && projectID == "" {
			// Project-scoped session with no explicit project: query each
			// allowed project and merge, rather than fetching the latest N
			// across ALL projects and post-filtering (which leaves a scoped
			// user seeing only the few of their rows that land in the
			// global page — the "only 2 tasks" bug).
			tasks = s.listTasksScoped(ctx, scoped, filter)
		} else {
			tasks, err = s.taskRepo.List(ctx, filter)
			if err != nil {
				s.logger.Warn().
					Err(err).
					Str("status", status).
					Str("project_id", projectID).
					Msg("failed to load tasks for UI")
			}
		}
	} else {
		s.logger.Warn().Msg("task repository is not configured for UI")
	}
	tasks = filterTasksForRequest(r, tasks) // defense-in-depth; no-op for the scoped path

	// In-memory filter on the page slice. The repo doesn't support full-text
	// search and adding it for one UI page isn't worth the complexity — the
	// list is already capped at 100. If a user needs to scan further they
	// should narrow with status / project filters first.
	if query != "" {
		needle := strings.ToLower(query)
		filtered := tasks[:0]
		for _, t := range tasks {
			hay := strings.ToLower(t.ID + " " + t.ProjectID + " " + string(t.Payload))
			if strings.Contains(hay, needle) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	sortBy(tasks, taskColumns, sortKey, sortDir, "created")

	// Hierarchy decoration. ?flat=1 short-circuits — operators who
	// want the raw chronological list can opt out. The default
	// indented view groups subtasks under their on-page parent
	// (off-page parents are not back-fetched; the detail page's
	// breadcrumb is the authoritative cross-page lineage view).
	// In flat mode depth is held at zero so the template renders
	// rows without indent; the child-count pill still shows so the
	// operator knows which rows are parents.
	flat := r.URL.Query().Get("flat") == "1"
	hierarchy := map[string]TaskHierarchyMeta{}
	if !flat {
		hierarchy = buildHierarchyMeta(tasks)
		tasks = groupByParent(tasks, hierarchy)
	} else {
		// Pre-populate with zero meta so the child-count pill code
		// below has somewhere to write counts.
		for _, t := range tasks {
			hierarchy[t.ID] = TaskHierarchyMeta{}
		}
	}
	// Decorate with direct-child counts in one bulk query so the
	// list can render a "+N" pill on parent rows without N+1
	// GetChildren calls. Best-effort: a DB error leaves the pill
	// off rather than failing the page.
	if s.taskRepo != nil && len(tasks) > 0 {
		ids := make([]string, 0, len(tasks))
		for _, t := range tasks {
			ids = append(ids, t.ID)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if counts, err := s.taskRepo.CountChildrenForParents(ctx, ids); err == nil {
			for id, n := range counts {
				meta := hierarchy[id]
				meta.ChildCount = n
				hierarchy[id] = meta
			}
		} else {
			s.logger.Debug().Err(err).Msg("CountChildrenForParents failed; subtask pill suppressed")
		}
	}

	// Per-status counts drive the filter-pill render — pills with
	// 0 rows are hidden so the row reflects what's actually
	// happening on the daemon (transient states like LEASED only
	// appear when a worker is mid-dispatch). Best-effort: a DB
	// error returns nil counts and the template falls back to its
	// always-render path for the headline states.
	var statusCounts map[persistence.TaskStatus]int64
	if s.taskRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if counts, err := s.taskRepo.CountByStatus(ctx, projectID); err == nil {
			if projectID == "" && !requestHasAllProjectAccess(r) {
				statusCounts = countStatuses(tasks)
			} else {
				statusCounts = counts
			}
		}
	}

	var totalCount int64
	for _, n := range statusCounts {
		totalCount += n
	}

	data := TasksData{
		Title:        "Tasks",
		CurrentPage:  "tasks",
		Tasks:        tasks,
		Status:       status,
		ProjectID:    projectID,
		Limit:        limit,
		LimitOptions: PageSizeOptions,
		Query:        query,
		Sort:         sortKey,
		Dir:          sortDir,
		BaseURL:      sortBaseURL(r),
		StatusCounts: statusCounts,
		TotalCount:   totalCount,
		Hierarchy:    hierarchy,
		Flat:         flat,
		NestedURL:    viewModeURL(r, false),
		FlatURL:      viewModeURL(r, true),
	}

	s.render(w, "tasks.html", data)
}

func filterTasksForRequest(r *http.Request, tasks []*persistence.Task) []*persistence.Task {
	if requestHasAllProjectAccess(r) {
		return tasks
	}
	out := tasks[:0]
	for _, t := range tasks {
		if t != nil && api.RequestAllowsProject(r, t.ProjectID) {
			out = append(out, t)
		}
	}
	return out
}

func requestHasAllProjectAccess(r *http.Request) bool {
	return api.RequestAllowsProject(r, "__vornik_scope_probe__")
}

// listTasksScoped queries each allowed project with the base filter and
// merges the results (newest first, capped to the base page size). Used
// for a project-scoped session with no explicit project filter so the
// page reflects the union of the caller's projects rather than a global
// latest-N slice that other tenants' rows dominate.
func (s *Server) listTasksScoped(ctx context.Context, projects []string, base persistence.TaskFilter) []*persistence.Task {
	var merged []*persistence.Task
	for _, p := range projects {
		f := base
		pid := p
		f.ProjectID = &pid
		rows, err := s.taskRepo.List(ctx, f)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", p).Msg("scoped task list failed for project")
			continue
		}
		merged = append(merged, rows...)
	}
	sortByCreatedDesc(merged)
	if base.PageSize > 0 && len(merged) > base.PageSize {
		merged = merged[:base.PageSize]
	}
	return merged
}

// sortByCreatedDesc orders tasks newest-first (stable) so a merged
// multi-project page matches the single-project ORDER BY created DESC.
func sortByCreatedDesc(tasks []*persistence.Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i] == nil || tasks[j] == nil {
			return tasks[j] == nil && tasks[i] != nil
		}
		return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
	})
}

func countStatuses(tasks []*persistence.Task) map[persistence.TaskStatus]int64 {
	out := map[persistence.TaskStatus]int64{}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		out[t.Status]++
	}
	return out
}

// buildHierarchyMeta computes depth-on-current-page for every task in
// the slice. Tasks whose parent isn't on the same page render at
// depth 0 — we deliberately don't fetch off-page ancestors because
// (a) it would force the list query into a recursive CTE per page
// load, (b) the detail page's breadcrumb already covers cross-page
// lineage, and (c) any task whose parent has scrolled off is
// probably old enough that the visual indent isn't earning anything.
func buildHierarchyMeta(tasks []*persistence.Task) map[string]TaskHierarchyMeta {
	out := make(map[string]TaskHierarchyMeta, len(tasks))
	onPage := make(map[string]*persistence.Task, len(tasks))
	for _, t := range tasks {
		onPage[t.ID] = t
	}
	const maxDepth = 10 // belt-and-braces against a self-referential cycle in bad data
	for _, t := range tasks {
		depth := 0
		cur := t
		seen := map[string]bool{cur.ID: true}
		for cur.ParentTaskID != nil && *cur.ParentTaskID != "" && depth < maxDepth {
			parent, ok := onPage[*cur.ParentTaskID]
			if !ok || seen[parent.ID] {
				break
			}
			seen[parent.ID] = true
			depth++
			cur = parent
		}
		out[t.ID] = TaskHierarchyMeta{Depth: depth}
	}
	return out
}

// groupByParent reorders tasks so direct children render immediately
// after their on-page parent (preserving the relative order within
// each level). Tasks whose parent isn't on the page keep their
// original position. Effect: visually contiguous trees without
// changing the underlying sort or pagination.
func groupByParent(tasks []*persistence.Task, hierarchy map[string]TaskHierarchyMeta) []*persistence.Task {
	if len(tasks) <= 1 {
		return tasks
	}
	childrenOf := make(map[string][]*persistence.Task)
	onPage := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		onPage[t.ID] = true
	}
	var roots []*persistence.Task
	for _, t := range tasks {
		if t.ParentTaskID != nil && *t.ParentTaskID != "" && onPage[*t.ParentTaskID] {
			childrenOf[*t.ParentTaskID] = append(childrenOf[*t.ParentTaskID], t)
		} else {
			roots = append(roots, t)
		}
	}
	out := make([]*persistence.Task, 0, len(tasks))
	var walk func(*persistence.Task)
	walk = func(p *persistence.Task) {
		out = append(out, p)
		for _, c := range childrenOf[p.ID] {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	// Safety net: if the walk somehow missed rows (shouldn't happen
	// with the onPage check above, but guard against malformed
	// cycles), append the rest verbatim.
	if len(out) < len(tasks) {
		emitted := make(map[string]bool, len(out))
		for _, t := range out {
			emitted[t.ID] = true
		}
		for _, t := range tasks {
			if !emitted[t.ID] {
				out = append(out, t)
			}
		}
	}
	return out
}

// viewModeURL builds a URL that *pins* the desired view mode
// (flat=true → ?flat=1, flat=false → no flat param), preserving
// every other query param. Used by the radio-style "Nested / Flat"
// selector so each button always produces the same target
// regardless of the current state.
//
// Reattaches the /ui prefix that the subtree middleware stripped
// from r.URL.Path — without it the emitted hrefs would 404 when
// the browser follows them at the public route.
func viewModeURL(r *http.Request, flat bool) string {
	q := r.URL.Query()
	if flat {
		q.Set("flat", "1")
	} else {
		q.Del("flat")
	}
	path := r.URL.Path
	if !strings.HasPrefix(path, uiPathPrefix+"/") && path != uiPathPrefix {
		path = uiPathPrefix + path
	}
	if encoded := q.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

// taskColumns maps each sort key to a comparator. Default is "created" —
// newest first — matching the repo's natural order.
var taskColumns = map[string]func(a, b *persistence.Task) int{
	"id":       func(a, b *persistence.Task) int { return cmp.Compare(a.ID, b.ID) },
	"project":  func(a, b *persistence.Task) int { return cmp.Compare(a.ProjectID, b.ProjectID) },
	"status":   func(a, b *persistence.Task) int { return cmp.Compare(string(a.Status), string(b.Status)) },
	"priority": func(a, b *persistence.Task) int { return cmp.Compare(a.Priority, b.Priority) },
	"attempt":  func(a, b *persistence.Task) int { return cmp.Compare(a.Attempt, b.Attempt) },
	"created":  func(a, b *persistence.Task) int { return a.CreatedAt.Compare(b.CreatedAt) },
}
