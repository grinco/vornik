package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fakeRegistry implements RegistrySnapshot with handful of
// projects + workflows the tests build inline.
type fakeRegistry struct {
	projects  []*registry.Project
	workflows []*registry.Workflow
}

func (f *fakeRegistry) GetProject(id string) *registry.Project {
	for _, p := range f.projects {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (f *fakeRegistry) GetWorkflow(id string) *registry.Workflow {
	for _, w := range f.workflows {
		if w.ID == id {
			return w
		}
	}
	return nil
}

func (f *fakeRegistry) ListProjects() []*registry.Project   { return f.projects }
func (f *fakeRegistry) ListWorkflows() []*registry.Workflow { return f.workflows }

func newTestHandler() (*Handler, *fakeTaskCreator) {
	pub := WorkflowA2APublishYes()
	wf := &registry.Workflow{
		ID:          "research",
		DisplayName: "Research",
		Description: "Find facts.",
		Version:     "1.0.0",
		Entrypoint:  "step",
		Steps:       map[string]registry.WorkflowStep{"step": {Type: "agent", Role: "researcher"}},
		A2A:         pub,
	}
	unpub := &registry.Workflow{
		ID:         "private",
		Entrypoint: "step",
		Steps:      map[string]registry.WorkflowStep{"step": {Type: "agent", Role: "x"}},
		// A2A.Publish defaults to false — not exposed
	}
	reg := &fakeRegistry{
		projects: []*registry.Project{
			{ID: "demo", DisplayName: "Demo", DefaultWorkflowID: "research"},
			{ID: "private-only", DisplayName: "Private", DefaultWorkflowID: "private"},
		},
		workflows: []*registry.Workflow{wf, unpub},
	}
	creator := &fakeTaskCreator{}
	return &Handler{
		BaseURLProvider: PublicBaseURLFunc(func() string { return "https://daemon.example.com" }),
		Registry:        reg,
		TaskCreator:     creator,
		Logger:          zerolog.Nop(),
	}, creator
}

// WorkflowA2APublishYes returns the field value with publish=true.
// Helper only — keeps inline test fixtures shorter.
func WorkflowA2APublishYes() registry.WorkflowA2A {
	return registry.WorkflowA2A{Publish: true}
}

type fakeTaskCreator struct {
	lastParams TaskCreateParams
	returnErr  error
}

func (f *fakeTaskCreator) Create(ctx context.Context, params TaskCreateParams) (*persistence.Task, error) {
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	f.lastParams = params
	return &persistence.Task{
		ID:        "task-" + params.ProjectID + "-1",
		ProjectID: params.ProjectID,
	}, nil
}

// --- agent card / index ---------------------------------------

func TestHandleWellKnown_Index(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	h.HandleWellKnown(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var idx AgentCardIndex
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatalf("parse: %v\n%s", err, rec.Body.String())
	}
	if len(idx.Agents) != 1 {
		t.Errorf("expected 1 published agent (private workflow excluded), got %d", len(idx.Agents))
	}
	if idx.Agents[0].URL != "https://daemon.example.com/a2a/v1/agents/demo/research" {
		t.Errorf("URL: %q", idx.Agents[0].URL)
	}
}

func TestHandleWellKnown_PerAgentCard(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json/demo/research", nil)
	h.HandleWellKnown(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d\n%s", rec.Code, rec.Body.String())
	}
	var card AgentCard
	_ = json.Unmarshal(rec.Body.Bytes(), &card)
	if card.Skills[0].ID != "research" {
		t.Errorf("card lookup wrong: %#v", card)
	}
}

func TestHandleWellKnown_UnpublishedReturns404(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json/private-only/private", nil)
	h.HandleWellKnown(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unpublished workflow, got %d", rec.Code)
	}
}

func TestHandleAgentRoute_Card(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/card", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d\n%s", rec.Code, rec.Body.String())
	}
}

// --- task submit ----------------------------------------------

func TestHandleAgentRoute_TaskSubmit(t *testing.T) {
	h, creator := newTestHandler()
	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"message":{"parts":[{"type":"text","text":"Find facts about Prague"}]}}`)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d\n%s", rec.Code, rec.Body.String())
	}
	var resp taskSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasPrefix(resp.TaskID, "task-demo-") {
		t.Errorf("taskId: %q", resp.TaskID)
	}
	if !strings.Contains(resp.StreamURL, "/a2a/v1/agents/demo/research/tasks/") {
		t.Errorf("streamUrl: %q", resp.StreamURL)
	}
	if creator.lastParams.Prompt != "Find facts about Prague" {
		t.Errorf("prompt not forwarded: %q", creator.lastParams.Prompt)
	}
	if creator.lastParams.CreationSource != persistence.TaskCreationSourceA2A {
		t.Errorf("creation source: %q", creator.lastParams.CreationSource)
	}
}

func TestHandleAgentRoute_TaskSubmit_RejectsEmptyText(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"message":{"parts":[]}}`)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", rec.Code)
	}
}

func TestHandleAgentRoute_TaskSubmit_RejectsOversizedBody(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"message":{"parts":[{"type":"text","text":"` + strings.Repeat("x", maxTaskSubmitBodyBytes) + `"}]}}`)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRoute_TaskSubmit_RejectsNonTextParts(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"message":{"parts":[{"type":"file","text":""}]}}`)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", rec.Code)
	}
}

func TestHandleAgentRoute_TaskSubmit_UnpublishedRejected(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"message":{"parts":[{"type":"text","text":"x"}]}}`)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/private-only/private/tasks", body)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unpublished workflow, got %d", rec.Code)
	}
}

func TestHandleAgentRoute_BadPath(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/justone", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed path, got %d", rec.Code)
	}
}

func TestHandleWellKnown_MethodGuard(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/.well-known/agent.json", nil)
	h.HandleWellKnown(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}
