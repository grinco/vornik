package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// testProjectCreateRegistry builds a registry with a coder swarm
// + two workflows. "build" is runnable by the swarm; "review"
// requires a "reviewer" role the swarm doesn't have — used by
// the incompatibility test.
func testProjectCreateRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write("swarms/coder.md", `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`)
	write("workflows/build.md", `---
workflowId: "build"
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
`)
	write("workflows/review.md", `---
workflowId: "review"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "reviewer"
    prompt: "review"
terminals:
  done:
    status: "COMPLETED"
---
`)
	write("projects/test.yaml", `projectId: "test-project"
displayName: "Test Project"
swarmId: "test-swarm"
defaultWorkflowId: "build"
defaultPriority: 75
rate_limit:
  tasks_per_minute: 1
`)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	return reg
}

// writeTestRegistryFile is preserved for shared helpers in the
// package. Other tests reference it.
func writeTestRegistryFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

var _ = writeTestRegistryFile

// newTaskFormServer builds a Server wired with a task creator
// pointing at the given mock repo + registry, suitable for
// exercising both the GET render and the POST handler.
func newTaskFormServer(t *testing.T, reg *registry.Registry, repo *mocks.MockTaskRepository) *Server {
	t.Helper()
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(repo),
		taskcreate.WithProjectRegistry(reg),
		taskcreate.WithRateLimiter(ratelimit.New()),
	)
	return NewServer(
		WithProjectRegistry(reg),
		WithTaskRepository(repo),
		WithTaskCreator(creator),
	)
}

// TestProjectCreateTaskForm_GET_RendersWorkflowDropdown covers
// the GET render: the form must include the workflow dropdown
// pre-populated with the project's compatible workflows and
// pre-select the project's default. "review" must be absent
// because the project's swarm can't run it.
func TestProjectCreateTaskForm_GET_RendersWorkflowDropdown(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	req := httptest.NewRequest(http.MethodGet, "/ui/projects/test-project/tasks/new", nil)
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskForm(rec, req, "test-project")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="prompt"`) {
		t.Error("form missing prompt textarea")
	}
	if !strings.Contains(body, `name="workflowId"`) {
		t.Error("form missing workflow dropdown")
	}
	if !strings.Contains(body, `name="taskType"`) {
		t.Error("form missing taskType input")
	}
	if !strings.Contains(body, `name="priority"`) {
		t.Error("form missing priority input")
	}
	// "build" must appear as an option, "review" must NOT (swarm-incompat).
	if !strings.Contains(body, `<option value="build"`) {
		t.Error("compatible workflow 'build' missing from dropdown")
	}
	if strings.Contains(body, `<option value="review"`) {
		t.Error("incompatible workflow 'review' should not appear in dropdown")
	}
	// Default workflow should be pre-selected.
	if !strings.Contains(body, `<option value="build" selected`) {
		t.Error("default workflow not pre-selected")
	}
}

// TestProjectCreateTaskForm_GET_UnknownProject covers the 404
// branch when the project doesn't exist.
func TestProjectCreateTaskForm_GET_UnknownProject(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	req := httptest.NewRequest(http.MethodGet, "/ui/projects/ghost/tasks/new", nil)
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskForm(rec, req, "ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestProjectCreateTaskSubmit_Valid_EnqueuesAndRedirects covers
// the happy path: a valid POST creates the task via the shared
// core and redirects with 303 to /ui/tasks/<id>.
func TestProjectCreateTaskSubmit_Valid_EnqueuesAndRedirects(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "Investigate the bug")
	form.Set("workflowId", "build")
	form.Set("taskType", "research")
	form.Set("priority", "30")

	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if repo.CallCount.Create != 1 {
		t.Fatalf("Create calls = %d, want 1", repo.CallCount.Create)
	}
	created := repo.LastCall.Task
	if created.ProjectID != "test-project" {
		t.Errorf("ProjectID = %q, want test-project", created.ProjectID)
	}
	if created.Priority != 30 {
		t.Errorf("Priority = %d, want 30", created.Priority)
	}
	if created.WorkflowID == nil || *created.WorkflowID != "build" {
		t.Errorf("WorkflowID = %v, want build", created.WorkflowID)
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "/ui/tasks/") {
		t.Errorf("Location header = %q, want /ui/tasks/...", rec.Header().Get("Location"))
	}
}

// TestProjectCreateTaskSubmit_MissingPrompt re-renders the form
// with an error banner + preserves whatever the operator did type.
func TestProjectCreateTaskSubmit_MissingPrompt(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "")
	form.Set("workflowId", "build")
	form.Set("taskType", "feature")
	form.Set("priority", "42")

	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if repo.CallCount.Create != 0 {
		t.Fatalf("Create calls = %d, want 0 (validation should reject)", repo.CallCount.Create)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "prompt is required") {
		t.Errorf("error banner missing 'Prompt is required'; body=%s", body)
	}
	// Sticky form: taskType + priority + workflow choice preserved.
	if !strings.Contains(body, `value="feature"`) {
		t.Error("taskType not preserved on re-render")
	}
	if !strings.Contains(body, `value="42"`) {
		t.Error("priority not preserved on re-render")
	}
	if !strings.Contains(body, `<option value="build" selected`) {
		t.Error("workflowId selection not preserved on re-render")
	}
}

// TestProjectCreateTaskSubmit_IncompatibleWorkflow covers the
// compatibility re-check: a workflow that exists but isn't
// runnable by the project's swarm must be rejected with an
// explicit error, not silently passed through.
func TestProjectCreateTaskSubmit_IncompatibleWorkflow(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "review this")
	form.Set("workflowId", "review") // swarm lacks 'reviewer' role
	form.Set("taskType", "research")
	form.Set("priority", "50")

	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if repo.CallCount.Create != 0 {
		t.Fatalf("Create calls = %d, want 0", repo.CallCount.Create)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "not compatible") {
		t.Errorf("error banner missing 'not compatible'; body=%s", body)
	}
}

// TestProjectCreateTaskSubmit_PriorityOutOfRange rejects 101+
// and negatives so a hostile or fat-finger form doesn't bypass
// the project's intended bounds.
func TestProjectCreateTaskSubmit_PriorityOutOfRange(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "x")
	form.Set("workflowId", "build")
	form.Set("priority", "200")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if repo.CallCount.Create != 0 {
		t.Errorf("Create calls = %d, want 0", repo.CallCount.Create)
	}
}

// TestProjectCreateTaskSubmit_NonNumericPriority — same idea,
// covering the parse-error branch in the form handler.
func TestProjectCreateTaskSubmit_NonNumericPriority(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "x")
	form.Set("workflowId", "build")
	form.Set("priority", "huge")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if repo.CallCount.Create != 0 {
		t.Errorf("Create calls = %d, want 0", repo.CallCount.Create)
	}
}

// TestProjectCreateTaskSubmit_MissingWorkflow rejects when the
// dropdown was somehow empty / cleared.
func TestProjectCreateTaskSubmit_MissingWorkflow(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "x")
	form.Set("workflowId", "")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "workflow is required") {
		t.Errorf("error banner missing 'Workflow is required'; body=%s", body)
	}
}

// TestProjectCreateTaskSubmit_NoCreatorReturns503 covers the
// nil-taskCreator branch — defensive guard for a misconfigured
// server.
func TestProjectCreateTaskSubmit_NoCreatorReturns503(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	s := NewServer(WithProjectRegistry(reg)) // no taskCreator

	form := url.Values{}
	form.Set("prompt", "x")
	form.Set("workflowId", "build")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestProjectCreateTaskSubmit_RateLimitedRendersError covers the
// case where the shared core rate-limits the request — the form
// re-renders with the rate-limit reason as the banner text and
// preserves the operator's input.
func TestProjectCreateTaskSubmit_RateLimitedRendersError(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	repo := &mocks.MockTaskRepository{}
	s := newTaskFormServer(t, reg, repo)

	form := url.Values{}
	form.Set("prompt", "first")
	form.Set("workflowId", "build")
	form.Set("taskType", "research")

	// First request — should succeed, exhausting the per-minute cap.
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("first POST status = %d, want 303", rec.Code)
	}

	// Second request — same minute, should be rate-limited.
	form.Set("prompt", "second")
	req = httptest.NewRequest(http.MethodPost, "/ui/projects/test-project/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.ProjectCreateTaskSubmit(rec, req, "test-project")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second POST status = %d, want 429", rec.Code)
	}
	if repo.CallCount.Create != 1 {
		t.Errorf("Create calls = %d, want 1 (second blocked)", repo.CallCount.Create)
	}
}

// TestCompatibleWorkflowsFor covers the swarm-compat helper
// directly: build is in, review is out.
func TestCompatibleWorkflowsFor(t *testing.T) {
	reg := testProjectCreateRegistry(t)
	project := reg.GetProject("test-project")
	got := compatibleWorkflowsFor(reg, project)
	want := []string{"build"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestCompatibleWorkflowsFor_NilRegistry returns nil rather than
// panicking — defensive contract used by other tests in the
// package that build a Server without a registry.
func TestCompatibleWorkflowsFor_NilRegistry(t *testing.T) {
	if got := compatibleWorkflowsFor(nil, nil); got != nil {
		t.Errorf("compatibleWorkflowsFor(nil, nil) = %v, want nil", got)
	}
}

// TestWorkflowRolesSatisfied_PlanStepsAndEmptyRolesSkipped — the
// helper must treat non-agent / non-plan step types as ignorable
// (gates, approvals don't need roles) and skip steps with empty
// roles. Exercises both branches in one table.
func TestWorkflowRolesSatisfied_PlanStepsAndEmptyRolesSkipped(t *testing.T) {
	wf := &registry.Workflow{
		Steps: map[string]registry.WorkflowStep{
			"a": {Type: "agent", Role: "coder"},
			"p": {Type: "plan", Role: "coder"},  // also satisfied
			"g": {Type: "gate", Role: "anyone"}, // skipped
			"e": {Type: "agent", Role: ""},      // empty role skipped
		},
	}
	roles := map[string]bool{"coder": true}
	if !workflowRolesSatisfied(wf, roles) {
		t.Error("expected satisfied=true when all roles covered")
	}

	// Now require a role the swarm lacks.
	wf.Steps["p"] = registry.WorkflowStep{Type: "plan", Role: "reviewer"}
	if workflowRolesSatisfied(wf, roles) {
		t.Error("expected satisfied=false when 'reviewer' is missing")
	}
}

// TestProjectCreateTaskForm_NoRegistry covers the case where the
// server has no registry wired — the form must render an error
// rather than panicking.
func TestProjectCreateTaskForm_NoRegistry(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/x/tasks/new", nil)
	rec := httptest.NewRecorder()
	s.ProjectCreateTaskForm(rec, req, "x")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// _ keeps the persistence and context imports honest in case
// future tests need them without re-importing.
var _ = context.Background
var _ = persistence.TaskStatusQueued
