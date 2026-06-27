package ui

import (
	"context"
	"encoding/json"
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

// scriptedLLM is a stronger test double than captureLLM: it
// returns a different canned response per call so the wizard's
// multi-call shape (one LLM call shaping the SWARM, one shaping
// the WORKFLOW) can be tested independently. Each call appends
// to Captured for assertion.
type scriptedLLM struct {
	Responses []string
	Calls     int
	Captured  []struct {
		Model  string
		System string
		User   string
	}
}

func (s *scriptedLLM) Complete(_ context.Context, model, system, user string) (*AssistantResult, error) {
	s.Captured = append(s.Captured, struct {
		Model  string
		System string
		User   string
	}{model, system, user})
	idx := s.Calls
	s.Calls++
	if idx >= len(s.Responses) {
		idx = len(s.Responses) - 1
	}
	return &AssistantResult{Text: s.Responses[idx]}, nil
}

// writeWizardFixture seeds a registry where the project has a
// brief but no swarm yet — that's the case the wizard is
// designed for. autonomy.enabled = true so the "full generate"
// gate fires.
func writeWizardFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))
	// Existing swarm + workflow the project points to, so the
	// loader can load the project. The wizard generates NEW
	// swarm + workflow drafts; it doesn't overwrite these.
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "seed-swarm.md"), []byte(`---
swarmId: seed-swarm
roles:
  - name: lead
    model: seed-model
    systemPrompt: seed lead body
    runtime:
      image: test
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "seed-wf.md"), []byte(`---
workflowId: seed-wf
entrypoint: a
steps:
  a:
    type: agent
    role: lead
    on_success: done
    prompt: seed step body
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "wizdemo.yaml"), []byte(`projectId: wizdemo
displayName: Wizard Demo
swarmId: seed-swarm
defaultWorkflowId: seed-wf
defaultPriority: 50
maxConcurrentTasks: 1
autonomy:
  enabled: true
  mode: llm
  goal: existing yaml goal
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "wizdemo.md"), []byte(`---
projectId: wizdemo
---

## Goal

Produce sourced research reports for analysts.

## Audience

Internal research analysts.

## Success criteria

Each report cites primary sources.

## Out of scope

Code review.

## Risk & cadence

Low-risk daily cadence.
`), 0o644))
	return root
}

// validSwarmJSON is a properly-shaped wizard response chunk
// for the swarm-generation call. The wizard expects the LLM to
// emit a JSON envelope around the generated SWARM.md content so
// extraction is deterministic.
func validSwarmJSON(id string) string {
	return `{"swarm_md": "---\nswarmId: ` + id + `\nleadRole: lead\nroles:\n  - name: lead\n    description: Plans research\n    systemPrompt: \"You are the lead.\"\n    runtime:\n      image: vornik-agent:latest\n---\n"}`
}

func validWorkflowJSON(id string) string {
	return `{"workflow_md": "---\nworkflowId: ` + id + `\nentrypoint: plan\nsteps:\n  plan:\n    type: agent\n    role: lead\n    on_success: done\n    prompt: \"Plan the research.\"\nterminals:\n  done:\n    status: COMPLETED\n---\n"}`
}

func wizardServer(t *testing.T, root string, llm AssistantLLM) (*Server, *reloadingReloader) {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithAssistantLLM(llm),
		WithAssistantDefaultModel("default-model"),
	)
	return server, reloader
}

// TestWizardGenerate_HappyPath — given a project with a brief
// and autonomy enabled, the wizard makes two LLM calls (one for
// the swarm, one for the workflow), writes the two new files,
// updates the project.yaml's swarmId / defaultWorkflowId, and
// reloads the registry so the new swarm/workflow are live.
func TestWizardGenerate_HappyPath(t *testing.T) {
	root := writeWizardFixture(t)
	swarmID := "wizdemo-swarm"
	wfID := "wizdemo-wf"
	llm := &scriptedLLM{Responses: []string{
		validSwarmJSON(swarmID),
		validWorkflowJSON(wfID),
	}}
	server, reloader := wizardServer(t, root, llm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WizardGenerate(rec, req, "wizdemo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	// Two LLM calls; second one's user prompt includes the
	// generated swarm so the workflow's roles match real names.
	require.Equal(t, 2, llm.Calls)
	assert.Contains(t, llm.Captured[1].User, "lead", "workflow call must see the generated role names")

	// Files written under the canonical paths.
	swarmPath := filepath.Join(root, "swarms", swarmID+".md")
	wfPath := filepath.Join(root, "workflows", wfID+".md")
	_, err := os.Stat(swarmPath)
	require.NoError(t, err)
	_, err = os.Stat(wfPath)
	require.NoError(t, err)

	// Project's swarmId + defaultWorkflowId now point to the
	// generated artifacts.
	proj := server.projectReg.GetProject("wizdemo")
	require.NotNil(t, proj)
	assert.Equal(t, swarmID, proj.SwarmID)
	assert.Equal(t, wfID, proj.DefaultWorkflowID)

	// Response payload is JSON with both rendered paths for the
	// UI to link to.
	var resp wizardResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, swarmID, resp.SwarmID)
	assert.Equal(t, wfID, resp.WorkflowID)
}

// TestWizardGenerate_RefusesWithoutBrief — the wizard's
// grounding is the project brief; without one, refuse with 400
// (operator should create the brief first).
func TestWizardGenerate_RefusesWithoutBrief(t *testing.T) {
	root := writeWizardFixture(t)
	require.NoError(t, os.Remove(filepath.Join(root, "projects", "wizdemo.md")))

	llm := &scriptedLLM{Responses: []string{"never used"}}
	server, _ := wizardServer(t, root, llm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WizardGenerate(rec, req, "wizdemo")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, llm.Calls)
	assert.Contains(t, rec.Body.String(), "brief")
}

// TestWizardGenerate_GatedByAutonomy — full generation requires
// autonomy.enabled AND requireApproval=false (per the LLD's
// "Both, gated by autonomy level" answer). With autonomy off,
// the handler returns 403 so operators see the gate.
func TestWizardGenerate_GatedByAutonomy(t *testing.T) {
	root := writeWizardFixture(t)
	// Flip autonomy off.
	path := filepath.Join(root, "projects", "wizdemo.yaml")
	body, _ := os.ReadFile(path)
	body = []byte(strings.ReplaceAll(string(body), "enabled: true", "enabled: false"))
	require.NoError(t, os.WriteFile(path, body, 0o644))

	llm := &scriptedLLM{Responses: []string{"never used"}}
	server, _ := wizardServer(t, root, llm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WizardGenerate(rec, req, "wizdemo")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, llm.Calls)
	assert.Contains(t, rec.Body.String(), "autonomy")
}

// TestWizardGenerate_InvalidSwarmJSON — when the LLM produces
// JSON that doesn't parse as a valid SWARM.md, the wizard
// returns 502 and DOES NOT write any files.
func TestWizardGenerate_InvalidSwarmJSON(t *testing.T) {
	root := writeWizardFixture(t)
	llm := &scriptedLLM{Responses: []string{
		`{"swarm_md": "this is not valid swarm yaml"}`,
		`unused`,
	}}
	server, reloader := wizardServer(t, root, llm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WizardGenerate(rec, req, "wizdemo")
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, 0, reloader.calls)
	// Nothing under swarms/ or workflows/ matches the generated
	// id (the wizard hasn't run that far).
	entries, _ := os.ReadDir(filepath.Join(root, "swarms"))
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "wizdemo-swarm", "no wizard-generated swarm should be written on parse failure")
	}
}

// TestWizardGenerate_GETRejected — only POST is allowed; GET
// must not trigger an LLM call.
func TestWizardGenerate_GETRejected(t *testing.T) {
	root := writeWizardFixture(t)
	llm := &scriptedLLM{Responses: []string{"x"}}
	server, _ := wizardServer(t, root, llm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/wizdemo/wizard", nil)
	server.WizardGenerate(rec, req, "wizdemo")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, 0, llm.Calls)
}

// TestWizardGenerate_UnknownProject — 404.
func TestWizardGenerate_UnknownProject(t *testing.T) {
	root := writeWizardFixture(t)
	llm := &scriptedLLM{Responses: []string{"x"}}
	server, _ := wizardServer(t, root, llm)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/no-such/wizard", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.WizardGenerate(rec, req, "no-such")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestProjectDetail_RendersGenerateButton — the project detail
// page surfaces a "Generate swarm + workflow from brief" CTA
// when the project has a brief AND autonomy is enabled.
// Without the brief or autonomy gate the CTA is hidden.
func TestProjectDetail_RendersGenerateButton(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:          "demo",
			DisplayName: "Demo",
			SwarmID:     "swarm-1",
			Brief:       &registry.ProjectBrief{ProjectID: "demo", Goal: "x", Audience: "y", SuccessCriteria: "z"},
			Autonomy:    registry.ProjectAutonomy{Enabled: true},
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf strings.Builder
	require.NoError(t, s.templates.ExecuteTemplate(&buf, "project_detail.html", data))
	out := buf.String()
	assert.Contains(t, out, "Configuration wizard")
	assert.Contains(t, out, "generate swarm + workflow from brief")
	assert.Contains(t, out, `action="/ui/projects/demo/wizard"`)
}

// TestProjectDetail_HidesGenerateButtonWhenNoBrief — without a
// brief the Generate FORM stays hidden so clicking can't 400.
// The wizard heading + prereq checklist now ALWAYS render to
// guide the operator toward enabling it.
func TestProjectDetail_HidesGenerateButtonWhenNoBrief(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:       "demo",
			SwarmID:  "swarm-1",
			Autonomy: registry.ProjectAutonomy{Enabled: true},
		},
		Swarm: &registry.Swarm{ID: "swarm-1", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	var buf strings.Builder
	require.NoError(t, s.templates.ExecuteTemplate(&buf, "project_detail.html", data))
	out := buf.String()
	// The form action must NOT be there — clicking would 400.
	assert.NotContains(t, out, `action="/ui/projects/demo/wizard"`)
	// But the wizard heading + prereq checklist DO render.
	assert.Contains(t, out, "Configuration wizard")
}

// TestWizardGenerate_FormValueOverridesIDs — operators may
// supply custom swarmId / workflowId via form fields to avoid
// the auto-derived "wizdemo-swarm" / "wizdemo-wf" defaults.
func TestWizardGenerate_FormValueOverridesIDs(t *testing.T) {
	root := writeWizardFixture(t)
	swarmID := "my-research-swarm"
	wfID := "my-research-wf"
	llm := &scriptedLLM{Responses: []string{
		validSwarmJSON(swarmID),
		validWorkflowJSON(wfID),
	}}
	server, _ := wizardServer(t, root, llm)

	form := url.Values{}
	form.Set("swarmId", swarmID)
	form.Set("workflowId", wfID)
	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	server.WizardGenerate(rec, req, "wizdemo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := server.projectReg.GetProject("wizdemo")
	require.NotNil(t, proj)
	assert.Equal(t, swarmID, proj.SwarmID)
	assert.Equal(t, wfID, proj.DefaultWorkflowID)
}

func TestWizardGenerate_RejectsUnsafeArtifactIDs(t *testing.T) {
	root := writeWizardFixture(t)
	llm := &scriptedLLM{Responses: []string{
		validSwarmJSON("../projects/pwned"),
		validWorkflowJSON("safe-wf"),
	}}
	server, _ := wizardServer(t, root, llm)

	form := url.Values{}
	form.Set("swarmId", "../projects/pwned")
	form.Set("workflowId", "safe-wf")
	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	server.WizardGenerate(rec, req, "wizdemo")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, llm.Calls)
	_, err := os.Stat(filepath.Join(root, "projects", "pwned.md"))
	assert.True(t, os.IsNotExist(err))
}

func TestWizardGenerate_RefusesToOverwriteExistingArtifacts(t *testing.T) {
	root := writeWizardFixture(t)
	existing := `---
swarmId: wizdemo-swarm
leadRole: lead
roles:
  - name: lead
    systemPrompt: existing
    runtime:
      image: test
---
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "wizdemo-swarm.md"), []byte(existing), 0o644))
	llm := &scriptedLLM{Responses: []string{
		validSwarmJSON("wizdemo-swarm"),
		validWorkflowJSON("wizdemo-wf"),
	}}
	server, _ := wizardServer(t, root, llm)

	req := httptest.NewRequest(http.MethodPost, "/projects/wizdemo/wizard", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	server.WizardGenerate(rec, req, "wizdemo")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 0, llm.Calls)
	body, err := os.ReadFile(filepath.Join(root, "swarms", "wizdemo-swarm.md"))
	require.NoError(t, err)
	assert.Equal(t, existing, string(body))
}
