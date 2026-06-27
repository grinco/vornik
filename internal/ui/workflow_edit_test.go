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

// writeWorkflowFixture seeds a registry with a project + swarm
// + workflow where the workflow has two steps and one terminal,
// each step has a body-section prompt the editor can target.
func writeWorkflowFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p1.yaml"), []byte(`projectId: p1
displayName: P1
swarmId: s1
defaultWorkflowId: edit-wf
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
  - name: coder
    model: test-model
    systemPrompt: coder
    runtime:
      image: test
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "edit-wf.md"), []byte(`---
# operator comment that must survive form saves
workflowId: edit-wf
displayName: "Editable Workflow"
version: "1.0.0"
entrypoint: plan
maxStepVisits: 3
maxIterations: 20
steps:
  plan:
    type: agent
    role: lead
    on_success: implement
    on_fail: failed
    timeout: 15m
  implement:
    type: agent
    role: coder
    on_success: done
    on_fail: failed
    timeout: 60m
terminals:
  done:
    status: COMPLETED
  failed:
    status: FAILED
---

# Editable Workflow

A workflow used in unit tests for the editor.

## Prompts

### plan

Analyse the task and plan an implementation.

### implement

Implement the plan. Run tests before declaring done.

## Error handling

Both steps route to failed terminal on error.
`), 0o600))
	return root
}

func workflowEditServer(t *testing.T, root string) (*Server, *reloadingReloader) {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))
	return server, reloader
}

func postWorkflowForm(workflowID string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/workflows/"+workflowID+"/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// baselineWorkflowFormValues returns the minimum set of form
// fields a real browser would post for the writeWorkflowFixture
// workflow. Each test overrides only what it cares about —
// without these defaults the form save zeros out per-step
// frontmatter (role / transitions / timeout) and the validator
// rejects the resulting workflow.
func baselineWorkflowFormValues() url.Values {
	v := url.Values{}
	v.Set("displayName", "Editable Workflow")
	v.Set("version", "1.0.0")
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	v.Set("stepRole_plan", "lead")
	v.Set("stepOnSuccess_plan", "implement")
	v.Set("stepOnFail_plan", "failed")
	v.Set("stepTimeout_plan", "15m")
	v.Set("stepRole_implement", "coder")
	v.Set("stepOnSuccess_implement", "done")
	v.Set("stepOnFail_implement", "failed")
	v.Set("stepTimeout_implement", "60m")
	v.Set("stepPrompt_plan", "Analyse the task and plan an implementation.")
	v.Set("stepPrompt_implement", "Implement the plan. Run tests before declaring done.")
	return v
}

// TestWorkflowEdit_RendersAssistAffordance — symmetric to the
// swarm-side assertion: every AGENT step row carries the AI
// Assist controls and the data-kind/target/subject attributes.
// Non-agent steps (gate, approval) should NOT get the buttons
// since they have no prompt.
func TestWorkflowEdit_RendersAssistAffordance(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`data-assist-kind="workflow_step"`,
		`data-assist-target="edit-wf"`,
		`data-assist-subject="plan"`,
		`data-assist-subject="implement"`,
		"Draft",
		"Critique",
	} {
		assert.Contains(t, body, want, "workflow editor missing %q from AI Assist affordance", want)
	}
}

// TestWorkflowEdit_PopulatesFromWorkflow — GET handler renders
// every editable scalar + per-step prompt textarea + per-step
// read-only summary + terminal list.
func TestWorkflowEdit_PopulatesFromWorkflow(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	for _, want := range []string{
		`name="displayName"`,
		`name="version"`,
		`name="entrypoint"`,
		`name="maxStepVisits"`,
		`name="maxIterations"`,
		`name="maxWallClock"`,
		`name="stepPrompt_plan"`,
		`name="stepPrompt_implement"`,
		"Editable Workflow",
		"Analyse the task and plan an implementation.",
		"Implement the plan. Run tests before declaring done.",
		// Per-step editable inputs now carry the current values.
		`value="lead"`,
		`value="implement"`,
		`value="done"`,
		`value="15m"`,
		// Terminals shown read-only.
		">done<",
		">failed<",
		"COMPLETED",
		"FAILED",
	} {
		assert.Contains(t, body, want, "rendered workflow editor missing %q", want)
	}
}

// TestWorkflowEdit_InvalidID — slash-traversal rejected.
func TestWorkflowEdit_InvalidID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/etc/passwd/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "../etc/passwd")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestWorkflowEdit_UnknownID — workflow not in the registry.
func TestWorkflowEdit_UnknownID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/no-such/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "no-such")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestWorkflowEdit_PerStepFrontmatterControls — every step row
// renders form inputs for role / on_success / on_fail / timeout
// (the highest-touch per-step fields). Type stays read-only —
// changing a step's type is rare and risky enough to keep the
// operator in Advanced YAML for it.
func TestWorkflowEdit_PerStepFrontmatterControls(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/edit", nil)
	rec := httptest.NewRecorder()
	server.WorkflowEdit(rec, req, "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`name="stepRole_plan"`,
		`name="stepOnSuccess_plan"`,
		`name="stepOnFail_plan"`,
		`name="stepTimeout_plan"`,
		`name="stepRole_implement"`,
	} {
		assert.Contains(t, body, want, "workflow editor missing per-step field %q", want)
	}
}

// TestWorkflowSave_UpdatesPerStepFrontmatter — form posts new
// role / transitions / timeout for each step, the YAML patcher
// targets steps.<id>.<field>, the registry round-trips the
// values, and the operator comment in the file survives.
func TestWorkflowSave_UpdatesPerStepFrontmatter(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineWorkflowFormValues()
	// Override only the per-step frontmatter under test.
	form.Set("stepRole_plan", "coder")       // was: lead
	form.Set("stepTimeout_plan", "20m")      // was: 15m
	form.Set("stepTimeout_implement", "75m") // was: 60m

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	plan := wf.Steps["plan"]
	assert.Equal(t, "coder", plan.Role)
	assert.Equal(t, "implement", plan.OnSuccess)
	assert.Equal(t, "failed", plan.OnFail)
	assert.Equal(t, "20m", plan.Timeout)
	impl := wf.Steps["implement"]
	assert.Equal(t, "75m", impl.Timeout)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(after)
	assert.Contains(t, got, "# operator comment that must survive form saves",
		"frontmatter operator comment must be preserved through per-step edits")
	// Backup of pre-save content.
	backups, err := filepath.Glob(path + ".bak-*")
	require.NoError(t, err)
	require.Len(t, backups, 1)
	bak, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, string(before), string(bak))
}

// TestWorkflowSave_UpdatesScalarsAndPrompts — happy path:
// update displayName, version, entrypoint, both step prompts.
// Operator comment + Error handling section must survive.
func TestWorkflowSave_UpdatesScalarsAndPrompts(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineWorkflowFormValues()
	form.Set("displayName", "Editable Workflow v2")
	form.Set("version", "1.1.0")
	form.Set("entrypoint", "plan")
	form.Set("maxStepVisits", "5")
	form.Set("maxIterations", "30")
	form.Set("maxWallClock", "45m")
	form.Set("stepPrompt_plan", "Plan v2. Focus on edge cases.")
	form.Set("stepPrompt_implement", "Implement v2. Run tests then declare done.")

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	assert.Equal(t, "Editable Workflow v2", wf.DisplayName)
	assert.Equal(t, "1.1.0", wf.Version)
	assert.Equal(t, "plan", wf.Entrypoint)
	assert.Equal(t, 5, wf.MaxStepVisits)
	assert.Equal(t, 30, wf.MaxIterations)
	assert.Equal(t, "45m", wf.MaxWallClock)
	assert.Contains(t, wf.Steps["plan"].Prompt, "Plan v2. Focus on edge cases.")
	assert.Contains(t, wf.Steps["implement"].Prompt, "Implement v2. Run tests then declare done.")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(after)
	assert.Contains(t, got, "# operator comment that must survive form saves", "frontmatter operator comment must be preserved")
	assert.Contains(t, got, "## Error handling", "non-prompts body section must survive")
	assert.Contains(t, got, "Both steps route to failed terminal on error.", "docs section content must survive")

	backups, err := filepath.Glob(path + ".bak-*")
	require.NoError(t, err)
	require.Len(t, backups, 1)
	bak, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, string(before), string(bak))
}

// TestWorkflowSave_ValidationFailure — set entrypoint to a
// nonexistent step. Validator rejects, file untouched, no
// reload.
func TestWorkflowSave_ValidationFailure(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineWorkflowFormValues()
	form.Set("entrypoint", "ghost-step")

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, reloader.calls)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestWorkflowSave_InvalidNumberKeepsFileIntact — bad number
// input is rejected before YAML patching so existing limits are
// not silently removed as zero values.
func TestWorkflowSave_InvalidNumberKeepsFileIntact(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := url.Values{}
	form.Set("displayName", "Editable Workflow")
	form.Set("version", "1.0.0")
	form.Set("entrypoint", "plan")
	form.Set("maxStepVisits", "many")
	form.Set("stepPrompt_plan", "x")
	form.Set("stepPrompt_implement", "y")

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "maxStepVisits must be an integer")
	assert.Equal(t, 0, reloader.calls)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestWorkflowSave_InvalidID — slash-traversal at save time.
func TestWorkflowSave_InvalidID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, _ := workflowEditServer(t, root)
	form := url.Values{}
	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("../etc/passwd", form), "../etc/passwd")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestWorkflowSave_ReloadErrorReportsConflict — file written
// but reload failed: 409 with backup path.
func TestWorkflowSave_ReloadErrorReportsConflict(t *testing.T) {
	root := writeWorkflowFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &erroringReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	form := baselineWorkflowFormValues()

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 1, reloader.calls)
}

// TestTrimWorkflowParserPrefix — strip the WORKFLOW.md banner
// from inline error messages.
func TestTrimWorkflowParserPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"WORKFLOW.md test.md: missing prompt for step plan", "missing prompt for step plan"},
		{"plain error", "plain error"},
		{"WORKFLOW.md", "WORKFLOW.md"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, trimWorkflowParserPrefix(tc.in))
	}
}

// writeWorkflowFixtureWithCleanup seeds the same project + swarm
// as writeWorkflowFixture but with a workflow whose frontmatter
// carries a `cleanup_artifacts:` list. Used by the round-trip
// regression test for the 2026-05-18 incident where every form
// save silently stripped this key.
func writeWorkflowFixtureWithCleanup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p1.yaml"), []byte(`projectId: p1
displayName: P1
swarmId: s1
defaultWorkflowId: edit-wf
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
  - name: coder
    model: test-model
    systemPrompt: coder
    runtime:
      image: test
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "edit-wf.md"), []byte(`---
workflowId: edit-wf
displayName: "Editable Workflow"
version: "1.0.0"
entrypoint: plan
maxStepVisits: 3
maxIterations: 20
cleanup_artifacts:
  - research.md
  - deliverable.md
  - summary.txt
steps:
  plan:
    type: agent
    role: lead
    on_success: implement
    on_fail: failed
    timeout: 15m
  implement:
    type: agent
    role: coder
    on_success: done
    on_fail: failed
    timeout: 60m
terminals:
  done:
    status: COMPLETED
  failed:
    status: FAILED
---

# Editable Workflow

## Prompts

### plan

Analyse the task and plan an implementation.

### implement

Implement the plan. Run tests before declaring done.
`), 0o600))
	return root
}

// TestWorkflowEdit_CleanupArtifactsRoundTrip pins the 2026-05-18
// regression: a workflow with `cleanup_artifacts:` survived a form
// save unchanged, even when the operator didn't touch the field.
// The bug was the editor not knowing about the key at all — every
// POST stripped it. Now the GET surfaces the textarea, the POST
// re-emits the same list, and the on-disk YAML retains the entries.
func TestWorkflowEdit_CleanupArtifactsRoundTrip(t *testing.T) {
	root := writeWorkflowFixtureWithCleanup(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")

	// Pre-save invariant: the registry parsed the field off disk.
	wfBefore := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wfBefore)
	assert.Equal(t, []string{"research.md", "deliverable.md", "summary.txt"}, wfBefore.CleanupArtifacts,
		"fixture must seed CleanupArtifacts so the regression actually exercises the save path")

	// GET surfaces the textarea pre-populated with the artifact
	// list. Without the field on the form the operator can't even
	// SEE the field, which is half of the regression.
	getReq := httptest.NewRequest(http.MethodGet, "/workflows/edit-wf/edit", nil)
	getRec := httptest.NewRecorder()
	server.WorkflowEdit(getRec, getReq, "edit-wf")
	require.Equal(t, http.StatusOK, getRec.Code)
	body := getRec.Body.String()
	assert.Contains(t, body, `name="cleanupArtifacts"`, "editor must surface a form field for cleanup_artifacts")
	assert.Contains(t, body, "research.md", "textarea must pre-populate with current CleanupArtifacts entries")
	assert.Contains(t, body, "deliverable.md")
	assert.Contains(t, body, "summary.txt")

	// POST the form with the SAME cleanup_artifacts value the GET
	// surfaced. This mirrors what a browser does when the operator
	// only changes an unrelated field — the regression manifested
	// because the form had nothing for this key, so the POST body
	// was empty and the patcher dropped it.
	form := baselineWorkflowFormValues()
	form.Set("cleanupArtifacts", "research.md\ndeliverable.md\nsummary.txt")

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	// In-memory registry: cleanup_artifacts survived.
	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	assert.Equal(t, []string{"research.md", "deliverable.md", "summary.txt"}, wf.CleanupArtifacts,
		"workflow editor must round-trip cleanup_artifacts verbatim — silently dropping this list caused the 2026-05-18 incident")

	// On-disk YAML: the key + entries are still there. Pin the key
	// name explicitly so a future rename or RemoveIfEmpty bug
	// reappears as a test failure on the line below.
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(saved)
	assert.Contains(t, got, "cleanup_artifacts:", "saved YAML must retain the cleanup_artifacts key")
	assert.Contains(t, got, "research.md")
	assert.Contains(t, got, "deliverable.md")
	assert.Contains(t, got, "summary.txt")
}

// TestWorkflowEdit_CleanupArtifactsRemovedWhenEmpty exercises the
// inverse: an operator who clears every line in the textarea ends
// up with no cleanup_artifacts key on disk. RemoveIfEmpty handles
// this; the test pins the contract so future edits don't ship
// `cleanup_artifacts: []` litter.
func TestWorkflowEdit_CleanupArtifactsRemovedWhenEmpty(t *testing.T) {
	root := writeWorkflowFixtureWithCleanup(t)
	server, reloader := workflowEditServer(t, root)
	path := filepath.Join(root, "workflows", "edit-wf.md")

	form := baselineWorkflowFormValues()
	form.Set("cleanupArtifacts", "") // operator emptied the textarea

	rec := httptest.NewRecorder()
	server.WorkflowSave(rec, postWorkflowForm("edit-wf", form), "edit-wf")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	wf := server.projectReg.GetWorkflow("edit-wf")
	require.NotNil(t, wf)
	assert.Empty(t, wf.CleanupArtifacts,
		"emptying the textarea must clear CleanupArtifacts in the reloaded registry")

	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(saved)
	assert.NotContains(t, got, "cleanup_artifacts:",
		"empty textarea should drop the cleanup_artifacts key entirely, not leave a []")
}
