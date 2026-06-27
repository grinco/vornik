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

// TestInbox_RanksAndFiltersItems — approvals/checkpoints rank above failures;
// FAILED older than 24h is excluded; each row links to the task detail.
func TestInbox_RanksAndFiltersItems(t *testing.T) {
	now := time.Now()
	staleClass := "context_timeout"
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if f.Status == nil {
				return nil, nil
			}
			switch *f.Status {
			case persistence.TaskStatusAwaitingApproval:
				return []*persistence.Task{{ID: "t-approve", ProjectID: "p1", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now}}, nil
			case persistence.TaskStatusFailed:
				return []*persistence.Task{
					{ID: "t-failed-fresh", ProjectID: "p1", LastErrorClass: &staleClass, CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour)},
					{ID: "t-failed-old", ProjectID: "p1", CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)},
				}, nil
			default:
				return nil, nil
			}
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Approval ranks before the failure.
	ai := strings.Index(body, "t-approve")
	fi := strings.Index(body, "t-failed-fresh")
	if ai < 0 || fi < 0 || ai > fi {
		t.Errorf("approval should rank before failure (ai=%d fi=%d)", ai, fi)
	}
	// 24h-stale failure excluded.
	if strings.Contains(body, "t-failed-old") {
		t.Error("FAILED older than 24h must be excluded from the inbox")
	}
	// Row links to the task detail + shows the error class.
	if !strings.Contains(body, `/ui/tasks/t-approve`) {
		t.Error("inbox row missing task-detail link")
	}
	if !strings.Contains(body, "context_timeout") {
		t.Error("failure row should surface the error class")
	}
}

// TestInbox_ScopedUserSeesOwnRowsPastGlobalCap pins the cross-project
// visibility scope audit follow-up: a project-scoped session must query
// its own project(s) directly, not
// the latest-N-across-all-projects slice. With the old global-200 query
// a busy instance's other-project rows fill the page and the scoped
// user's own actionable rows fall past the cap → invisible.
func TestInbox_ScopedUserSeesOwnRowsPastGlobalCap(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if f.Status == nil || *f.Status != persistence.TaskStatusAwaitingApproval {
				return nil, nil
			}
			// Per-project query for the caller's project surfaces their row.
			if f.ProjectID != nil && *f.ProjectID == "p1" {
				return []*persistence.Task{{ID: "mine", ProjectID: "p1", CreatedAt: now, UpdatedAt: now}}, nil
			}
			// Global query (no project filter) returns a full page of
			// OTHER-project rows that bury the caller's row past the cap.
			if f.ProjectID == nil || *f.ProjectID == "" {
				bulk := make([]*persistence.Task, 0, f.PageSize)
				for i := 0; i < f.PageSize; i++ {
					bulk = append(bulk, &persistence.Task{ID: "other", ProjectID: "p2", CreatedAt: now, UpdatedAt: now})
				}
				return bulk, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"})
	rec := httptest.NewRecorder()
	srv.Inbox(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/tasks/mine") {
		t.Fatalf("scoped caller must see their own row even when other projects fill the global page:\n%s", body)
	}
	if strings.Contains(body, "/ui/tasks/other") {
		t.Fatal("foreign-project rows leaked into a scoped inbox")
	}
}

// TestInbox_EmptyState — nothing actionable renders the all-clear state.
func TestInbox_EmptyState(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if !strings.Contains(rec.Body.String(), "All clear") {
		t.Errorf("expected all-clear empty state:\n%s", rec.Body.String())
	}
}
