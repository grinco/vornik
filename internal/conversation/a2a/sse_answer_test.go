package a2a

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// delayedExecLookup returns nil for the first readyAfter calls, then the
// execution — modelling the QUEUED→leased delay a fast A2A client races.
type delayedExecLookup struct {
	exec       *persistence.Execution
	readyAfter int32
	calls      atomic.Int32
}

func (d *delayedExecLookup) GetByTaskID(_ context.Context, _ string) (*persistence.Execution, error) {
	if d.calls.Add(1) <= d.readyAfter {
		return nil, persistence.ErrNotFound
	}
	return d.exec, nil
}

// Regression for the 2026-08-01 loopback e2e failure: a fast A2A client opened
// the SSE stream before the task was leased (no execution row yet); the handler
// closed with a lone "submitted" and the client errored `partner ended in state
// "submitted"` even though the task ran to completion. The stream must now WAIT
// for the execution, then stream to terminal.
func TestHandleTaskStream_WaitsForExecutionThenStreams(t *testing.T) {
	restore := shortExecutionWait(t)
	defer restore()
	// Give the poller room to tick a couple of times before the exec appears.
	executionWaitTimeout = 3 * time.Second

	ch := make(chan livepubsub.LiveEvent, 1)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: ch}

	out := &persistence.Artifact{ID: "a1", Name: "result.json", ArtifactClass: persistence.ArtifactClassOutput}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupWithResult{envelope: nil},
		Executions:     &delayedExecLookup{exec: &persistence.Execution{ID: "e1", TaskID: "demo-task"}, readyAfter: 1},
		Artifacts:      fakeArtifactLister{arts: []*persistence.Artifact{out}},
		ArtifactOpener: fakeArtifactOpener{content: map[string]string{"a1": `{"answer":"queue-backed leasing"}`}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"state":"submitted"`) {
		t.Errorf("expected an initial submitted frame while waiting:\n%s", body)
	}
	if !strings.Contains(body, `"state":"completed"`) || !strings.Contains(body, "queue-backed leasing") {
		t.Fatalf("stream must reach terminal + carry the answer after the execution appears:\n%s", body)
	}
}

// taskLookupWithResult returns "demo-task" in project "demo" carrying a
// ResultEnvelope — the workflow's validated answer.
type taskLookupWithResult struct{ envelope []byte }

func (l taskLookupWithResult) Get(_ context.Context, taskID string) (*persistence.Task, error) {
	return &persistence.Task{ID: taskID, ProjectID: "demo", ResultEnvelope: l.envelope}, nil
}

// taskLookupStatus returns a task with a fixed status (for the terminal-race).
type taskLookupStatus struct{ status persistence.TaskStatus }

func (l taskLookupStatus) Get(_ context.Context, taskID string) (*persistence.Task, error) {
	return &persistence.Task{ID: taskID, ProjectID: "demo", Status: l.status}, nil
}

// Regression for the 2026-08-01 "stuck consumer": the task finished before the
// live subscription, so subscribing yielded an open channel with NO events and
// the stream hung on keepalives forever. The bridge must detect the already-
// terminal status and deliver the answer + terminal without any live event.
func TestHandleTaskStream_AlreadyTerminalDeliversAnswer(t *testing.T) {
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: make(chan livepubsub.LiveEvent)} // never emits
	out := &persistence.Artifact{ID: "a1", Name: "result.json", ArtifactClass: persistence.ArtifactClassOutput}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupStatus{status: persistence.TaskStatusCompleted},
		Executions:     fakeExecLookup{exec: &persistence.Execution{ID: "e1", TaskID: "demo-task"}},
		Artifacts:      fakeArtifactLister{arts: []*persistence.Artifact{out}},
		ArtifactOpener: fakeArtifactOpener{content: map[string]string{"a1": `{"answer":"already done"}`}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	done := make(chan struct{})
	go func() { h.HandleAgentRoute(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream hung on an already-terminal task")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "already done") || !strings.Contains(body, `"state":"completed"`) || !strings.Contains(body, `"final":true`) {
		t.Fatalf("already-terminal task must deliver answer + completed/final:\n%s", body)
	}
}

// fakeArtifactLister / fakeArtifactOpener stand in for the OUTPUT-artifact
// source — the REAL place a completed top-level task's answer lives.
type fakeArtifactLister struct{ arts []*persistence.Artifact }

func (f fakeArtifactLister) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return f.arts, nil
}

type fakeArtifactOpener struct{ content map[string]string }

func (f fakeArtifactOpener) Open(_ context.Context, id string) (io.ReadCloser, error) {
	c, ok := f.content[id]
	if !ok {
		return nil, errors.New("artifact not found")
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

// The real answer channel: a completed top-level task's deliverable is its
// non-transcript OUTPUT-class artifact content, NOT Task.ResultEnvelope
// (which is unwired for top-level tasks). The bridge must read it.
func TestHandleTaskStream_EmitsAnswerFromOutputArtifact(t *testing.T) {
	ch := make(chan livepubsub.LiveEvent, 1)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: ch}

	out := &persistence.Artifact{ID: "art-1", Name: "answer.md", ArtifactClass: persistence.ArtifactClassOutput}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupWithResult{envelope: nil}, // NO ResultEnvelope
		Executions:     fakeExecLookup{exec: &persistence.Execution{ID: "e1", TaskID: "demo-task"}},
		Artifacts:      fakeArtifactLister{arts: []*persistence.Artifact{out}},
		ArtifactOpener: fakeArtifactOpener{content: map[string]string{"art-1": "Vornik schedules via a postgres lease loop."}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: artifact") || !strings.Contains(body, "postgres lease loop") {
		t.Fatalf("answer must come from the OUTPUT artifact content:\n%s", body)
	}
}

// Transcript-class OUTPUT artifacts (…-response.md) are step diagnostics, not
// the deliverable — they must be excluded (matches companion result()).
func TestHandleTaskStream_ExcludesTranscriptArtifacts(t *testing.T) {
	ch := make(chan livepubsub.LiveEvent, 1)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: ch}

	transcript := &persistence.Artifact{ID: "t-1", Name: "expert-response.md", ArtifactClass: persistence.ArtifactClassOutput}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupWithResult{envelope: nil},
		Executions:     fakeExecLookup{exec: &persistence.Execution{ID: "e1", TaskID: "demo-task"}},
		Artifacts:      fakeArtifactLister{arts: []*persistence.Artifact{transcript}},
		ArtifactOpener: fakeArtifactOpener{content: map[string]string{"t-1": "raw transcript"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)

	if strings.Contains(rec.Body.String(), "raw transcript") {
		t.Fatalf("transcript artifact must be excluded from the answer:\n%s", rec.Body.String())
	}
}

// The live event stream carries STATUS only — no event holds the agent's
// answer text (OutcomeRecorded is class/notes; LLM tokens aren't proxied).
// The answer lives in Task.ResultEnvelope, so on terminal the bridge must
// emit it as an `event: artifact` frame or the A2A caller gets no answer
// (a2a-expert-federation-design §7).
func TestHandleTaskStream_EmitsAnswerArtifactOnTerminal(t *testing.T) {
	ch := make(chan livepubsub.LiveEvent, 1)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}
	sub := &fakeSubscriber{ch: ch}

	h, _ := newTestHandler()
	h.LiveSubscriber = sub
	wireSSEForTest(t, &SSEDeps{
		Tasks:      taskLookupWithResult{envelope: []byte(`{"answer":"grounded product answer","citations":["doc-1"]}`)},
		Executions: fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: artifact") {
		t.Fatalf("terminal stream must emit an artifact frame carrying the answer:\n%s", body)
	}
	if !strings.Contains(body, "grounded product answer") {
		t.Fatalf("artifact frame must carry the ResultEnvelope answer:\n%s", body)
	}
	// The terminal status frame must still be present and final.
	if !strings.Contains(body, `"state":"completed"`) || !strings.Contains(body, `"final":true`) {
		t.Fatalf("terminal completed/final status frame missing:\n%s", body)
	}
}

// A terminal task with no ResultEnvelope emits no artifact frame (no crash,
// just the terminal status) — e.g. a workflow that produced no envelope.
func TestHandleTaskStream_NoEnvelopeNoArtifact(t *testing.T) {
	ch := make(chan livepubsub.LiveEvent, 1)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}
	sub := &fakeSubscriber{ch: ch}

	h, _ := newTestHandler()
	h.LiveSubscriber = sub
	wireSSEForTest(t, &SSEDeps{
		Tasks:      taskLookupWithResult{envelope: nil},
		Executions: fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)

	if strings.Contains(rec.Body.String(), "event: artifact") {
		t.Fatalf("no ResultEnvelope → no artifact frame:\n%s", rec.Body.String())
	}
}
