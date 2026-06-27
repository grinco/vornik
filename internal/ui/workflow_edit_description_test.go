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

// writeWorkflowFixtureWithDescription mirrors writeWorkflowFixture
// but seeds the workflow with a frontmatter `description:` so the
// GET-renders-it test has something concrete to assert.
func writeWorkflowFixtureWithDescription(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p1.yaml"), []byte(`projectId: p1
displayName: P1
swarmId: s1
defaultWorkflowId: desc-wf
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "s1.md"), []byte(`---
swarmId: s1
roles:
  - name: lead
    model: test-model
    systemPrompt: lead
    runtime:
      image: test
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "desc-wf.md"), []byte(`---
workflowId: desc-wf
displayName: "Has Description"
description: "Pre-existing summary the editor must surface."
version: "1.0.0"
entrypoint: plan
steps:
  plan:
    type: agent
    role: lead
    on_success: done
    on_fail: failed
    timeout: 15m
terminals:
  done:
    status: COMPLETED
  failed:
    status: FAILED
---

## Prompts

### plan

Do the thing.
`), 0o600))
	return root
}

func descBaselineForm() url.Values {
	v := url.Values{}
	v.Set("displayName", "Has Description")
	v.Set("description", "Pre-existing summary the editor must surface.")
	v.Set("version", "1.0.0")
	v.Set("entrypoint", "plan")
	v.Set("stepRole_plan", "lead")
	v.Set("stepOnSuccess_plan", "done")
	v.Set("stepOnFail_plan", "failed")
	v.Set("stepTimeout_plan", "15m")
	v.Set("stepPrompt_plan", "Do the thing.")
	return v
}

// TestWorkflowEdit_RendersDescriptionTextarea — GET handler
// surfaces the `description:` frontmatter value inside a textarea
// named "description". Fixes Finding A: the editor was missing
// the field entirely.
func TestWorkflowEdit_RendersDescriptionTextarea(t *testing.T) {
	root := writeWorkflowFixtureWithDescription(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(&reloadingReloader{reg: reg, root: root}))

	req := httptest.NewRequest(http.MethodGet, "/workflows/desc-wf/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "desc-wf")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The textarea must be named "description" and contain the
	// pre-existing summary so a round-trip GET → POST without
	// edits doesn't lose the value.
	assert.Contains(t, body, `name="description"`, "description textarea missing from editor")
	assert.Contains(t, body, "Pre-existing summary the editor must surface.", "rendered description value missing")
}

// TestWorkflowEdit_RendersEmptyDescriptionTextarea — when the
// workflow has no description on disk the textarea still renders
// (operator must be able to fill it in).
func TestWorkflowEdit_RendersEmptyDescriptionTextarea(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `name="description"`,
		"description textarea must render even when the workflow has no description on disk")
}

// TestWorkflowSave_RoundTripsDescription — POST persists a new
// description into the YAML frontmatter; a subsequent registry
// reload picks it up; the on-disk file carries the value.
func TestWorkflowSave_RoundTripsDescription(t *testing.T) {
	root := writeWorkflowFixtureWithDescription(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	form := descBaselineForm()
	form.Set("description", "Brand new description set via the form editor.")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workflows/desc-wf/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WorkflowSave(rec, req, "desc-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	wf := server.projectReg.GetWorkflow("desc-wf")
	require.NotNil(t, wf)
	assert.Equal(t, "Brand new description set via the form editor.", wf.Description)

	// On-disk file must carry the new description so a daemon
	// restart picks it up identically.
	saved, err := os.ReadFile(filepath.Join(root, "workflows", "desc-wf.md"))
	require.NoError(t, err)
	assert.Contains(t, string(saved), "Brand new description set via the form editor.")
}

// TestWorkflowSave_BackfillsDescriptionFromEmpty — saving with a
// non-empty description into a workflow that didn't have one adds
// the field to the YAML. Models the doctor-fix workflow: operator
// hits the form, fills in description, save, doctor turns green.
func TestWorkflowSave_BackfillsDescriptionFromEmpty(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	form := baselineWorkflowFormValues()
	form.Set("description", "Backfilled description from the editor.")

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, 1, reloader.calls)

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	assert.Equal(t, "Backfilled description from the editor.", wf.Description)
}

// TestWorkflowSave_DescriptionTrimmed — leading/trailing whitespace
// in the form value is stripped so the YAML stays clean.
func TestWorkflowSave_DescriptionTrimmed(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	form := baselineWorkflowFormValues()
	form.Set("description", "  description with surrounding spaces.  \n")

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	assert.Equal(t, "description with surrounding spaces.", wf.Description)
}

// TestWorkflowSave_DescriptionOverCapRejected — input past the
// validator cap returns 400 with an inline error and leaves the
// file untouched.
func TestWorkflowSave_DescriptionOverCapRejected(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineWorkflowFormValues()
	form.Set("description", strings.Repeat("x", registry.WorkflowDescriptionMaxLen+1))

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "description must be")
	assert.Equal(t, 0, reloader.calls)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "rejected POST must leave the file untouched")
}

// TestWorkflowSave_ClearDescription_RemovesKey — setting the
// textarea to empty and saving deletes the `description:` line
// from the YAML rather than leaving a `description: ""` litter
// behind.
func TestWorkflowSave_ClearDescription_RemovesKey(t *testing.T) {
	root := writeWorkflowFixtureWithDescription(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(&reloadingReloader{reg: reg, root: root}))

	form := descBaselineForm()
	form.Set("description", "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workflows/desc-wf/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WorkflowSave(rec, req, "desc-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	saved, err := os.ReadFile(filepath.Join(root, "workflows", "desc-wf.md"))
	require.NoError(t, err)
	got := string(saved)
	if strings.Contains(got, `description:`) {
		t.Errorf("cleared description should remove the key entirely, got:\n%s", got)
	}
}
