package ui

// Form-based task-creation surface — /ui/projects/{id}/tasks/new.
// Before this existed the UI only had an inline two-input form on
// the project-detail page (taskType + raw JSON payload). The real-
// world E2E flow drove operators to fall back to curl because the
// payload field expected hand-rolled JSON and the page reloaded
// without any redirect to the resulting task.
//
// This file adds:
//
//   - GET /ui/projects/{id}/tasks/new  — full-page form with
//     a prompt textarea, workflow dropdown filtered to those the
//     project's swarm can actually run, task-type, priority.
//   - POST handler reuses the shared taskcreate.Creator core so
//     the API path and the UI path can't drift again.
//
// On success the operator is redirected (303) to /ui/tasks/<id>
// so the new task is visible immediately. On validation failure
// the form re-renders with the operator's input preserved
// (sticky-form pattern) plus an inline error banner.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// ProjectCreateTaskFormData backs the project_create_task.html
// template. Sticky-form fields (Prompt, WorkflowID, TaskType,
// Priority) round-trip the operator's POST values when the form
// re-renders after a validation failure.
type ProjectCreateTaskFormData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	// Error renders as a red banner above the form when set.
	Error string

	// Form field values (sticky on re-render).
	Prompt     string
	WorkflowID string
	TaskType   string
	Priority   int

	// CompatibleWorkflows is the workflow-id menu rendered as
	// dropdown options. Filtered to exclude any workflow the
	// project's swarm can't run (i.e. has roles the swarm
	// doesn't declare) so the operator can't pick an incompat
	// option in the first place. Sorted alphabetically for
	// stable rendering.
	CompatibleWorkflows []string
	// DefaultWorkflowID is the project's defaultWorkflowId,
	// pre-selected when the form first renders. Empty when the
	// project hasn't pinned a default — the operator must pick.
	DefaultWorkflowID string
	// DefaultPriority is what the project YAML sets; used as
	// the initial value of the priority input.
	DefaultPriority int
}

// ProjectCreateTaskForm handles GET /ui/projects/{id}/tasks/new.
// Renders the empty form with workflow options pre-populated from
// the project's swarm-compatible set and priority pre-filled with
// the project's default. Archived projects render the form with a
// blocking banner — submitting will be rejected by Submit too.
func (s *Server) ProjectCreateTaskForm(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.buildCreateTaskFormData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil && p.IsArchived() {
			data.Error = "This project is archived and cannot accept new tasks. Unarchive from the project page to resume."
			w.WriteHeader(http.StatusConflict)
		}
	}
	// First-render defaults — operator hasn't typed anything yet.
	data.Prompt = ""
	data.WorkflowID = data.DefaultWorkflowID
	data.TaskType = defaultTaskType
	data.Priority = data.DefaultPriority
	s.render(w, "project_create_task.html", data)
}

// ProjectCreateTaskSubmit handles POST /ui/projects/{id}/tasks/new.
// Validates the form, calls the shared taskcreate.Creator, and on
// success 303-redirects to the new task's detail page. On any
// failure the form re-renders with the operator's input preserved.
func (s *Server) ProjectCreateTaskSubmit(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.buildCreateTaskFormData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "project_create_task.html", data)
		return
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil && p.IsArchived() {
			data.Error = "This project is archived and cannot accept new tasks. Unarchive from the project page to resume."
			w.WriteHeader(http.StatusConflict)
			s.render(w, "project_create_task.html", data)
			return
		}
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_create_task.html", data)
		return
	}

	// Overlay form values onto data so a re-render after a
	// validation failure shows what the operator typed.
	data.Prompt = strings.TrimSpace(r.FormValue("prompt"))
	data.WorkflowID = strings.TrimSpace(r.FormValue("workflowId"))
	data.TaskType = strings.TrimSpace(r.FormValue("taskType"))
	if data.TaskType == "" {
		data.TaskType = defaultTaskType
	}
	if raw := strings.TrimSpace(r.FormValue("priority")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			data.Priority = n
		} else {
			data.Error = "Priority must be an integer 0..100"
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, "project_create_task.html", data)
			return
		}
	} else {
		data.Priority = data.DefaultPriority
	}

	// Validate. The shared core would catch most of these too,
	// but we want surface-specific messaging (e.g. "Prompt is
	// required" rather than the core's silent acceptance — the
	// API allows prompt-less tasks, the UI doesn't).
	if data.Prompt == "" {
		data.Error = "Prompt is required."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_create_task.html", data)
		return
	}
	if data.WorkflowID == "" {
		data.Error = "Workflow is required."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_create_task.html", data)
		return
	}
	if data.Priority < 0 || data.Priority > 100 {
		data.Error = "Priority must be between 0 and 100."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_create_task.html", data)
		return
	}

	// Confirm the picked workflow is in the compatibility set.
	// The shared core enforces this too, but checking here gives
	// the operator a clearer error than the core's generic
	// "missing role(s)" message — and lets us re-render the form
	// without doing the persistence-layer round trip.
	if !containsString(data.CompatibleWorkflows, data.WorkflowID) {
		data.Error = "Selected workflow is not compatible with the project's swarm."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_create_task.html", data)
		return
	}

	if s.taskCreator == nil {
		// Defensive: the daemon container always wires this in
		// production. Without it we'd silently fail; surface a
		// clear 503 instead.
		data.Error = "Task creation is not configured on this server."
		w.WriteHeader(http.StatusServiceUnavailable)
		s.render(w, "project_create_task.html", data)
		return
	}

	task, err := s.taskCreator.Create(r.Context(), taskcreate.Params{
		ProjectID:  projectID,
		TaskType:   data.TaskType,
		Prompt:     data.Prompt,
		Priority:   data.Priority,
		WorkflowID: data.WorkflowID,
	})
	if err != nil {
		// Map known reasons to HTTP statuses. Anything else
		// renders as a generic 500.
		ce := taskcreate.AsError(err)
		status := http.StatusInternalServerError
		msg := "Failed to create task: " + err.Error()
		if ce != nil {
			msg = ce.Message
			switch ce.Reason {
			case taskcreate.ReasonValidation,
				taskcreate.ReasonWorkflowNotFound,
				taskcreate.ReasonWorkflowIncompat:
				status = http.StatusBadRequest
			case taskcreate.ReasonProjectNotFound:
				status = http.StatusNotFound
			case taskcreate.ReasonRateLimited, taskcreate.ReasonBudgetExceeded:
				status = http.StatusTooManyRequests
			}
		}
		data.Error = msg
		w.WriteHeader(status)
		s.render(w, "project_create_task.html", data)
		return
	}

	// Success — 303 (See Other) is the right status for "POST,
	// then GET this other URL." Browsers turn the redirect into
	// a fresh GET so a refresh won't re-submit the form.
	http.Redirect(w, r, "/ui/tasks/"+task.ID, http.StatusSeeOther)
}

// buildCreateTaskFormData computes the static parts of the form's
// render-state: project lookup, the swarm-compatible workflow
// menu, the project's defaults. Returns a struct with .Error set
// when the project / registry is missing; callers should
// short-circuit on that.
func (s *Server) buildCreateTaskFormData(projectID string) ProjectCreateTaskFormData {
	data := ProjectCreateTaskFormData{
		Title:       "New task: " + projectID,
		CurrentPage: "projects",
		ProjectID:   projectID,
	}
	if s.projectReg == nil {
		data.Error = "Registry is not configured on this server."
		return data
	}
	project := s.projectReg.GetProject(projectID)
	if project == nil {
		data.Error = "Project not found: " + projectID
		return data
	}
	data.DefaultWorkflowID = project.DefaultWorkflowID
	data.DefaultPriority = project.DefaultPriority
	data.CompatibleWorkflows = compatibleWorkflowsFor(s.projectReg, project)
	return data
}

// compatibleWorkflowsFor returns the sorted list of workflow IDs
// the given project's swarm can actually run. Empty when the
// swarm doesn't resolve (registry validation will have logged
// that already; the form shows an empty dropdown so the operator
// fixes the project first).
func compatibleWorkflowsFor(reg *registry.Registry, project *registry.Project) []string {
	if reg == nil || project == nil {
		return nil
	}
	swarm := reg.GetSwarm(project.SwarmID)
	if swarm == nil {
		return nil
	}
	roles := make(map[string]bool, len(swarm.Roles))
	for _, r := range swarm.Roles {
		roles[r.Name] = true
	}
	var out []string
	for _, wf := range reg.ListWorkflows() {
		if wf == nil {
			continue
		}
		if workflowRolesSatisfied(wf, roles) {
			out = append(out, wf.ID)
		}
	}
	sort.Strings(out)
	return out
}

// workflowRolesSatisfied reports whether every agent/plan step in
// wf names a role present in roles. Mirrors the doctor check's
// missingRolesForWorkflow contract but inverted for the
// "in / out" filter the form needs.
func workflowRolesSatisfied(wf *registry.Workflow, roles map[string]bool) bool {
	for _, step := range wf.Steps {
		if step.Type != "agent" && step.Type != "plan" {
			continue
		}
		if step.Role == "" {
			continue
		}
		if !roles[step.Role] {
			return false
		}
	}
	return true
}

// defaultTaskType is the value pre-filled in the task-type input
// when the form first renders. "research" is the lowest-friction
// default — every swarm has a researcher-class role and the
// payload format ({"prompt": "..."}) matches what researchers
// already expect. The operator can override it; the field is just
// an <input type="text">.
const defaultTaskType = "research"

// containsString is the tiny "is s in haystack" helper used by
// the form's workflow-compat re-check. Stdlib-free so the file
// doesn't depend on slices for one call. Named with the String
// suffix to avoid colliding with the test-only contains helper
// in pure_helpers_test.go.
func containsString(haystack []string, s string) bool {
	for _, h := range haystack {
		if h == s {
			return true
		}
	}
	return false
}
