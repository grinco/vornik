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

// TestLiveNow_RendersRunningExecutions — the fleet view lists non-terminal
// executions as tiles linking to the per-execution live page.
func TestLiveNow_RendersRunningExecutions(t *testing.T) {
	now := time.Now()
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			// Only return a row for the RUNNING query so the test asserts a
			// single, deterministic card.
			if f.Status == nil || *f.Status != persistence.ExecutionStatusRunning {
				return nil, nil
			}
			step := "implement"
			return []*persistence.Execution{{
				ID: "exec-1", TaskID: "task-1", ProjectID: "proj-a",
				Status: persistence.ExecutionStatusRunning, CurrentStepID: &step,
				CreatedAt: now.Add(-3 * time.Minute), UpdatedAt: now.Add(-2 * time.Second),
			}}, nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo))

	rec := httptest.NewRecorder()
	srv.LiveNow(rec, httptest.NewRequest(http.MethodGet, "/ui/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Now Running",
		`/ui/tasks/task-1/live`, // tile links to the per-execution live page
		"RUNNING",
		"implement",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q", want)
		}
	}
}

// TestLiveNow_ScopedUserSeesOwnRowsPastGlobalCap pins the cross-project
// visibility scope audit follow-up: a project-scoped session must query
// its own project(s) rather than the
// global latest-N execution slice, or its rows fall past the page cap on
// a busy multi-project instance.
func TestLiveNow_ScopedUserSeesOwnRowsPastGlobalCap(t *testing.T) {
	now := time.Now()
	step := "implement"
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			if f.Status == nil || *f.Status != persistence.ExecutionStatusRunning {
				return nil, nil
			}
			if f.ProjectID != nil && *f.ProjectID == "p1" {
				return []*persistence.Execution{{
					ID: "mine", TaskID: "task-mine", ProjectID: "p1",
					Status: persistence.ExecutionStatusRunning, CurrentStepID: &step,
					CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
				}}, nil
			}
			if f.ProjectID == nil || *f.ProjectID == "" {
				bulk := make([]*persistence.Execution, 0, f.PageSize)
				for i := 0; i < f.PageSize; i++ {
					bulk = append(bulk, &persistence.Execution{
						ID: "other", TaskID: "task-other", ProjectID: "p2",
						Status: persistence.ExecutionStatusRunning, CurrentStepID: &step,
						CreatedAt: now, UpdatedAt: now,
					})
				}
				return bulk, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo))
	req := scopedUIRequest(http.MethodGet, "/ui/live", []string{"p1"})
	rec := httptest.NewRecorder()
	srv.LiveNow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/tasks/task-mine/live") {
		t.Fatalf("scoped caller must see their own running execution past the global cap:\n%s", body)
	}
	if strings.Contains(body, "/ui/tasks/task-other/live") {
		t.Fatal("foreign-project execution leaked into a scoped live view")
	}
}

// TestLiveNow_EmptyState — no running executions renders the idle empty state,
// not an error.
func TestLiveNow_EmptyState(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo))

	rec := httptest.NewRecorder()
	srv.LiveNow(rec, httptest.NewRequest(http.MethodGet, "/ui/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nothing running") {
		t.Errorf("expected idle empty state, got:\n%s", rec.Body.String())
	}
}
