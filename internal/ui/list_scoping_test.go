package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// stubScopeAuditRepo records the project filter ToolAudit List ran with.
type stubScopeAuditRepo struct {
	persistence.ToolAuditRepository
	calls []string
}

func (s *stubScopeAuditRepo) List(_ context.Context, f persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if f.ProjectID == nil {
		s.calls = append(s.calls, "*")
	} else {
		s.calls = append(s.calls, *f.ProjectID)
	}
	return nil, nil
}

func TestRequestHasAllProjectAccess_Scope(t *testing.T) {
	scoped := httptest.NewRequest("GET", "/", nil)
	scoped = scoped.WithContext(api.ContextWithProjectScope(scoped.Context(), "janka"))
	if requestHasAllProjectAccess(scoped) {
		t.Error("scoped session must NOT have all-project access")
	}
	all := httptest.NewRequest("GET", "/", nil)
	all = all.WithContext(api.ContextWithProjectScope(all.Context(), "*"))
	if !requestHasAllProjectAccess(all) {
		t.Error("star-scope (admin) must have all-project access")
	}
}

// regression: 2026-06-22 — /insights/trends aggregated a global task
// sample for a project-scoped session (cross-project leak). A scoped
// session with no explicit project must default to its own project.
func TestInsightsTrends_ScopedDefaultsToOwnProject(t *testing.T) {
	repo := &stubScopeTaskRepo{byProject: map[string][]*persistence.Task{}}
	s := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/insights/trends", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "janka"))
	s.InsightsTrends(httptest.NewRecorder(), req)
	if len(repo.calls) == 0 {
		t.Fatal("expected a scoped task query")
	}
	for _, c := range repo.calls {
		if c != "janka" {
			t.Errorf("trends queried %q; scoped session must query only janka", c)
		}
	}
}

// stubScopeSpendRepo records the project ids the per-project spend
// series were queried with. Only the methods InsightsSpend touches are
// implemented; the rest panic via the embedded nil interface.
type stubScopeSpendRepo struct {
	persistence.TaskLLMUsageRepository
	sourceCalls []string
}

func (s *stubScopeSpendRepo) AggregateBySource(_ context.Context, _, _ time.Time, projectID string) ([]persistence.SourceSpend, error) {
	s.sourceCalls = append(s.sourceCalls, projectID)
	return nil, nil
}
func (s *stubScopeSpendRepo) AggregateByProject(_ context.Context, _, _ time.Time, _ int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (s *stubScopeSpendRepo) TopTasks(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (s *stubScopeSpendRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}
func (s *stubScopeSpendRepo) TimeSeriesByDay(_ context.Context, _, _ time.Time, _ string) ([]persistence.DailySpend, error) {
	return nil, nil
}

// regression: 2026-06-22 — spend's "All" silently showed only the first
// project (and would have queried globally) for a scoped user. "All my
// projects" must union the caller's projects, never query globally.
func TestSpend_AllUnionsScopedProjects(t *testing.T) {
	repo := &stubScopeSpendRepo{}
	s := NewServer(WithLLMUsageRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/spend?window=7d", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "janka", "snake"))
	s.Spend(httptest.NewRecorder(), req)
	seen := map[string]bool{}
	for _, c := range repo.sourceCalls {
		if c == "" {
			t.Fatal("spend 'All' must not query globally for a scoped user")
		}
		seen[c] = true
	}
	if !seen["janka"] || !seen["snake"] {
		t.Errorf("spend 'All' must union both projects; queried %v", repo.sourceCalls)
	}
}

func TestResolveProjectScope(t *testing.T) {
	s := NewServer()
	run := func(scope []string, explicit string) (*httptest.ResponseRecorder, []string, []string, bool) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(api.ContextWithProjectScope(req.Context(), scope...))
		rec := httptest.NewRecorder()
		q, o, ok := s.resolveProjectScope(rec, req, explicit)
		return rec, q, o, ok
	}
	// Scoped + All → union of the caller's projects (non-nil).
	if _, q, o, ok := run([]string{"janka", "snake"}, ""); !ok || len(q) != 2 || len(o) != 2 {
		t.Errorf("scoped/all: q=%v o=%v ok=%v, want both", q, o, ok)
	}
	// Scoped + explicit allowed → just that project.
	if _, q, _, ok := run([]string{"janka", "snake"}, "snake"); !ok || len(q) != 1 || q[0] != "snake" {
		t.Errorf("scoped/explicit: q=%v ok=%v, want [snake]", q, ok)
	}
	// Scoped + explicit DISALLOWED → 403, ok=false.
	if rec, _, _, ok := run([]string{"janka"}, "ibkr-trader"); ok || rec.Code != http.StatusForbidden {
		t.Errorf("scoped/disallowed: ok=%v code=%d, want 403", ok, rec.Code)
	}
	// All-access (star) + All → nil (global single query).
	if _, q, _, ok := run([]string{"*"}, ""); !ok || q != nil {
		t.Errorf("admin/all: q=%v ok=%v, want nil (global)", q, ok)
	}
}

// regression: 2026-06-22 — with two projects enabled for a scoped user,
// trends showed only the first. The "All" view must union ALL the
// caller's projects (and never query globally).
func TestInsightsTrends_AllUnionsScopedProjects(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := &stubScopeTaskRepo{byProject: map[string][]*persistence.Task{
		"janka": {tsk("j", "janka", base)},
		"snake": {tsk("s", "snake", base)},
	}}
	s := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/insights/trends", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "janka", "snake"))
	s.InsightsTrends(httptest.NewRecorder(), req)
	seen := map[string]bool{}
	for _, c := range repo.calls {
		if c == "*" {
			t.Fatal("trends 'All' must not query globally for a scoped user")
		}
		seen[c] = true
	}
	if !seen["janka"] || !seen["snake"] {
		t.Errorf("trends 'All' must union both projects; queried %v", repo.calls)
	}
}

// regression: 2026-06-22 — /insights/tool-budget had the same global-leak
// shape as trends.
func TestInsightsToolBudget_ScopedDefaultsToOwnProject(t *testing.T) {
	repo := &stubScopeAuditRepo{}
	s := NewServer(WithToolAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/insights/tool-budget", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "janka"))
	s.InsightsToolBudget(httptest.NewRecorder(), req)
	if len(repo.calls) == 0 {
		t.Fatal("expected a scoped tool-audit query")
	}
	for _, c := range repo.calls {
		if c != "janka" {
			t.Errorf("tool-budget queried %q; scoped session must query only janka", c)
		}
	}
}

// stubScopeTaskRepo records which project filters List was called with
// and returns canned per-project rows.
type stubScopeTaskRepo struct {
	persistence.TaskRepository
	byProject map[string][]*persistence.Task
	calls     []string
}

func (s *stubScopeTaskRepo) List(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
	if f.ProjectID == nil {
		s.calls = append(s.calls, "*")
		var all []*persistence.Task
		for _, v := range s.byProject {
			all = append(all, v...)
		}
		return all, nil
	}
	s.calls = append(s.calls, *f.ProjectID)
	return s.byProject[*f.ProjectID], nil
}

func tsk(id, project string, created time.Time) *persistence.Task {
	return &persistence.Task{ID: id, ProjectID: project, CreatedAt: created}
}

// regression: 2026-06-22 — a project-scoped session (Janka) saw only "2
// tasks" because the page fetched the latest N across ALL projects then
// post-filtered to allowed ones, leaving only the handful of her rows
// that landed in the global page. listTasksScoped must query per allowed
// project and merge.
func TestListTasksScoped_MergesPerProjectNewestFirstCapped(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := &stubScopeTaskRepo{byProject: map[string][]*persistence.Task{
		"janka": {tsk("j1", "janka", base.Add(3*time.Minute)), tsk("j2", "janka", base.Add(1*time.Minute))},
		"snake": {tsk("s1", "snake", base.Add(2*time.Minute))},
		// other tenant the scoped user must never see
		"ibkr-trader": {tsk("x1", "ibkr-trader", base.Add(9*time.Minute))},
	}}
	s := NewServer(WithTaskRepository(repo))

	got := s.listTasksScoped(context.Background(), []string{"janka", "snake"},
		persistence.TaskFilter{PageSize: 10})

	if len(got) != 3 {
		t.Fatalf("merged tasks = %d, want 3 (janka+snake only)", len(got))
	}
	// Newest-first across projects: j1(+3) > s1(+2) > j2(+1).
	if got[0].ID != "j1" || got[1].ID != "s1" || got[2].ID != "j2" {
		t.Errorf("order = %s,%s,%s; want j1,s1,j2", got[0].ID, got[1].ID, got[2].ID)
	}
	// Must query each scoped project explicitly and NEVER unfiltered
	// (the unfiltered global query is the leak being fixed).
	for _, c := range repo.calls {
		if c == "*" {
			t.Fatal("listTasksScoped must not issue an unfiltered (all-project) query")
		}
		if c == "ibkr-trader" {
			t.Fatal("queried a project outside the scoped set")
		}
	}
}

// stubScopeExecRepo mirrors stubScopeTaskRepo for executions.
type stubScopeExecRepo struct {
	persistence.ExecutionRepository
	byProject map[string][]*persistence.Execution
	calls     []string
}

func (s *stubScopeExecRepo) List(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	if f.ProjectID == nil {
		s.calls = append(s.calls, "*")
		var all []*persistence.Execution
		for _, v := range s.byProject {
			all = append(all, v...)
		}
		return all, nil
	}
	s.calls = append(s.calls, *f.ProjectID)
	return s.byProject[*f.ProjectID], nil
}

// regression: 2026-06-22 — a project-scoped session (Janka) saw only "3
// executions" because the page fetched the latest N across ALL projects
// then post-filtered, dropping her rows past the global page boundary.
// listExecutionsScoped must query per allowed project and merge.
func TestListExecutionsScoped_MergesPerProject(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := &stubScopeExecRepo{byProject: map[string][]*persistence.Execution{
		"janka":       {{ID: "e1", ProjectID: "janka", CreatedAt: base.Add(2 * time.Minute)}},
		"snake":       {{ID: "e2", ProjectID: "snake", CreatedAt: base.Add(3 * time.Minute)}},
		"ibkr-trader": {{ID: "x", ProjectID: "ibkr-trader", CreatedAt: base.Add(9 * time.Minute)}},
	}}
	s := NewServer(WithExecutionRepository(repo))
	got := s.listExecutionsScoped(context.Background(), []string{"janka", "snake"},
		persistence.ExecutionFilter{PageSize: 10})
	if len(got) != 2 || got[0].ID != "e2" || got[1].ID != "e1" {
		t.Fatalf("merged execs = %v, want [e2 e1] (snake newer)", got)
	}
	for _, c := range repo.calls {
		if c == "*" || c == "ibkr-trader" {
			t.Fatalf("scoped exec list issued an out-of-scope query: %q", c)
		}
	}
}

func TestListTasksScoped_CapsToPageSize(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := &stubScopeTaskRepo{byProject: map[string][]*persistence.Task{
		"janka": {tsk("a", "janka", base.Add(4*time.Minute)), tsk("b", "janka", base.Add(3*time.Minute))},
		"snake": {tsk("c", "snake", base.Add(2*time.Minute)), tsk("d", "snake", base.Add(1*time.Minute))},
	}}
	s := NewServer(WithTaskRepository(repo))
	got := s.listTasksScoped(context.Background(), []string{"janka", "snake"},
		persistence.TaskFilter{PageSize: 2})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("cap: got %d rows (%v), want 2 newest [a b]", len(got), got)
	}
}
