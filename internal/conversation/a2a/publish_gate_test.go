package a2a

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/registry"
)

func publishGateHandler(projects []*registry.Project, workflows []*registry.Workflow) *Handler {
	return &Handler{
		BaseURLProvider: PublicBaseURLFunc(func() string { return "https://daemon.example.com" }),
		Registry:        &fakeRegistry{projects: projects, workflows: workflows},
		Logger:          zerolog.Nop(),
	}
}

func agentSet(h *Handler) map[string]bool {
	out := map[string]bool{}
	for _, a := range h.listPublishedAgents() {
		out[a.ProjectID+"/"+a.WorkflowID] = true
	}
	return out
}

// A non-default workflow with an explicit project binding publishes ONLY
// under the bound project — not cartesian across every project (the
// bug a naive gate relaxation would introduce).
func TestListPublishedAgents_NonDefaultWorkflowBindsToItsProjects(t *testing.T) {
	productQA := &registry.Workflow{
		ID:         "product-qa",
		Version:    "1.0.0",
		Entrypoint: "answer",
		Steps:      map[string]registry.WorkflowStep{"answer": {Type: "agent", Role: "expert"}},
		A2A:        registry.WorkflowA2A{Publish: true, Projects: []string{"companion-example"}},
	}
	projects := []*registry.Project{
		{ID: "companion-example", DefaultWorkflowID: "adaptive"},
		{ID: "janka", DefaultWorkflowID: "adaptive"},
	}
	got := agentSet(publishGateHandler(projects, []*registry.Workflow{productQA}))

	if !got["companion-example/product-qa"] {
		t.Error("product-qa must publish under its bound project companion-example")
	}
	if got["janka/product-qa"] {
		t.Error("product-qa must NOT publish under janka (cartesian leak)")
	}
}

// Back-compat: a published workflow with no explicit Projects binding
// still publishes as the default workflow of its project.
func TestListPublishedAgents_DefaultWorkflowBackCompat(t *testing.T) {
	research := &registry.Workflow{
		ID:         "research",
		Version:    "1.0.0",
		Entrypoint: "step",
		Steps:      map[string]registry.WorkflowStep{"step": {Type: "agent", Role: "r"}},
		A2A:        registry.WorkflowA2A{Publish: true}, // no Projects → default-workflow behavior
	}
	projects := []*registry.Project{
		{ID: "demo", DefaultWorkflowID: "research"},
		{ID: "other", DefaultWorkflowID: "adaptive"},
	}
	got := agentSet(publishGateHandler(projects, []*registry.Workflow{research}))

	if !got["demo/research"] {
		t.Error("default-workflow publish (no Projects) must still work")
	}
	if got["other/research"] {
		t.Error("research must not publish under a project that doesn't default to it")
	}
}

// A published workflow bound to a project that doesn't exist publishes
// nowhere (no panic, no leak).
func TestListPublishedAgents_UnknownBoundProjectPublishesNowhere(t *testing.T) {
	wf := &registry.Workflow{
		ID:         "product-qa",
		Version:    "1.0.0",
		Entrypoint: "answer",
		Steps:      map[string]registry.WorkflowStep{"answer": {Type: "agent", Role: "x"}},
		A2A:        registry.WorkflowA2A{Publish: true, Projects: []string{"ghost"}},
	}
	projects := []*registry.Project{{ID: "companion-example", DefaultWorkflowID: "adaptive"}}
	if len(agentSet(publishGateHandler(projects, []*registry.Workflow{wf}))) != 0 {
		t.Error("workflow bound to a nonexistent project must publish nowhere")
	}
}
