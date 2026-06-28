package ui

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// alreadyOnboardedDetector returns a Detector whose heuristic considers
// the install fully configured, so the dashboard does not redirect to
// /ui/setup during non-setup tests.
func alreadyOnboardedDetector() onboarding.Detector {
	return onboarding.Detector{
		Config: &config.Config{
			Chat: config.ChatConfig{
				Endpoint: "http://localhost:11434",
				Model:    "test-model",
			},
			Telegram: config.TelegramConfig{
				DispatcherProjectID: "default",
			},
		},
	}
}

// TestParsePageSize_DefaultsAndAllowlist covers every reachable
// branch of the shared validator. The four "valid" cases must
// pass through; everything else must fall back to the audit-
// derived default of 20.
//
// The defensive shape — strict allowlist, not a numeric range —
// is load-bearing: this value flows directly into a SQL LIMIT.
// A range check would let ?limit=99999 succeed; an allowlist
// caps the worst-case scan at PageSizeOptions's max.
func TestParsePageSize_DefaultsAndAllowlist(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"empty falls back to default", "", DefaultPageSize},
		{"valid 10", "10", 10},
		{"valid 20", "20", 20},
		{"valid 50", "50", 50},
		{"valid 100", "100", 100},
		{"non-numeric falls back", "abc", DefaultPageSize},
		{"out-of-set 25 falls back", "25", DefaultPageSize},
		{"out-of-set 75 falls back", "75", DefaultPageSize},
		{"negative falls back", "-10", DefaultPageSize},
		{"zero falls back", "0", DefaultPageSize},
		{"hostile huge falls back", "999999999", DefaultPageSize},
		{"whitespace falls back", " 20 ", DefaultPageSize},
		{"hex falls back", "0x10", DefaultPageSize},
		{"float falls back", "20.0", DefaultPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePageSize(tc.raw)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPageSizeOptions_StableContract guards the public option
// set so a refactor doesn't silently change the menu rendered
// in every list view's header. If you intend to add 200, update
// this test deliberately — it's the canonical record of the UX
// contract.
func TestPageSizeOptions_StableContract(t *testing.T) {
	assert.Equal(t, []int{10, 20, 50, 100}, PageSizeOptions,
		"PageSizeOptions is the shared menu — changing it shifts the chrome on every list view")
	assert.Equal(t, 20, DefaultPageSize,
		"DefaultPageSize matches the audit page (the canonical implementation)")
}

// --- helpers shared by the per-page handler tests below ---

// pageSizeSelectorRegex matches the shared partial's rendered
// <select> on any list page. The handler tests below assert the
// chrome is present (i.e. the partial is correctly wired) by
// matching this once per page.
//
// Pattern: any <select name="limit" ...> followed by four <option>
// elements for 10, 20, 50, 100 (the canonical menu). Whitespace
// between elements is permissive — the renderer doesn't promise
// pretty-printed HTML, only that the elements exist.
var pageSizeSelectorRegex = regexp.MustCompile(
	`(?s)<select[^>]*\bname="limit"[^>]*>.*?` +
		`<option value="10".*?` +
		`<option value="20".*?` +
		`<option value="50".*?` +
		`<option value="100".*?` +
		`</select>`,
)

// assertSelectorRendered runs the regex against body and surfaces
// a useful error excerpt when it doesn't match. Common failure
// mode: a previous render-error left an HTML fragment that
// stopped before the selector, so showing the tail of the body
// helps the operator see where the break happened.
func assertSelectorRendered(t *testing.T, body, pageName string) {
	t.Helper()
	if !pageSizeSelectorRegex.MatchString(body) {
		// Show enough of the body to spot where the render
		// stopped without dumping the entire HTML.
		tail := body
		if len(tail) > 2000 {
			tail = "…" + tail[len(tail)-2000:]
		}
		t.Errorf("%s: page-size selector with options 10/20/50/100 not found in body. Tail:\n%s",
			pageName, tail)
	}
}

// --- /ui/audit ---

// auditRepoStub returns audit entries up to PageSize. The handler
// passes the validated limit through to PageSize verbatim, so this
// test fixture lets us assert "operator-supplied limit reaches the
// repo".
type auditRepoStub struct {
	entries []*persistence.ToolAuditEntry
}

func (a *auditRepoStub) Log(context.Context, *persistence.ToolAuditEntry) error { return nil }
func (a *auditRepoStub) List(ctx context.Context, f persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if f.PageSize > 0 && f.PageSize < len(a.entries) {
		return a.entries[:f.PageSize], nil
	}
	return a.entries, nil
}
func (a *auditRepoStub) CountByTool(context.Context, string) (map[string]int64, error) {
	return nil, nil
}

// makeAuditEntries returns n entries seeded with deterministic IDs
// so a test can assert "we got exactly k of them".
func makeAuditEntries(n int) []*persistence.ToolAuditEntry {
	out := make([]*persistence.ToolAuditEntry, n)
	for i := 0; i < n; i++ {
		out[i] = &persistence.ToolAuditEntry{
			ID:        fmt.Sprintf("audit-%d", i),
			TaskID:    "task-1",
			ProjectID: "p1",
			ToolName:  "file_read",
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestAudit_RendersPageSizeSelector(t *testing.T) {
	srv := NewServer(WithToolAuditRepository(&auditRepoStub{}))
	req := httptest.NewRequest("GET", "/audit", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code, "body: %s", rr.Body.String())
	assertSelectorRendered(t, rr.Body.String(), "audit")
}

func TestAudit_LimitHonoursOperatorChoice(t *testing.T) {
	repo := &auditRepoStub{entries: makeAuditEntries(100)}
	srv := NewServer(WithToolAuditRepository(repo))

	req := httptest.NewRequest("GET", "/audit?limit=50", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	// The stub trims to PageSize before returning, so a body that
	// renders ≤50 rows AND has the "50" option pre-selected proves
	// the limit threaded through to the repo. We assert the
	// selected attribute on option value="50".
	assert.Contains(t, rr.Body.String(), `<option value="50" selected`,
		"audit ?limit=50 must mark 50 as the currently-selected option")
}

func TestAudit_DefaultLimitWhenAbsent(t *testing.T) {
	srv := NewServer(WithToolAuditRepository(&auditRepoStub{}))
	req := httptest.NewRequest("GET", "/audit", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	// The default (20) is pre-selected when ?limit is absent.
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

func TestAudit_InvalidLimitFallsBack(t *testing.T) {
	srv := NewServer(WithToolAuditRepository(&auditRepoStub{}))
	for _, raw := range []string{"abc", "999", "0", "-1"} {
		t.Run("raw="+raw, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/audit?limit="+raw, nil)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			require.Equal(t, 200, rr.Code, "hostile limit %q must not 500", raw)
			assert.Contains(t, rr.Body.String(), `<option value="20" selected`,
				"hostile limit %q must fall back to the default", raw)
		})
	}
}

// --- /ui/tasks ---

// tasksRepoStub plumbs PageSize → returned-slice-length the same
// way the audit stub does, so we can assert the limit reaches the
// repo without standing up Postgres.
func tasksRepoStub(rows []*persistence.Task) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if f.PageSize > 0 && f.PageSize < len(rows) {
				return rows[:f.PageSize], nil
			}
			return rows, nil
		},
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{persistence.TaskStatusCompleted: int64(len(rows))}, nil
		},
	}
}

func makeTasks(n int) []*persistence.Task {
	out := make([]*persistence.Task, n)
	for i := 0; i < n; i++ {
		out[i] = &persistence.Task{
			ID:        fmt.Sprintf("task-%d", i),
			ProjectID: "p1",
			Status:    persistence.TaskStatusCompleted,
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestTasks_RendersPageSizeSelector(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(5))))
	req := httptest.NewRequest("GET", "/tasks", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assertSelectorRendered(t, rr.Body.String(), "tasks")
}

func TestTasks_LimitHonoursOperatorChoice(t *testing.T) {
	rows := makeTasks(100)
	srv := NewServer(WithTaskRepository(tasksRepoStub(rows)))
	req := httptest.NewRequest("GET", "/tasks?limit=50", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="50" selected`)
}

func TestTasks_DefaultLimitWhenAbsent(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(0))))
	req := httptest.NewRequest("GET", "/tasks", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

func TestTasks_InvalidLimitFallsBack(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(0))))
	req := httptest.NewRequest("GET", "/tasks?limit=999", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

// TestTasks_PreservesExistingFilters proves the form-based partial
// keeps status/project_id/q/flat through a page-size change. The
// audit-style "build one URL per option" pattern silently drops
// these on submit — this regression test stops a future revert.
func TestTasks_PreservesExistingFilters(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(0))))
	req := httptest.NewRequest("GET", "/tasks?status=COMPLETED&project_id=p1&q=hello&flat=1", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	// Hidden inputs must echo each non-empty filter so the form
	// submission carries them forward.
	assert.Contains(t, body, `<input type="hidden" name="status" value="COMPLETED">`)
	assert.Contains(t, body, `<input type="hidden" name="project_id" value="p1">`)
	assert.Contains(t, body, `<input type="hidden" name="q" value="hello">`)
	assert.Contains(t, body, `<input type="hidden" name="flat" value="1">`)
}

// --- /ui/ (dashboard / landing page) ---

func TestDashboard_RendersPageSizeSelector(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(5))), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assertSelectorRendered(t, rr.Body.String(), "dashboard")
}

func TestDashboard_LimitHonoursOperatorChoice(t *testing.T) {
	rows := makeTasks(100)
	srv := NewServer(WithTaskRepository(tasksRepoStub(rows)), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/?limit=10", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="10" selected`)
}

func TestDashboard_DefaultLimitWhenAbsent(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(0))), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

func TestDashboard_InvalidLimitFallsBack(t *testing.T) {
	srv := NewServer(WithTaskRepository(tasksRepoStub(makeTasks(0))), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/?limit=bogus", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

// --- /ui/memory ---

// memoryTestRegistry stands up an N-project registry from inline
// YAML. Memory's "lists projects" semantics means we need a
// physical registry rather than a stub. Swarm + workflow live as
// markdown files with YAML frontmatter (the registry's canonical
// shape — same as testProjectCreateRegistry).
func memoryTestRegistry(t *testing.T, projectCount int) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatalf("mkdir swarms: %v", err)
	}
	swarmDoc := `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`
	if err := os.WriteFile(filepath.Join(dir, "swarms", "test.md"), []byte(swarmDoc), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	wfDoc := `---
workflowId: "test-workflow"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "do work"
terminals:
  done:
    status: "COMPLETED"
---
`
	if err := os.WriteFile(filepath.Join(dir, "workflows", "test.md"), []byte(wfDoc), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	for i := 0; i < projectCount; i++ {
		// Two-digit zero-pad so registry sort order is deterministic.
		yaml := fmt.Sprintf(`projectId: "p%02d"
displayName: "Project %02d"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
defaultPriority: 75
`, i, i)
		if err := os.WriteFile(filepath.Join(dir, "projects", fmt.Sprintf("p%02d.yaml", i)), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write project %d: %v", i, err)
		}
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load failed: %v", err)
	}
	return reg
}

func TestMemory_RendersPageSizeSelector(t *testing.T) {
	reg := memoryTestRegistry(t, 5)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest("GET", "/memory", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assertSelectorRendered(t, rr.Body.String(), "memory")
}

func TestMemory_LimitTrimsProjectRows(t *testing.T) {
	reg := memoryTestRegistry(t, 50)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest("GET", "/memory?limit=10", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	// p00..p09 must be present (10 rows); p10 must not.
	assert.Contains(t, body, "p00")
	assert.Contains(t, body, "p09")
	assert.NotContains(t, body, ">p10<", "row p10 must be trimmed at limit=10")
	assert.Contains(t, body, `<option value="10" selected`)
}

func TestMemory_DefaultLimitWhenAbsent(t *testing.T) {
	reg := memoryTestRegistry(t, 5)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest("GET", "/memory", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

func TestMemory_InvalidLimitFallsBack(t *testing.T) {
	reg := memoryTestRegistry(t, 5)
	srv := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest("GET", "/memory?limit=junk", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

// --- /ui/projects/<id> ---

// projectDetailTestRegistry — single-project version of the
// memory helper so the project-detail handler has something to
// resolve from path /projects/<id>.
func projectDetailTestRegistry(t *testing.T) *registry.Registry {
	return memoryTestRegistry(t, 1)
}

func TestProjectDetail_RendersPageSizeSelector(t *testing.T) {
	srv := NewServer(
		WithProjectRegistry(projectDetailTestRegistry(t)),
		WithTaskRepository(tasksRepoStub(makeTasks(5))),
	)
	req := httptest.NewRequest("GET", "/projects/p00", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assertSelectorRendered(t, rr.Body.String(), "project_detail")
}

func TestProjectDetail_LimitHonoursOperatorChoice(t *testing.T) {
	rows := makeTasks(100)
	srv := NewServer(
		WithProjectRegistry(projectDetailTestRegistry(t)),
		WithTaskRepository(tasksRepoStub(rows)),
	)
	req := httptest.NewRequest("GET", "/projects/p00?limit=50", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="50" selected`)
}

func TestProjectDetail_DefaultLimitWhenAbsent(t *testing.T) {
	srv := NewServer(
		WithProjectRegistry(projectDetailTestRegistry(t)),
		WithTaskRepository(tasksRepoStub(makeTasks(0))),
	)
	req := httptest.NewRequest("GET", "/projects/p00", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

func TestProjectDetail_InvalidLimitFallsBack(t *testing.T) {
	srv := NewServer(
		WithProjectRegistry(projectDetailTestRegistry(t)),
		WithTaskRepository(tasksRepoStub(makeTasks(0))),
	)
	req := httptest.NewRequest("GET", "/projects/p00?limit=999", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

// --- /ui/spend ---

// spendRepoStub is a minimal TaskLLMUsageRepository that exposes
// the methods spend.go touches. Only TopTasks is interesting for
// the page-size assertions; everything else returns empty so the
// page renders cleanly.
type spendRepoStub struct {
	topTasks []persistence.TaskSpend
}

func (s *spendRepoStub) Record(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (s *spendRepoStub) Upsert(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (s *spendRepoStub) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (s *spendRepoStub) SumCost(context.Context, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (s *spendRepoStub) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (s *spendRepoStub) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}
func (s *spendRepoStub) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (s *spendRepoStub) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (s *spendRepoStub) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (s *spendRepoStub) TopTasks(_ context.Context, _ time.Time, _ time.Time, limit int, _ string) ([]persistence.TaskSpend, error) {
	// Honor the operator-supplied row cap the same way Postgres
	// would — trim to the requested LIMIT. Test fixtures with
	// fewer rows than `limit` short-circuit through unchanged.
	if limit > 0 && limit < len(s.topTasks) {
		return s.topTasks[:limit], nil
	}
	return s.topTasks, nil
}
func (s *spendRepoStub) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}

func makeTaskSpends(n int) []persistence.TaskSpend {
	out := make([]persistence.TaskSpend, n)
	for i := 0; i < n; i++ {
		out[i] = persistence.TaskSpend{
			TaskID:    fmt.Sprintf("ts-%d", i),
			ProjectID: "p1",
			Status:    "COMPLETED",
			CostUSD:   float64(n - i),
			StepCount: 1,
		}
	}
	return out
}

func TestSpend_RendersPageSizeSelector(t *testing.T) {
	srv := NewServer(WithLLMUsageRepository(&spendRepoStub{
		topTasks: makeTaskSpends(5),
	}))
	req := httptest.NewRequest("GET", "/spend", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assertSelectorRendered(t, rr.Body.String(), "spend")
}

func TestSpend_LimitHonoursOperatorChoice(t *testing.T) {
	rows := makeTaskSpends(100)
	srv := NewServer(WithLLMUsageRepository(&spendRepoStub{topTasks: rows}))
	req := httptest.NewRequest("GET", "/spend?limit=50", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `<option value="50" selected`)
	// ts-0 and ts-49 should be in the body; ts-50 onwards trimmed.
	assert.Contains(t, body, "ts-0")
	assert.Contains(t, body, "ts-49")
	assert.NotContains(t, body, ">ts-50<", "row ts-50 must be trimmed at limit=50")
}

func TestSpend_DefaultLimitWhenAbsent(t *testing.T) {
	srv := NewServer(WithLLMUsageRepository(&spendRepoStub{topTasks: makeTaskSpends(5)}))
	req := httptest.NewRequest("GET", "/spend", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

func TestSpend_InvalidLimitFallsBack(t *testing.T) {
	srv := NewServer(WithLLMUsageRepository(&spendRepoStub{topTasks: makeTaskSpends(5)}))
	req := httptest.NewRequest("GET", "/spend?limit=999", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `<option value="20" selected`)
}

// TestSpend_PreservesWindowAndProject — the spend page has window +
// project filters at the top of the page. Changing page size must
// keep both so the operator's drill-down isn't reset.
func TestSpend_PreservesWindowAndProject(t *testing.T) {
	srv := NewServer(WithLLMUsageRepository(&spendRepoStub{topTasks: makeTaskSpends(5)}))
	req := httptest.NewRequest("GET", "/spend?window=30d&project=p1", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	// The selector form must echo both filters as hidden inputs so
	// they survive a page-size submission. Look for them inside the
	// shared partial's form element rather than anywhere on the
	// page (the top-of-page <form> also carries window + project).
	formStart := strings.Index(body, `<form method="GET" action="/ui/spend"`)
	require.GreaterOrEqual(t, formStart, 0, "spend page-size selector form not found")
	// Take the slice from the form opening forward and verify the
	// hidden inputs sit inside it.
	formSlice := body[formStart:]
	assert.Contains(t, formSlice, `<input type="hidden" name="window" value="30d">`)
	assert.Contains(t, formSlice, `<input type="hidden" name="project" value="p1">`)
}

func (s *spendRepoStub) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (s *spendRepoStub) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
