package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// captureLLM is the test double for AssistantLLM. Each test
// configures Response (and optionally Err / token counts); the
// test then inspects Captured to assert on what the handler
// sent.
type captureLLM struct {
	Response            string
	Model               string
	PromptTokens        int
	CompletionTokens    int
	CacheCreationTokens int
	CacheReadTokens     int
	Err                 error
	Calls               int
	Captured            struct {
		Model  string
		System string
		User   string
	}
}

func (c *captureLLM) Complete(_ context.Context, model, system, user string) (*AssistantResult, error) {
	c.Calls++
	c.Captured.Model = model
	c.Captured.System = system
	c.Captured.User = user
	if c.Err != nil {
		return nil, c.Err
	}
	return &AssistantResult{
		Text:                c.Response,
		Model:               c.Model,
		PromptTokens:        c.PromptTokens,
		CompletionTokens:    c.CompletionTokens,
		CacheCreationTokens: c.CacheCreationTokens,
		CacheReadTokens:     c.CacheReadTokens,
	}, nil
}

// TestAssistantDraft_RejectsNonPOST — the assistant call can
// spend real LLM tokens, so GET/query requests must never reach
// the backend.
func TestAssistantDraft_RejectsNonPOST(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "should not be used"}
	server := newAssistantServer(t, root, llm)

	req := httptest.NewRequest(http.MethodGet, "/assistant/draft?kind=swarm_role&projectId=demo&targetId=demo-swarm&subjectId=lead&action=draft", nil)
	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
	assert.Equal(t, 0, llm.Calls)
}

// writeAssistantFixture seeds a registry with everything the
// assistant handler needs to resolve project → swarm → role
// context, plus a workflow whose steps the assistant can
// reference. PROJECT.md brief is included so the grounding
// path is exercised.
func writeAssistantFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "demo.yaml"), []byte(`projectId: demo
displayName: Demo
swarmId: demo-swarm
defaultWorkflowId: demo-wf
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "demo.md"), []byte(`---
projectId: demo
---

## Goal

Make researchers productive.

## Audience

Internal research analysts.

## Success criteria

Sourced answers; no hallucinations.
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "demo-swarm.md"), []byte(`---
swarmId: demo-swarm
leadRole: lead
roles:
  - name: lead
    description: Plans research
    model: lead-model-x
    systemPrompt: "lead body"
    runtime:
      image: test
  - name: writer
    description: Writes the report
    model: writer-model-y
    systemPrompt: "writer body"
    runtime:
      image: test
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "demo-wf.md"), []byte(`---
workflowId: demo-wf
entrypoint: research
steps:
  research:
    type: agent
    role: lead
    on_success: write
    prompt: "research body"
  write:
    type: agent
    role: writer
    on_success: done
    prompt: "write body"
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	return root
}

func newAssistantServer(t *testing.T, root string, llm AssistantLLM) *Server {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	opts := []ServerOption{WithProjectRegistry(reg)}
	if llm != nil {
		opts = append(opts, WithAssistantLLM(llm))
	}
	return NewServer(opts...)
}

func postAssistant(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/assistant/draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// TestAssistantDraft_SwarmRoleHappyPath — happy path for the
// swarm-role attach point. Handler builds context from the
// project's brief + sibling roles + the current value, sends to
// the LLM, returns JSON with the suggestion.
func TestAssistantDraft_SwarmRoleHappyPath(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Here is a fresh lead-role prompt draft."}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "current lead prompt text")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp assistantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Here is a fresh lead-role prompt draft.", resp.Suggestion)
	assert.Empty(t, resp.Error)

	// Model resolution: project.assistant.model unset → falls
	// back to the swarm leadRole's model ("lead-model-x").
	assert.Equal(t, "lead-model-x", llm.Captured.Model)

	// Grounding context: project brief sections + sibling roles
	// + current value all appear in the user prompt.
	user := llm.Captured.User
	assert.Contains(t, user, "Make researchers productive.", "brief goal must ground the prompt")
	assert.Contains(t, user, "Internal research analysts.", "brief audience must ground the prompt")
	assert.Contains(t, user, "writer", "sibling role names must be in context")
	assert.Contains(t, user, "Writes the report", "sibling role descriptions must be in context")
	assert.Contains(t, user, "current lead prompt text", "current value must be in the user prompt")
	// The system prompt frames the assistant as a prompt-writing helper.
	system := llm.Captured.System
	assert.NotEmpty(t, system)
}

// TestAssistantDraft_WorkflowStepHappyPath — the workflow-step
// attach point. Model resolution defaults to the daemon-default
// when the workflow doesn't override.
func TestAssistantDraft_WorkflowStepHappyPath(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Workflow step draft."}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "daemon-default-model"

	form := url.Values{}
	form.Set("kind", "workflow_step")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-wf")
	form.Set("subjectId", "research")
	form.Set("action", "draft")
	form.Set("currentValue", "current research prompt")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp assistantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Workflow step draft.", resp.Suggestion)

	user := llm.Captured.User
	assert.Contains(t, user, "current research prompt", "current value must be in the prompt")
	// Sibling step ids in the workflow graph appear so the
	// assistant can spot graph-shaped contradictions.
	assert.Contains(t, user, "write", "sibling step ids must appear in context")
	// Workflows now resolve model through the project's swarm
	// leadRole for parity with swarm_role (Phase 2 v2.2).
	assert.Equal(t, "lead-model-x", llm.Captured.Model, "workflow path resolves through swarm leadRole model")
}

// TestAssistantDraft_CritiqueActionPrompt — Critique builds a
// different system+user prompt than Draft. We only assert that
// the action arrives at the LLM (no overwrite); operators see
// the critique as a separate textbox they read, not Apply.
func TestAssistantDraft_CritiqueActionPrompt(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Critique:\n- vague\n- no constraints"}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "any"

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "critique")
	form.Set("currentValue", "Plan the work.")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, llm.Captured.User, "Plan the work.")
	// The system prompt must shift mode — pin one signal of
	// critique mode rather than re-asserting full prompt text.
	assert.Contains(t, strings.ToLower(llm.Captured.System), "critique")
}

// TestAssistantDraft_NoLLMConfigured — when no AssistantLLM is
// wired, the handler returns 503 with a helpful message. Avoids
// silent failures where operators click and see nothing.
func TestAssistantDraft_NoLLMConfigured(t *testing.T) {
	root := writeAssistantFixture(t)
	server := newAssistantServer(t, root, nil) // no WithAssistantLLM

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "not configured")
}

// TestAssistantDraft_UnknownProject — projectId that doesn't
// resolve returns 404 rather than calling the LLM.
func TestAssistantDraft_UnknownProject(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "no-such")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// LLM must not be invoked for unknown projects.
	assert.Empty(t, llm.Captured.Model)
}

// TestAssistantDraft_UnknownSwarmRole — subjectId that doesn't
// resolve to a role gets a 400 with an actionable message.
func TestAssistantDraft_UnknownSwarmRole(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "ghost")
	form.Set("action", "draft")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, llm.Captured.Model)
}

// TestAssistantDraft_InvalidAction — only "draft" / "critique"
// are accepted in v1.
func TestAssistantDraft_InvalidAction(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "unknown-action")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestAssistantDraft_LLMError — LLM call failure surfaces as a
// JSON error response (so the JS front-end can show it inline)
// rather than a generic 500 page.
func TestAssistantDraft_LLMError(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Err: errors.New("gateway timeout")}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	var resp assistantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Error, "gateway timeout")
}

// TestAssistantDraft_TightenAction — Tighten + Expand are the
// two refactor modes added in Phase 2 v2. System prompt frames
// the LLM as a rewrite-in-place coach; the response is meant
// to land via Apply without operator review of every clause.
func TestAssistantDraft_TightenAction(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Tighter version."}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "any"

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "tighten")
	form.Set("currentValue", "A long-winded prompt with redundant constraints and verbose phrasing.")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, strings.ToLower(llm.Captured.System), "tighten")
	assert.Contains(t, llm.Captured.User, "A long-winded prompt")
}

func TestAssistantDraft_ExpandAction(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Expanded version."}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "any"

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "expand")
	form.Set("currentValue", "Plan things.")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, strings.ToLower(llm.Captured.System), "expand")
}

// TestAssistantDraft_BriefSection — new kind targeting one of
// the project brief's named sections. Sibling sections (the
// other four brief sections + the description preamble) ground
// the LLM so a draft for "Goal" can reference the project's
// "Audience" verbatim.
func TestAssistantDraft_BriefSection(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Drafted Goal section."}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "default-model"

	form := url.Values{}
	form.Set("kind", "brief_section")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo")
	form.Set("subjectId", "Goal")
	form.Set("action", "draft")
	form.Set("currentValue", "rough initial goal")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	user := llm.Captured.User
	assert.Contains(t, user, "rough initial goal", "current value must be in the prompt")
	assert.Contains(t, user, "Internal research analysts.", "sibling brief section (Audience) must ground the draft")
	// Phase 2 v2.2: brief_section also resolves through the
	// project's swarm leadRole model when available, matching
	// swarm_role / workflow_step behaviour.
	assert.Equal(t, "lead-model-x", llm.Captured.Model)
}

// TestAssistantDraft_BriefSection_InvalidSection — unknown
// section names get a 400 rather than a generic LLM call.
func TestAssistantDraft_BriefSection_InvalidSection(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "brief_section")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo")
	form.Set("subjectId", "ghost-section")
	form.Set("action", "draft")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, llm.Calls)
}

// TestAssistantDraft_ProjectField_AutonomyGoal — kind for the
// project config form's prose fields (currently just
// autonomy.goal). Grounds on the brief + the role-model lookup
// path, same as swarm_role.
func TestAssistantDraft_ProjectField_AutonomyGoal(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "Goal draft for autonomy loop."}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "project_field")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo")
	form.Set("subjectId", "autonomy.goal")
	form.Set("action", "draft")
	form.Set("currentValue", "current autonomy goal")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, llm.Captured.User, "current autonomy goal")
	assert.Contains(t, llm.Captured.User, "Internal research analysts.", "brief must ground autonomy.goal prompts")
	// project_field for an autonomy.goal on a project with a
	// swarm should still use the swarm leadRole's model (same
	// rule as swarm_role).
	assert.Equal(t, "lead-model-x", llm.Captured.Model)
}

// TestAssistantDraft_ProjectField_InvalidSubject — only known
// project_field subjects are accepted.
func TestAssistantDraft_ProjectField_InvalidSubject(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "project_field")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo")
	form.Set("subjectId", "not-a-field")
	form.Set("action", "draft")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, llm.Calls)
}

// TestProjectBriefEdit_RendersAssistAffordance — the brief
// editor's Goal / Audience / SuccessCriteria textareas each
// carry the AI Assist controls so operators can iterate on
// brief copy with the assistant.
func TestProjectBriefEdit_RendersAssistAffordance(t *testing.T) {
	root := writeAssistantFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg), WithAssistantLLM(&captureLLM{}))

	req := httptest.NewRequest(http.MethodGet, "/projects/demo/brief", nil)
	rec := httptest.NewRecorder()
	server.ProjectBriefEdit(rec, req, "demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`data-assist-kind="brief_section"`,
		`data-assist-subject="Goal"`,
		`data-assist-subject="Audience"`,
		`data-assist-subject="Success criteria"`,
	} {
		assert.Contains(t, body, want, "brief editor missing %q from AI Assist affordance", want)
	}
}

// TestProjectConfigFormEdit_AutonomyGoalHasAssistAffordance —
// the project config form's autonomy.goal textarea is the most
// prompt-shaped field on the page; it should carry the assist
// affordance.
func TestProjectConfigFormEdit_AutonomyGoalHasAssistAffordance(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`data-assist-kind="project_field"`,
		`data-assist-subject="autonomy.goal"`,
	} {
		assert.Contains(t, body, want, "project config form autonomy.goal missing %q", want)
	}
}

// TestAssistantDraft_ConflictsAction — the new conflicts mode
// sends every sibling role's FULL systemPrompt body in the
// context (not just names + descriptions like draft/critique).
// The LLM can then flag direct contradictions between prompts.
func TestAssistantDraft_ConflictsAction(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "- writer expects sources, lead doesn't surface them"}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "any"

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "conflicts")
	form.Set("currentValue", "Plan the work and delegate.")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	// Sibling FULL prompt bodies appear in the user prompt for
	// the conflict-check mode — the test fixture seeded both
	// lead/writer with their inline systemPrompts.
	user := llm.Captured.User
	assert.Contains(t, user, "writer body", "conflicts mode must include sibling prompt body, not just description")
	// System prompt frames the LLM as a conflict-checker.
	assert.Contains(t, strings.ToLower(llm.Captured.System), "conflict")
}

// TestResolveAssistantModel_RespectsProjectOverride — when
// project.assistant.model is set, it wins over the swarm
// leadRole's model. Per-LLD optional override.
func TestResolveAssistantModel_RespectsProjectOverride(t *testing.T) {
	sw := &registry.Swarm{
		LeadRole: "lead",
		Roles:    []registry.SwarmRole{{Name: "lead", Model: "lead-model"}},
	}
	proj := &registry.Project{
		Assistant: registry.ProjectAssistant{Model: "project-override-model"},
	}
	assert.Equal(t, "project-override-model", resolveAssistantModelForProject(proj, sw, "daemon-default"))

	// Empty override falls through to the swarm leadRole's
	// model — same as resolveAssistantModel.
	proj2 := &registry.Project{}
	assert.Equal(t, "lead-model", resolveAssistantModelForProject(proj2, sw, "daemon-default"))
}

// TestAssistantDraft_ProjectAssistantModelOverride — full
// integration: handler picks the override even when the swarm
// has a leadRole model.
func TestAssistantDraft_ProjectAssistantModelOverride(t *testing.T) {
	root := writeAssistantFixture(t)
	// Add an assistant.model override to the project YAML.
	projectPath := filepath.Join(root, "projects", "demo.yaml")
	existing, err := os.ReadFile(projectPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(projectPath, append(existing, []byte("\nassistant:\n  model: project-override-model\n")...), 0o644))

	llm := &captureLLM{Response: "ok"}
	server := newAssistantServer(t, root, llm)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "project-override-model", llm.Captured.Model)
}

// TestAssistantDraft_BriefAbsent — a project with no PROJECT.md
// brief still works — the system prompt is built without the
// brief sections.
func TestAssistantDraft_BriefAbsent(t *testing.T) {
	root := writeAssistantFixture(t)
	// Remove the brief; project.yaml remains so the project
	// resolves but with Project.Brief = nil.
	require.NoError(t, os.Remove(filepath.Join(root, "projects", "demo.md")))
	llm := &captureLLM{Response: "Draft without brief grounding."}
	server := newAssistantServer(t, root, llm)
	server.assistantDefaultModel = "any"

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	// User prompt must still reference sibling roles even
	// without the brief, so the assistant has SOMETHING to
	// ground on.
	assert.Contains(t, llm.Captured.User, "writer")
}

// fakeUsageRepo is a minimal stub for the persistence
// TaskLLMUsageRepository — enough surface for the assistant
// budget + record paths, anything else panics so missed wiring
// surfaces loudly.
type fakeUsageRepo struct {
	Spend     float64
	Recorded  []*persistence.TaskLLMUsage
	RecordErr error
}

func (f *fakeUsageRepo) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	if f.RecordErr != nil {
		return f.RecordErr
	}
	f.Recorded = append(f.Recorded, u)
	return nil
}
func (f *fakeUsageRepo) Upsert(_ context.Context, _ *persistence.TaskLLMUsage) error {
	panic("fakeUsageRepo.Upsert: not used by assistant tests")
}
func (f *fakeUsageRepo) List(_ context.Context, _ persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	panic("fakeUsageRepo.List: not used by assistant tests")
}
func (f *fakeUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return f.Spend, nil
}
func (f *fakeUsageRepo) DeleteOlderThan(_ context.Context, _ time.Time) (int64, error) {
	panic("fakeUsageRepo.DeleteOlderThan: not used by assistant tests")
}
func (f *fakeUsageRepo) SumCost(_ context.Context, _, _ time.Time) (float64, error) {
	panic("fakeUsageRepo.SumCost: not used by assistant tests")
}
func (f *fakeUsageRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	panic("not used")
}
func (f *fakeUsageRepo) AggregateByProject(_ context.Context, _, _ time.Time, _ int) ([]persistence.ProjectSpend, error) {
	panic("not used")
}
func (f *fakeUsageRepo) AggregateBySource(_ context.Context, _, _ time.Time, _ string) ([]persistence.SourceSpend, error) {
	panic("not used")
}
func (f *fakeUsageRepo) TimeSeriesByDay(_ context.Context, _, _ time.Time, _ string) ([]persistence.DailySpend, error) {
	panic("not used")
}
func (f *fakeUsageRepo) TopTasks(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.TaskSpend, error) {
	panic("not used")
}
func (f *fakeUsageRepo) TaskCostBreakdown(_ context.Context, _ string) ([]persistence.StepSpend, error) {
	panic("not used")
}

// TestAssistantDraft_BudgetHardCapBlocks — when the project's
// daily hard cap is already exceeded, the handler refuses the
// LLM call with 429 and doesn't fire the assistant.
func TestAssistantDraft_BudgetHardCapBlocks(t *testing.T) {
	root := writeAssistantFixture(t)
	// Add a 5 USD daily hard cap to the project.
	projectPath := filepath.Join(root, "projects", "demo.yaml")
	existing, err := os.ReadFile(projectPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(projectPath, append(existing, []byte("\nbudget:\n  daily_hard_usd: 5\n")...), 0o644))

	llm := &captureLLM{Response: "should never run"}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	usage := &fakeUsageRepo{Spend: 5.50} // already past hard cap
	server := NewServer(
		WithProjectRegistry(reg),
		WithAssistantLLM(llm),
		WithLLMUsageRepository(usage),
	)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, 0, llm.Calls)
	assert.Contains(t, rec.Body.String(), "budget")
}

// TestAssistantDraft_LogsUsage — successful LLM call writes a
// TaskLLMUsage row with source=_authoring so spend appears in
// the per-project rollup.
func TestAssistantDraft_LogsUsage(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "ok", PromptTokens: 1500, CompletionTokens: 250}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	usage := &fakeUsageRepo{}
	server := NewServer(
		WithProjectRegistry(reg),
		WithAssistantLLM(llm),
		WithLLMUsageRepository(usage),
	)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Len(t, usage.Recorded, 1)
	row := usage.Recorded[0]
	assert.Equal(t, "demo", row.ProjectID)
	assert.Equal(t, "lead-model-x", row.Model)
	assert.Equal(t, int64(1500), row.PromptTokens)
	assert.Equal(t, int64(250), row.CompletionTokens)
	assert.Equal(t, "_authoring", row.Source)
	assert.NotEmpty(t, row.ID)
}

// TestAssistantDraft_PropagatesCacheTokens — provider-native
// prompt-prefix cache fields (Bedrock / Anthropic) must land on
// the TaskLLMUsage row so /ui/spend's _authoring source shows
// the same hit-ratio + savings columns as dispatcher / executor
// sources. Mirrors the cache wiring in agent_process.go.
func TestAssistantDraft_PropagatesCacheTokens(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{
		Response:            "ok",
		PromptTokens:        2000,
		CompletionTokens:    300,
		CacheCreationTokens: 1200,
		CacheReadTokens:     800,
	}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	usage := &fakeUsageRepo{}
	server := NewServer(
		WithProjectRegistry(reg),
		WithAssistantLLM(llm),
		WithLLMUsageRepository(usage),
	)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Len(t, usage.Recorded, 1)
	row := usage.Recorded[0]
	assert.Equal(t, int64(1200), row.CacheCreationTokens)
	assert.Equal(t, int64(800), row.CacheReadTokens)
}

func TestAssistantDraft_UsesActualResponseModelForUsageAndJSON(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{
		Response:         "ok",
		Model:            "actual-provider-model",
		PromptTokens:     1500,
		CompletionTokens: 250,
	}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	usage := &fakeUsageRepo{}
	server := NewServer(
		WithProjectRegistry(reg),
		WithAssistantLLM(llm),
		WithLLMUsageRepository(usage),
	)

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Len(t, usage.Recorded, 1)
	assert.Equal(t, "actual-provider-model", usage.Recorded[0].Model)

	var body assistantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "actual-provider-model", body.Model)
}

// TestAssistantDraft_NoUsageRepoStillWorks — when no usage repo
// is wired (for tests / minimal deployments), the assistant
// still calls the LLM. Budget guard simply no-ops.
func TestAssistantDraft_NoUsageRepoStillWorks(t *testing.T) {
	root := writeAssistantFixture(t)
	llm := &captureLLM{Response: "ok"}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg), WithAssistantLLM(llm))
	// No WithLLMUsageRepository.

	form := url.Values{}
	form.Set("kind", "swarm_role")
	form.Set("projectId", "demo")
	form.Set("targetId", "demo-swarm")
	form.Set("subjectId", "lead")
	form.Set("action", "draft")
	form.Set("currentValue", "x")

	rec := httptest.NewRecorder()
	server.AssistantSuggest(rec, postAssistant(form.Encode()))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, llm.Calls)
}

// TestResolveAssistantModel — the model-resolution helper is
// exercised by the handler tests, but we pin the precedence
// rule explicitly so future refactors don't silently reorder.
func TestResolveAssistantModel(t *testing.T) {
	// 1. Swarm leadRole's model wins when set.
	sw := &registry.Swarm{
		LeadRole: "lead",
		Roles:    []registry.SwarmRole{{Name: "lead", Model: "lead-model"}},
	}
	assert.Equal(t, "lead-model", resolveAssistantModel(sw, "daemon-default"))

	// 2. When leadRole has no model, fall through to daemon default.
	sw2 := &registry.Swarm{
		LeadRole: "lead",
		Roles:    []registry.SwarmRole{{Name: "lead"}},
	}
	assert.Equal(t, "daemon-default", resolveAssistantModel(sw2, "daemon-default"))

	// 3. No swarm at all → daemon default.
	assert.Equal(t, "daemon-default", resolveAssistantModel(nil, "daemon-default"))
}

func (f *fakeUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (f *fakeUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
