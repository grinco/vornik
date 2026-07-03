// Regression tests for the 2026-07-03 audit finding S1: a systemic
// cross-project authorization bypass across the /ui subtree. Handlers on
// /ui/projects/{id}/…, /ui/tasks/{id}/… and /ui/executions/{id}/… routes
// loaded the target entity by URL id and acted on it WITHOUT calling
// api.RequestAllowsProject, so a caller scoped to project A could read or
// mutate project B's resources. Each test drives a scoped request against a
// foreign project and asserts the request is rejected (404 — existence is not
// leaked), matching the convention already used by the gated sibling handlers.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// scopedReq builds a request stamped with a project-scoped auth context, as
// AuthMiddleware would for a project-bound key / RoleUser session.
func scopedReq(method, target, scope, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	return r.WithContext(api.ContextWithProjectScope(r.Context(), scope))
}

func TestProjectScopeIDFromPath(t *testing.T) {
	cases := map[string]string{
		"p2":              "p2",
		"p2/brief":        "p2",
		"p2/config/form":  "p2",
		"p2/tasks/new":    "p2",
		"p2/documents/d1": "p2",
		"new":             "", // create routes carry no project
		"new/wizard":      "",
		"":                "",
	}
	for in, want := range cases {
		if got := projectScopeIDFromPath(in); got != want {
			t.Errorf("projectScopeIDFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUIRequireProjectScope(t *testing.T) {
	s := &Server{}

	// Foreign project → denied with 404, no leak.
	rec := httptest.NewRecorder()
	if s.uiRequireProjectScope(rec, scopedReq(http.MethodGet, "/projects/p2", "p1", ""), "p2") {
		t.Fatal("foreign project must be denied")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project: want 404, got %d", rec.Code)
	}

	// Own project → allowed, nothing written.
	rec = httptest.NewRecorder()
	if !s.uiRequireProjectScope(rec, scopedReq(http.MethodGet, "/projects/p1", "p1", ""), "p1") {
		t.Fatal("own project must be allowed")
	}

	// Empty projectID (non-project route) → allowed.
	rec = httptest.NewRecorder()
	if !s.uiRequireProjectScope(rec, scopedReq(http.MethodGet, "/projects/new", "p1", ""), "") {
		t.Fatal("empty projectID must be allowed")
	}

	// Unscoped / all-access caller (admin, auth-off, unscoped key) → allowed
	// for any project. No scope stamped on the request.
	rec = httptest.NewRecorder()
	if !s.uiRequireProjectScope(rec, httptest.NewRequest(http.MethodGet, "/projects/p2", nil), "p2") {
		t.Fatal("all-access caller must be allowed for any project")
	}
}

// The projectRouter carries the project id in the URL, so the scope gate can
// run centrally before dispatch. Verify a cross-project request is blocked at
// the router (404) while an own-project request passes the gate and reaches
// the handler (which 503s here only because the registry is not wired).
func TestProjectRouter_CrossProjectScopeGate(t *testing.T) {
	// Cross-project: blocked at the gate.
	s := &Server{}
	rec := httptest.NewRecorder()
	s.projectRouter(rec, scopedReq(http.MethodPost, "/projects/p2/delete-now", "p1", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project delete-now: want 404 (gate), got %d", rec.Code)
	}

	// Own project: gate passes, handler runs and hits the nil registry (503).
	rec = httptest.NewRecorder()
	s.projectRouter(rec, scopedReq(http.MethodPost, "/projects/p2/delete-now", "p2", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("own-project delete-now: want 503 (gate passed, reached handler), got %d", rec.Code)
	}
}

func TestTaskConversationAction_CrossProjectDenied(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p2", Status: persistence.TaskStatusRunning}, nil
		},
	}
	s := &Server{taskRepo: taskRepo, taskMessageRepo: &uiTcStubMsgRepo{}}

	rec := httptest.NewRecorder()
	form := url.Values{"content": []string{"hi"}}.Encode()
	s.TaskConversationAction(rec, scopedReq(http.MethodPost, "/tasks/t1/message", "p1", form))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project task action: want 404, got %d", rec.Code)
	}
}

func TestExecutionReplay_CrossProjectDenied(t *testing.T) {
	s := &Server{
		execRepo: &mocks.MockExecutionRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
				return &persistence.Execution{ID: id, ProjectID: "p2"}, nil
			},
		},
	}
	rec := httptest.NewRecorder()
	s.ExecutionReplay(rec, scopedReq(http.MethodGet, "/executions/e1/replay", "p1", ""), "e1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project replay: want 404, got %d", rec.Code)
	}
}

// ProjectConfigFormSave writes the same autonomy gates / tool allowlist as the
// admin-gated raw-YAML ProjectConfigSave, so it must require admin too (S1 /
// D2/D3): a project-scoped, non-admin caller is refused even for their own
// project.
func TestProjectConfigFormSave_RequiresAdmin(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.ProjectConfigFormSave(rec, scopedReq(http.MethodPost, "/projects/p1/config/form", "p1", ""), "p1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin config form save: want 403, got %d", rec.Code)
	}
}

func TestExecutionLive_CrossProjectDenied(t *testing.T) {
	s := &Server{
		execRepo: &mocks.MockExecutionRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
				return &persistence.Execution{ID: id, ProjectID: "p2", Status: persistence.ExecutionStatusRunning}, nil
			},
		},
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return nil, nil },
		},
	}
	rec := httptest.NewRecorder()
	s.ExecutionLive(rec, scopedReq(http.MethodGet, "/executions/e1/live", "p1", ""), "e1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project live: want 404, got %d", rec.Code)
	}
}
