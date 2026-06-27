package ui

// /ui/workflows/new — create a new WORKFLOW.md from a blank
// starter. Sibling of /ui/swarms/new — same shape, same flow
// (form → write → reload → redirect to the editor). Starter is
// a single-step agent workflow that hits the success terminal.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// WorkflowsNewData is the model for the workflows_new.html template.
type WorkflowsNewData struct {
	Title       string
	CurrentPage string

	WorkflowID  string
	DisplayName string
	StepName    string
	RoleName    string

	Error   string
	Success string

	CreatedWorkflowID string
	CreatedPath       string
}

var workflowIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
var stepNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)

// WorkflowsNew renders the starter form. GET only.
func (s *Server) WorkflowsNew(w http.ResponseWriter, r *http.Request) {
	data := WorkflowsNewData{
		Title:       "New workflow",
		CurrentPage: "workflows",
		StepName:    "run",
		RoleName:    "assistant",
	}
	s.render(w, "workflows_new.html", data)
}

// WorkflowsCreate handles POST /ui/workflows/new. Form fields:
//
//	workflowId   — filename slug + identity field
//	displayName  — human-readable name
//	stepName     — the single starter step's id (entrypoint)
//	roleName     — role the step invokes (must exist in the
//	               project's swarm; we don't cross-validate here
//	               because workflows aren't bound to a swarm until
//	               a project picks them up)
//
// On success: writes <configsDir>/workflows/<workflowId>.md,
// reloads, redirects to /ui/workflows/<workflowId>/edit.
func (s *Server) WorkflowsCreate(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.configsDir) == "" {
		http.Error(w, "Daemon doesn't know where to write workflow files (configsDir unset)", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	data := WorkflowsNewData{
		Title:       "New workflow",
		CurrentPage: "workflows",
		WorkflowID:  strings.TrimSpace(r.FormValue("workflowId")),
		DisplayName: strings.TrimSpace(r.FormValue("displayName")),
		StepName:    strings.TrimSpace(r.FormValue("stepName")),
		RoleName:    strings.TrimSpace(r.FormValue("roleName")),
	}
	if data.StepName == "" {
		data.StepName = "run"
	}
	if data.RoleName == "" {
		data.RoleName = "assistant"
	}

	if data.WorkflowID == "" || !workflowIDPattern.MatchString(data.WorkflowID) {
		data.Error = "Workflow ID must be 3–32 chars: lowercase letters, digits, hyphens; cannot start/end with a hyphen."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflows_new.html", data)
		return
	}
	if !stepNamePattern.MatchString(data.StepName) {
		data.Error = "Step name must start with a lowercase letter; allowed: lowercase letters, digits, underscores, hyphens (≤31 chars)."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflows_new.html", data)
		return
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`).MatchString(data.RoleName) {
		data.Error = "Role name must start with a lowercase letter and contain only lowercase letters, digits, hyphens (≤31 chars)."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflows_new.html", data)
		return
	}
	if data.DisplayName == "" {
		data.DisplayName = data.WorkflowID
	}

	body := renderWorkflowStarter(data.WorkflowID, data.DisplayName, data.StepName, data.RoleName)

	parsed, perr := registry.ParseWorkflowMarkdown([]byte(body), data.WorkflowID+".md")
	if perr != nil {
		data.Error = "Internal: starter WORKFLOW.md failed self-parse: " + perr.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflows_new.html", data)
		return
	}
	if vErr := parsed.Validate(data.WorkflowID + ".md"); vErr != nil {
		data.Error = "Internal: starter WORKFLOW.md failed validation: " + vErr.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflows_new.html", data)
		return
	}

	target := filepath.Join(s.configsDir, "workflows", data.WorkflowID+".md")
	if _, err := os.Stat(target); err == nil {
		data.Error = fmt.Sprintf("A workflow at workflows/%s.md already exists. Pick a different ID or delete the existing one first.", data.WorkflowID)
		w.WriteHeader(http.StatusConflict)
		s.render(w, "workflows_new.html", data)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		data.Error = "Filesystem check failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflows_new.html", data)
		return
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		data.Error = "Mkdir failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflows_new.html", data)
		return
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		data.Error = "Write failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflows_new.html", data)
		return
	}

	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			data.Error = "Saved workflows/" + data.WorkflowID + ".md but daemon reload failed: " + err.Error() +
				"\nThe file is on disk; restart the daemon or fix the cause and retry the reload."
			w.WriteHeader(http.StatusConflict)
			s.render(w, "workflows_new.html", data)
			return
		}
	}

	http.Redirect(w, r, "/ui/workflows/"+data.WorkflowID+"/edit", http.StatusSeeOther)
}

// renderWorkflowStarter produces a minimal valid WORKFLOW.md: one
// agent step (the entrypoint) plus a success/fail terminal pair.
// The operator fills in the prompt body in the editor.
func renderWorkflowStarter(workflowID, displayName, stepName, roleName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "workflowId: %q\n", workflowID)
	fmt.Fprintf(&b, "displayName: %q\n", displayName)
	fmt.Fprintf(&b, "entrypoint: %q\n", stepName)
	fmt.Fprintf(&b, "steps:\n")
	fmt.Fprintf(&b, "  %s:\n", stepName)
	fmt.Fprintf(&b, "    type: \"agent\"\n")
	fmt.Fprintf(&b, "    role: %q\n", roleName)
	fmt.Fprintf(&b, "    on_success: \"done\"\n")
	fmt.Fprintf(&b, "    on_fail: \"failed\"\n")
	fmt.Fprintf(&b, "terminals:\n")
	fmt.Fprintf(&b, "  done:\n")
	fmt.Fprintf(&b, "    status: \"COMPLETED\"\n")
	fmt.Fprintf(&b, "    message: \"Workflow complete\"\n")
	fmt.Fprintf(&b, "  failed:\n")
	fmt.Fprintf(&b, "    status: \"FAILED\"\n")
	fmt.Fprintf(&b, "    message: \"Workflow step failed\"\n")
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", displayName)
	fmt.Fprintf(&b, "Created via the workflow starter at `/ui/workflows/new`.\n")
	fmt.Fprintf(&b, "Edit this file in the workflow editor at\n")
	fmt.Fprintf(&b, "`/ui/workflows/%s/edit` — add steps, gates, plan branches,\n", workflowID)
	fmt.Fprintf(&b, "or replace the single agent step below.\n\n")
	fmt.Fprintf(&b, "The starter step calls the `%s` role on the project's\n", roleName)
	fmt.Fprintf(&b, "configured swarm; make sure that role exists before pointing\n")
	fmt.Fprintf(&b, "a project at this workflow.\n\n")
	fmt.Fprintf(&b, "## Prompts\n\n")
	fmt.Fprintf(&b, "### %s\n\n", stepName)
	fmt.Fprintf(&b, "Replace this prose with the prompt that drives the agent for\n")
	fmt.Fprintf(&b, "this step. Reference task inputs by name (e.g. `{{ .Prompt }}`)\n")
	fmt.Fprintf(&b, "and describe the deliverable the role should produce.\n")
	return b.String()
}
