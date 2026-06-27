// Package ui: dispatch tests for the per-resource UI routers
// (swarmRouter / workflowRouter / taskRouter / projectRouter /
// memoryRouter). These pin the path/method → handler mapping
// without requiring the backing data plane.
//
// Strategy mirrors api/routes_dispatch_test.go: we don't validate
// every handler's body — those have their own tests. We just confirm
// the right handler fired for the right URL+method combo. "Dispatched"
// is confirmed by the response being anything other than http.NotFound
// with the bare "404 page not found\n" body (which is what the default
// case writes when no router arm matches).
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/mocks"
)

// helper: assert dispatcher routed (not the default 404 page).
func assertDispatched(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	if rec.Code == http.StatusNotFound &&
		strings.Contains(rec.Body.String(), "404 page not found") {
		t.Errorf("%s: dispatcher returned default NotFound page", label)
	}
}

// --- swarmRouter -----------------------------------------------------

func TestSwarmRouter_EditPathsDispatch(t *testing.T) {
	srv := NewServer()
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"edit-get", http.MethodGet, "/swarms/sw-1/edit"},
		{"edit-post", http.MethodPost, "/swarms/sw-1/edit"},
		{"delete-post", http.MethodPost, "/swarms/sw-1/delete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.swarmRouter(rec, req)
			assertDispatched(t, rec, tc.name)
		})
	}
}

func TestSwarmRouter_Unknown_NotFound(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/swarms/sw-1/banana", nil)
	rec := httptest.NewRecorder()
	srv.swarmRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// --- workflowRouter --------------------------------------------------

func TestWorkflowRouter_EditPathsDispatch(t *testing.T) {
	srv := NewServer()
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"edit-get", http.MethodGet, "/workflows/wf-1/edit"},
		{"edit-post", http.MethodPost, "/workflows/wf-1/edit"},
		{"delete-post", http.MethodPost, "/workflows/wf-1/delete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.workflowRouter(rec, req)
			assertDispatched(t, rec, tc.name)
		})
	}
}

func TestWorkflowRouter_Unknown_NotFound(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/workflows/wf-1/orange", nil)
	rec := httptest.NewRecorder()
	srv.workflowRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d", rec.Code)
	}
}

// --- taskRouter ------------------------------------------------------

func TestTaskRouter_DispatchesAllKnownSuffixes(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	// Some downstream handlers (logs/stream, events) are streaming
	// loops; we give every dispatch an already-cancelled context so
	// they return immediately. The dispatch decision is what we're
	// pinning, not the loop body.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"cancel", http.MethodPost, "/tasks/t1/cancel"},
		{"retry", http.MethodPost, "/tasks/t1/retry"},
		{"logs-stream", http.MethodGet, "/tasks/t1/logs/stream"},
		{"status", http.MethodGet, "/tasks/t1/status"},
		{"post-mortem", http.MethodPost, "/tasks/t1/post-mortem"},
		{"events", http.MethodGet, "/tasks/t1/events"},
		{"conv-message", http.MethodPost, "/tasks/t1/message"},
		{"conv-directive", http.MethodPost, "/tasks/t1/directive"},
		{"conv-answer", http.MethodPost, "/tasks/t1/messages/cp1/answer"},
		{"conv-amend", http.MethodPost, "/tasks/t1/amend"},
		{"conv-pause", http.MethodPost, "/tasks/t1/pause"},
		{"conv-resume", http.MethodPost, "/tasks/t1/resume"},
		{"conv-close", http.MethodPost, "/tasks/t1/close"},
		// Note: GET /tasks/{id} (the default case → TaskDetail) is
		// covered by dedicated tests; it requires the full project
		// registry + execution repo wiring to render without panic.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			srv.taskRouter(rec, req)
			assertDispatched(t, rec, tc.name)
		})
	}
}

// --- projectRouter ---------------------------------------------------

func TestProjectRouter_DispatchesKnownSuffixes(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"new-get", http.MethodGet, "/projects/new"},
		{"new-post", http.MethodPost, "/projects/new"},
		{"brief-get", http.MethodGet, "/projects/project-1/brief"},
		{"brief-post", http.MethodPost, "/projects/project-1/brief"},
		{"config-form-get", http.MethodGet, "/projects/project-1/config/form"},
		{"config-form-post", http.MethodPost, "/projects/project-1/config/form"},
		{"config-get", http.MethodGet, "/projects/project-1/config"},
		{"config-post", http.MethodPost, "/projects/project-1/config"},
		{"keys-get", http.MethodGet, "/projects/project-1/keys"},
		{"tasks-new-get", http.MethodGet, "/projects/project-1/tasks/new"},
		{"tasks-new-post", http.MethodPost, "/projects/project-1/tasks/new"},
		{"chat-get", http.MethodGet, "/projects/project-1/chat"},
		{"chat-post", http.MethodPost, "/projects/project-1/chat/messages"},
		{"wizard-post", http.MethodPost, "/projects/project-1/wizard"},
		{"artifacts-get", http.MethodGet, "/projects/project-1/artifacts"},
		{"artifacts-raw", http.MethodGet, "/projects/project-1/artifacts/raw"},
		{"artifacts-delete", http.MethodPost, "/projects/project-1/artifacts/delete"},
		{"detail", http.MethodGet, "/projects/project-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.projectRouter(rec, req)
			assertDispatched(t, rec, tc.name)
		})
	}
}

func TestProjectRouter_ArtifactsDelete_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	// GET on /artifacts/delete → 405 per the explicit fall-through arm.
	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/artifacts/delete", nil)
	rec := httptest.NewRecorder()
	srv.projectRouter(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", rec.Code)
	}
}

// --- memoryRouter ----------------------------------------------------

func TestMemoryRouter_DispatchesKnownSuffixes(t *testing.T) {
	srv := NewServer()
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"index", http.MethodGet, "/memory/"},
		{"index-no-slash", http.MethodGet, "/memory"},
		{"project", http.MethodGet, "/memory/p1"},
		{"rollback", http.MethodPost, "/memory/p1/rollback"},
		{"quarantine", http.MethodPost, "/memory/p1/quarantine"},
		{"inspect", http.MethodPost, "/memory/p1/inspect"},
		{"search", http.MethodGet, "/memory/p1/search"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.memoryRouter(rec, req)
			assertDispatched(t, rec, tc.name)
		})
	}
}

func TestMemoryRouter_Unknown_NotFound(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/memory/p1/banana", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d", rec.Code)
	}
}
