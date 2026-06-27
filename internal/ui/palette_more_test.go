// Package ui: additional PaletteSearch coverage — project & task
// matchers that the static-action test doesn't reach.
package ui

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestPaletteSearch_ProjectMatches(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	req := httptest.NewRequest("GET", "/ui/palette/search?q=project-1", nil)
	rec := httptest.NewRecorder()
	srv.PaletteSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"kind":"project"`) {
		t.Errorf("expected project kind in body: %s", body)
	}
}

func TestPaletteSearch_TaskMatches(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_20260519121212_deadbeefcafe1234",
					ProjectID: "p1",
					Status:    persistence.TaskStatusRunning,
				},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := httptest.NewRequest("GET", "/ui/palette/search?q=deadbeef", nil)
	rec := httptest.NewRecorder()
	srv.PaletteSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"task"`) {
		t.Errorf("expected task kind in body: %s", rec.Body.String())
	}
}

func TestPaletteSearch_FiltersForeignTasks(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task_visible_deadbeef", ProjectID: "p1", Status: persistence.TaskStatusRunning},
				{ID: "task_foreign_deadbeef", ProjectID: "p2", Status: persistence.TaskStatusRunning},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := scopedUIRequest("GET", "/ui/palette/search?q=deadbeef", []string{"p1"})
	rec := httptest.NewRecorder()
	srv.PaletteSearch(rec, req)
	if !strings.Contains(rec.Body.String(), "task_visible_deadbeef") {
		t.Fatalf("visible task missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "task_foreign_deadbeef") {
		t.Fatalf("foreign task leaked: %s", rec.Body.String())
	}
}

func TestPaletteSearch_TaskRepoError_StillReturnsActions(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := httptest.NewRequest("GET", "/ui/palette/search?q=tasks", nil)
	rec := httptest.NewRecorder()
	srv.PaletteSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestPaletteSearch_EmptyQuery_ReturnsAllActions(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("GET", "/ui/palette/search", nil)
	rec := httptest.NewRecorder()
	srv.PaletteSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: got %d", rec.Code)
	}
	body := rec.Body.String()
	// All 6 static actions should be present.
	for _, label := range []string{"Open inbox", "All tasks", "Memory", "Audit", "Spend", "Projects"} {
		if !strings.Contains(body, label) {
			t.Errorf("missing action %q in empty-query response", label)
		}
	}
}
