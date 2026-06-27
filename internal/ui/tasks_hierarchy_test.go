package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// stringPtr is a tiny helper used by these tests to take the address
// of a string literal without polluting the production code.
func stringPtr(s string) *string { return &s }

// TestBuildHierarchyMeta_DepthOnPage verifies that depth is computed
// against the page's task set and capped at the configured max to
// avoid runaway walks if a self-cycle slips in.
func TestBuildHierarchyMeta_DepthOnPage(t *testing.T) {
	root := &persistence.Task{ID: "task_root"}
	child := &persistence.Task{ID: "task_child", ParentTaskID: stringPtr("task_root")}
	grand := &persistence.Task{ID: "task_grand", ParentTaskID: stringPtr("task_child")}
	// Orphan: parent not on the page. Should report depth 0 — the
	// list deliberately doesn't back-fetch ancestors that aren't
	// rendered.
	orphan := &persistence.Task{ID: "task_orphan", ParentTaskID: stringPtr("task_missing")}

	got := buildHierarchyMeta([]*persistence.Task{root, child, grand, orphan})
	cases := map[string]int{
		"task_root":   0,
		"task_child":  1,
		"task_grand":  2,
		"task_orphan": 0,
	}
	for id, want := range cases {
		if got[id].Depth != want {
			t.Errorf("%s depth = %d, want %d", id, got[id].Depth, want)
		}
	}
}

// TestBuildHierarchyMeta_CycleSafe ensures a malformed self-cycle
// in the data doesn't hang the page render — the walker must bail
// out via the visited set or the depth cap, whichever fires first.
func TestBuildHierarchyMeta_CycleSafe(t *testing.T) {
	a := &persistence.Task{ID: "task_a", ParentTaskID: stringPtr("task_b")}
	b := &persistence.Task{ID: "task_b", ParentTaskID: stringPtr("task_a")}
	got := buildHierarchyMeta([]*persistence.Task{a, b})
	// We don't care about the specific depth value, only that the
	// call returned and the depth is bounded.
	if got["task_a"].Depth > 10 || got["task_b"].Depth > 10 {
		t.Errorf("cycle should be depth-capped, got %+v", got)
	}
}

// TestGroupByParent_NestsChildren reorders the slice so on-page
// children sit immediately after their parent regardless of the
// underlying sort. Off-page parents leave the child as a root.
func TestGroupByParent_NestsChildren(t *testing.T) {
	root := &persistence.Task{ID: "task_root"}
	other := &persistence.Task{ID: "task_other"}
	child := &persistence.Task{ID: "task_child", ParentTaskID: stringPtr("task_root")}
	// Input order: child, other, root → expected: other (root by
	// virtue of no on-page parent, appears first in scan), root
	// (root, appears next), child (sits under root). The point of
	// the test is that child ends up immediately after root, *not*
	// in its original position between root candidates.
	in := []*persistence.Task{child, other, root}
	hier := buildHierarchyMeta(in)
	out := groupByParent(in, hier)
	if len(out) != 3 {
		t.Fatalf("len=%d", len(out))
	}
	order := []string{out[0].ID, out[1].ID, out[2].ID}
	want := []string{"task_other", "task_root", "task_child"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("position %d: got %s, want %s (full: %v)", i, order[i], want[i], order)
		}
	}
}

// TestGroupByParent_NoOpEmpty handles the trivial cases without
// crashing — empty slice and single-element slice both return as-is.
func TestGroupByParent_NoOpEmpty(t *testing.T) {
	if got := groupByParent(nil, nil); len(got) != 0 {
		t.Errorf("nil input -> got %v", got)
	}
	t1 := &persistence.Task{ID: "task_solo"}
	if got := groupByParent([]*persistence.Task{t1}, nil); len(got) != 1 || got[0] != t1 {
		t.Errorf("single input not returned as-is: %v", got)
	}
}

// TestViewModeURL_PinsState verifies the radio-style URL builders.
// Each call produces the URL that PINS the requested mode (flat=true
// adds ?flat=1; flat=false removes it), regardless of the request's
// current state. Other query params survive the rewrite. The /ui
// prefix that the subtree middleware strips before the handler
// runs is reattached so the emitted href doesn't 404.
func TestViewModeURL_PinsState(t *testing.T) {
	// From a nested-mode request: the Flat URL must contain flat=1,
	// the Nested URL must contain no flat param at all. Both must
	// carry the /ui prefix.
	req := httptest.NewRequest("GET", "/tasks?status=RUNNING&limit=50", nil)
	flatURL := viewModeURL(req, true)
	if !strings.HasPrefix(flatURL, "/ui/") {
		t.Errorf("flat URL must reattach /ui prefix, got %q", flatURL)
	}
	if !strings.Contains(flatURL, "flat=1") || !strings.Contains(flatURL, "status=RUNNING") || !strings.Contains(flatURL, "limit=50") {
		t.Errorf("flat URL: expected flat=1 + preserved status+limit, got %q", flatURL)
	}
	nestedURL := viewModeURL(req, false)
	if !strings.HasPrefix(nestedURL, "/ui/") {
		t.Errorf("nested URL must reattach /ui prefix, got %q", nestedURL)
	}
	if strings.Contains(nestedURL, "flat=") {
		t.Errorf("nested URL must not carry flat param, got %q", nestedURL)
	}
	if !strings.Contains(nestedURL, "status=RUNNING") {
		t.Errorf("nested URL: expected status preserved, got %q", nestedURL)
	}
	// From a flat-mode request: same outputs (URLs pin, they don't toggle).
	req2 := httptest.NewRequest("GET", "/tasks?flat=1&status=RUNNING", nil)
	if got := viewModeURL(req2, true); !strings.Contains(got, "flat=1") {
		t.Errorf("flat→flat URL must still contain flat=1, got %q", got)
	}
	if got := viewModeURL(req2, false); strings.Contains(got, "flat=") {
		t.Errorf("flat→nested URL must drop flat param, got %q", got)
	}
}

// TestTasksHandler_RendersHierarchy is an end-to-end render test —
// the handler decorates tasks with the child-count pill and the
// indent style, and the template surfaces them.
func TestTasksHandler_RendersHierarchy(t *testing.T) {
	now := time.Now().UTC()
	root := &persistence.Task{ID: "task_root", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: now.Add(-2 * time.Hour)}
	child := &persistence.Task{ID: "task_child", ProjectID: "p1", Status: persistence.TaskStatusCompleted, ParentTaskID: stringPtr("task_root"), CreatedAt: now.Add(-1 * time.Hour)}

	mockRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{root, child}, nil
		},
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{}, nil
		},
		CountChildrenForParentsFunc: func(ctx context.Context, ids []string) (map[string]int, error) {
			// root has one child on the page.
			return map[string]int{"task_root": 1}, nil
		},
	}

	srv := NewServer(WithTaskRepository(mockRepo))
	req := httptest.NewRequest("GET", "/tasks", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status %d body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Pill on the parent row.
	if !strings.Contains(body, "+1") {
		t.Errorf("expected child-count pill +1, body: %s", body)
	}
	// Indent style on the child row (1rem because child sits at depth 1).
	if !strings.Contains(body, "padding-left: 1rem") {
		t.Errorf("expected padding-left: 1rem on the child row, body: %s", body)
	}
	// ↳ glyph on the child row.
	if !strings.Contains(body, "↳") {
		t.Errorf("expected ↳ glyph on child row, body: %s", body)
	}
}

// TestTasksHandler_FlatModeSkipsIndent confirms ?flat=1 disables
// both the grouping and the indent — the list renders chronologically
// with no padding-left, even when parent + child are on the page.
func TestTasksHandler_FlatModeSkipsIndent(t *testing.T) {
	root := &persistence.Task{ID: "task_root", ProjectID: "p1", Status: persistence.TaskStatusRunning}
	child := &persistence.Task{ID: "task_child", ProjectID: "p1", Status: persistence.TaskStatusCompleted, ParentTaskID: stringPtr("task_root")}
	mockRepo := &mocks.MockTaskRepository{
		ListFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{root, child}, nil
		},
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{}, nil
		},
	}
	srv := NewServer(WithTaskRepository(mockRepo))
	req := httptest.NewRequest("GET", "/tasks?flat=1", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	// No indent style anywhere in flat mode.
	if strings.Contains(body, "padding-left: 1rem") {
		t.Errorf("flat mode should not emit indent style; body: %s", body)
	}
}

func TestTasks_FiltersRowsAndCountsByScopedAPIKey(t *testing.T) {
	rows := []*persistence.Task{
		{ID: "task-a", ProjectID: "project-a", Status: persistence.TaskStatusCompleted, CreatedAt: time.Now()},
		{ID: "task-b", ProjectID: "project-b", Status: persistence.TaskStatusFailed, CreatedAt: time.Now()},
	}
	mockRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return rows, nil
		},
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{
				persistence.TaskStatusCompleted: 1,
				persistence.TaskStatusFailed:    1,
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(mockRepo))
	req := scopedUIRequest(http.MethodGet, "/tasks", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Tasks(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "task-a") {
		t.Fatalf("scoped task missing: %s", body)
	}
	if strings.Contains(body, "task-b") {
		t.Fatalf("foreign task leaked: %s", body)
	}
}

// TestTasks_ProjectFilterParamAliases pins the backward-compat fix for
// audit finding #6: the project-detail status tiles emit ?project=, the
// canonical form is ?project_id=, and both must scope the list to the
// project. Before the fix, ?project= was silently ignored and clicking a
// status tile showed every project's tasks.
func TestTasks_ProjectFilterParamAliases(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"canonical project_id", "/tasks?project_id=project-a&status=RUNNING"},
		{"legacy project", "/tasks?project=project-a&status=RUNNING"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotFilter persistence.TaskFilter
			mockRepo := &mocks.MockTaskRepository{
				ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
					gotFilter = filter
					return nil, nil
				},
				CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
					return map[persistence.TaskStatus]int64{}, nil
				},
			}
			srv := NewServer(WithTaskRepository(mockRepo))
			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			rec := httptest.NewRecorder()
			srv.Tasks(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if gotFilter.ProjectID == nil {
				t.Fatalf("project filter not applied for %q", tc.query)
			}
			if *gotFilter.ProjectID != "project-a" {
				t.Fatalf("project filter = %q, want project-a", *gotFilter.ProjectID)
			}
		})
	}
}

// TestTasks_ProjectIDWinsOverLegacyProject confirms ?project_id= takes
// precedence when both params are present.
func TestTasks_ProjectIDWinsOverLegacyProject(t *testing.T) {
	var gotFilter persistence.TaskFilter
	mockRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			gotFilter = filter
			return nil, nil
		},
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{}, nil
		},
	}
	srv := NewServer(WithTaskRepository(mockRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks?project_id=winner&project=loser", nil)
	rec := httptest.NewRecorder()
	srv.Tasks(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotFilter.ProjectID == nil || *gotFilter.ProjectID != "winner" {
		t.Fatalf("project_id should win; got %v", gotFilter.ProjectID)
	}
}

func TestTasks_RejectsUnauthorizedProjectFilter(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	req := scopedUIRequest(http.MethodGet, "/tasks?project_id=project-b", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Tasks(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}
