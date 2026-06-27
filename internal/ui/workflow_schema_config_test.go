package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reuses writeWorkflowFixture + workflowEditServer (workflow_edit_test.go):
// workflow "edit-wf" with steps plan + implement (body prompts), terminals
// done + failed, an operator comment, and entrypoint plan.

func setStep(v url.Values, i int, fields map[string]string) {
	for k, val := range fields {
		v.Set(fmt.Sprintf("steps[%d].%s", i, k), val)
	}
}

func postWorkflowSchema(id string, v url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/workflows/"+id+"/schema", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return withAdminUI(req)
}

// planStep / implementStep mirror the fixture so a preserved step submits
// its full field set (the form posts the whole collection).
func planStep() map[string]string {
	return map[string]string{
		"stepId": "plan", "type": "agent", "role": "lead",
		"on_success": "implement", "on_fail": "failed", "timeout": "15m",
		"prompt": "Analyse the task and plan an implementation.",
	}
}

func implementStep() map[string]string {
	return map[string]string{
		"stepId": "implement", "type": "agent", "role": "coder",
		"on_success": "done", "on_fail": "failed", "timeout": "60m",
		"prompt": "Implement the plan.",
	}
}

func TestWorkflowSchemaConfigEdit_RendersStepCards(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/schema", nil)
	server.WorkflowSchemaConfigEdit(rec, req, "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	for _, want := range []string{
		`name="entrypoint"`,
		`name="steps[0].stepId"`, // synthetic map key
		`name="steps[0].type"`,
		`name="steps[0].prompt"`,
		`name="steps[1].stepId"`,
	} {
		assert.Contains(t, body, want, "missing %q", want)
	}
	assert.Contains(t, body, "plan")
	assert.Contains(t, body, "Analyse the task and plan an implementation.")
}

// TestWorkflowSchemaConfigSave_RoundTrip — edit a step's frontmatter +
// body prompt and ADD a step in one save; comment + body prompts survive;
// the prompt round-trips via the body.
func TestWorkflowSchemaConfigSave_RoundTrip(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Workflow")
	v.Set("version", "1.0.0")
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	plan := planStep()
	plan["prompt"] = "Plan it better."
	setStep(v, 0, plan)
	// Rewire implement → review → done so the new step is reachable
	// (workflow Validate rejects unreachable steps).
	impl := implementStep()
	impl["on_success"] = "review"
	setStep(v, 1, impl)
	setStep(v, 2, map[string]string{
		"stepId": "review", "type": "gate", "on_success": "done", "on_fail": "failed",
	})

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	require.Len(t, wf.Steps, 3)
	assert.Equal(t, "Plan it better.", wf.Steps["plan"].Prompt)
	assert.Equal(t, "gate", wf.Steps["review"].Type)

	saved, err := os.ReadFile(filepath.Join(root, "workflows", "edit-wf.md"))
	require.NoError(t, err)
	got := string(saved)
	assert.Contains(t, got, "# operator comment that must survive form saves")
	assert.Contains(t, got, "Plan it better.")
}

// TestWorkflowSchemaConfigSave_RenameStep — rename a step via its id
// (delete old key + add new); entrypoint follows so validation passes.
func TestWorkflowSchemaConfigSave_RenameStep(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Workflow")
	v.Set("entrypoint", "design") // follow the rename plan → design
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	design := planStep()
	design["stepId"] = "design"
	setStep(v, 0, design)
	setStep(v, 1, implementStep())

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	_, hadPlan := wf.Steps["plan"]
	_, hasDesign := wf.Steps["design"]
	assert.False(t, hadPlan, "old step id removed")
	assert.True(t, hasDesign, "new step id present")
	assert.Equal(t, "design", wf.Entrypoint)
}

// TestWorkflowSchemaConfigSave_RequiredStepKeyRejected — a card with no
// step id is rejected with a per-field error, nothing written.
func TestWorkflowSchemaConfigSave_RequiredStepKeyRejected(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	v := url.Values{}
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	setStep(v, 0, planStep())
	setStep(v, 1, implementStep())
	setStep(v, 2, map[string]string{"type": "agent"}) // no stepId

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "required")
	assert.Equal(t, 0, reloader.calls)
}

// TestWorkflowSchemaConfigSave_WritesAudit — successful save records one
// admin-audit row.
func TestWorkflowSchemaConfigSave_WritesAudit(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)
	audit := &stubAdminAuditRepo{}
	server.adminAuditRepo = audit

	v := url.Values{}
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	setStep(v, 0, planStep())
	setStep(v, 1, implementStep())

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Len(t, audit.rows, 1)
	assert.Equal(t, "workflow.save", audit.rows[0].Action)
	assert.Equal(t, "edit-wf", audit.rows[0].Target)
}

func TestWorkflowSchemaBlankStepCard(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/schema/step?index=5", nil)
	server.WorkflowSchemaStepCard(rec, req, "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `name="steps[5].stepId"`)
	assert.Contains(t, body, `name="steps[5].type"`)
}

func TestWorkflowSchemaConfigEdit_MissingWorkflow(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workflows/ghost/schema", nil)
	server.WorkflowSchemaConfigEdit(rec, req, "ghost")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Workflow not found")
}

func TestWorkflowSchemaConfigSave_RequiresAdmin(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	v := url.Values{}
	setStep(v, 0, planStep())
	req := httptest.NewRequest(http.MethodPost, "/workflows/edit-wf/schema", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, req, "edit-wf") // no withAdminUI
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, reloader.calls)
}

func TestWorkflowSchemaConfigSave_ReloadConflict(t *testing.T) {
	root := writeWorkflowFixture(t)
	reg := newSwarmRegistry(t, root)
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(&erroringReloader{}))

	v := url.Values{}
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	setStep(v, 0, planStep())
	setStep(v, 1, implementStep())

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "reload failed")
}

func TestWorkflowSchemaConfigSave_NoReloaderUsesRegistryLoad(t *testing.T) {
	root := writeWorkflowFixture(t)
	reg := newSwarmRegistry(t, root)
	server := NewServer(WithProjectRegistry(reg)) // no reloader

	v := url.Values{}
	v.Set("displayName", "Renamed WF")
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	setStep(v, 0, planStep())
	setStep(v, 1, implementStep())

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "Renamed WF", reg.GetWorkflow("edit-wf").DisplayName)
}

func TestWorkflowSchemaRoute(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/schema", nil)
	server.workflowRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `name="steps[0].stepId"`)
}
