package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	a2aclient "vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestA2A_FullRoundTrip_RealHandlerAndClient exercises the complete A2A
// loop the feature exists for: the real inbound handler (agent card +
// task submit + SSE bridge) on one side, and the real shared outbound
// client (internal/a2a/client — the same code the a2a_call step and the
// coming consult tools use) on the other, over a real HTTP+SSE transport.
//
// It proves the answer round-trips: the expert publishes a workflow, the
// consumer submits a question, the bridge emits the task's ResultEnvelope
// as an artifact frame, and the client surfaces it as the answer. This is
// the end-to-end contract that the two halves were built to satisfy.
func TestA2A_FullRoundTrip_RealHandlerAndClient(t *testing.T) {
	// Expert side: a published "research" workflow on project "demo".
	wf := &registry.Workflow{
		ID:          "research",
		DisplayName: "Vornik Architecture Expert",
		Description: "Answers questions about the Vornik architecture.",
		Version:     "1.0.0",
		Entrypoint:  "step",
		Steps:       map[string]registry.WorkflowStep{"step": {Type: "agent", Role: "expert"}},
		A2A:         registry.WorkflowA2A{Publish: true},
	}
	reg := &fakeRegistry{
		projects:  []*registry.Project{{ID: "demo", DefaultWorkflowID: "research"}},
		workflows: []*registry.Workflow{wf},
	}

	// The bridge streams a single terminal event; on terminal it re-fetches
	// the task and emits its ResultEnvelope (the expert's grounded answer).
	ch := make(chan livepubsub.LiveEvent, 1)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}

	h := &Handler{
		// Empty base URL → path-only stream URL, which the client reattaches
		// to the test server's host (the production path-not-set behaviour).
		BaseURLProvider: PublicBaseURLFunc(func() string { return "" }),
		Registry:        reg,
		TaskCreator:     &fakeTaskCreator{},
		LiveSubscriber:  &fakeSubscriber{ch: ch},
		Logger:          zerolog.Nop(),
	}
	// The expert workflow's deliverable is an OUTPUT-class artifact (a
	// result.json), exactly as a real top-level task produces it.
	out := &persistence.Artifact{ID: "art-1", Name: "result.json", ArtifactClass: persistence.ArtifactClassOutput}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupWithResult{envelope: nil},
		Executions:     fakeExecLookup{exec: &persistence.Execution{ID: "e1", TaskID: "task-demo-1"}},
		Artifacts:      fakeArtifactLister{arts: []*persistence.Artifact{out}},
		ArtifactOpener: fakeArtifactOpener{content: map[string]string{"art-1": `{"answer":"Vornik uses a postgres queue-backed executor with leased tasks.","citations":["arch-lld"]}`}},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent.json", h.HandleWellKnown)
	mux.HandleFunc("/.well-known/agent.json/", h.HandleWellKnown)
	mux.HandleFunc("/a2a/v1/agents/", h.HandleAgentRoute)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Consumer side: the real outbound client calls the published agent.
	res, err := a2aclient.New().Call(context.Background(), a2aclient.CallRequest{
		AgentURL: srv.URL + "/a2a/v1/agents/demo/research",
		Text:     "How does the Vornik executor schedule work?",
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("round-trip Call failed: %v", err)
	}
	if res.State != "completed" {
		t.Fatalf("terminal state = %q, want completed", res.State)
	}
	if !strings.Contains(res.Answer, "queue-backed executor") {
		t.Fatalf("answer did not round-trip through the A2A loop: %q", res.Answer)
	}
	if res.TaskID != "task-demo-1" {
		t.Errorf("taskID = %q, want task-demo-1", res.TaskID)
	}
}
