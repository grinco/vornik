package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestProjectDetail_EmptyPathRedirectsToList — /projects/ (no ID)
// hands off to the projects list.
func TestProjectDetail_EmptyPathRedirectsToList(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/projects/", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDetail(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	// Should render the Projects header.
	assert.Contains(t, rec.Body.String(), "Projects")
}

// TestProjectDetail_LiteralListPathHandsOffToProjects — /projects/list.
func TestProjectDetail_LiteralListPathHandsOffToProjects(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/projects/list", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDetail(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestProjectDetail_UnknownProjectReturns404 — bogus ID → 404 +
// "project not found" template.
func TestProjectDetail_UnknownProjectReturns404(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/does-not-exist", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDetail(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Project Not Found")
}

// TestProjectDetail_RendersFromRegistry — happy path with the
// swarm fixture's "p1" project.
func TestProjectDetail_RendersFromRegistry(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/p1", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// DisplayName from fixture is "P1".
	assert.Contains(t, body, "P1")
}

// TestProjectDetail_WithTasksRendersTaskList — task repo wired
// returns one task; verify it surfaces in the project's task table.
func TestProjectDetail_WithTasksRendersTaskList(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task_p1_a", ProjectID: "p1", Status: persistence.TaskStatusCompleted},
			}, nil
		},
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{persistence.TaskStatusCompleted: 1}, nil
		},
	}
	srv.taskRepo = taskRepo
	req := httptest.NewRequest(http.MethodGet, "/projects/p1", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "task_p1_a")
}

// TestProjectDetail_LimitParamRoundTrips — ?limit=10 reaches the
// page-size selector + the repo filter.
func TestProjectDetail_LimitParamRoundTrips(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	gotPageSize := 0
	srv.taskRepo = &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			gotPageSize = f.PageSize
			return nil, nil
		},
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return nil, nil
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/p1?limit=10", nil)
	rec := httptest.NewRecorder()
	srv.ProjectDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 10, gotPageSize, "limit=10 should round-trip to the repo filter")
}
