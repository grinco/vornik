package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// TestWorkflowsNew_GETRendersForm anchors the starter-form gate:
// the page renders with sensible defaults so an operator who
// only fills the ID gets a working WORKFLOW.md.
func TestWorkflowsNew_GETRendersForm(t *testing.T) {
	srv, _ := newCreateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/workflows/new", nil)
	rr := httptest.NewRecorder()
	srv.WorkflowsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, `name="workflowId"`)
	assert.Contains(t, body, `name="stepName"`)
	assert.Contains(t, body, `name="roleName"`)
	assert.Contains(t, body, "run", "default step name shows in the input value")
}

// TestWorkflowsCreate_HappyPath — POSTing a valid form writes a
// WORKFLOW.md that parses, validates, and is structured the way
// the registry expects (entrypoint → agent step → terminal).
func TestWorkflowsCreate_HappyPath(t *testing.T) {
	srv, configsDir := newCreateRig(t)

	form := url.Values{
		"workflowId":  {"happy-wf"},
		"displayName": {"Happy WF"},
		"stepName":    {"go"},
		"roleName":    {"helper"},
	}
	req := httptest.NewRequest(http.MethodPost, "/workflows/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.WorkflowsCreate(rr, req)

	require.Equal(t, http.StatusSeeOther, rr.Code, "body=%s", rr.Body.String())
	assert.Equal(t, "/ui/workflows/happy-wf/edit", rr.Header().Get("Location"))

	data, err := os.ReadFile(filepath.Join(configsDir, "workflows", "happy-wf.md"))
	require.NoError(t, err)
	parsed, err := registry.ParseWorkflowMarkdown(data, "happy-wf.md")
	require.NoError(t, err, "rendered WORKFLOW.md must parse")
	require.NoError(t, parsed.Validate("happy-wf.md"))
	assert.Equal(t, "happy-wf", parsed.ID)
	assert.Equal(t, "go", parsed.Entrypoint)
	require.Contains(t, parsed.Steps, "go")
	assert.Equal(t, "agent", parsed.Steps["go"].Type)
	assert.Equal(t, "helper", parsed.Steps["go"].Role)
	// Both terminals must exist so the parser-validate path doesn't
	// reject "on_fail: failed" with an unknown-transition error.
	require.Contains(t, parsed.Terminals, "done")
	require.Contains(t, parsed.Terminals, "failed")
	assert.Equal(t, "COMPLETED", string(parsed.Terminals["done"].Status))
	assert.Equal(t, "FAILED", string(parsed.Terminals["failed"].Status))
	assert.NotEmpty(t, parsed.Steps["go"].Prompt,
		"starter must fill the step's prompt from the `## Prompts` subsection")
}

// TestWorkflowsCreate_RefusesOverwrite — second POST with the
// same ID fails loud, doesn't clobber the existing file.
func TestWorkflowsCreate_RefusesOverwrite(t *testing.T) {
	srv, configsDir := newCreateRig(t)

	require.NoError(t, os.MkdirAll(filepath.Join(configsDir, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configsDir, "workflows", "dup.md"),
		[]byte("---\nworkflowId: dup\nentrypoint: x\n---\n"), 0o644))

	form := url.Values{"workflowId": {"dup"}}
	req := httptest.NewRequest(http.MethodPost, "/workflows/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.WorkflowsCreate(rr, req)

	require.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "already exists")
}

// TestWorkflowsCreate_RejectsInvalidID anchors the same ID-pattern
// guard the swarms surface uses.
func TestWorkflowsCreate_RejectsInvalidID(t *testing.T) {
	srv, _ := newCreateRig(t)
	form := url.Values{"workflowId": {"BadID"}}
	req := httptest.NewRequest(http.MethodPost, "/workflows/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.WorkflowsCreate(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "Workflow ID must be")
}
