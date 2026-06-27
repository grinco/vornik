package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// --- SSE fakes -------------------------------------------------

// fakeExecLookup returns a fixed execution (or an error/nil) for GetByTaskID.
type fakeExecLookup struct {
	exec *persistence.Execution
	err  error
}

func (f fakeExecLookup) GetByTaskID(_ context.Context, _ string) (*persistence.Execution, error) {
	return f.exec, f.err
}

// fakeStreamTaskLookup maps task IDs to projects for the SSE scope check.
// "demo-task" → project "demo" (the published agent); "missing" → ErrNotFound;
// anything else → project "other" (out of scope).
type fakeStreamTaskLookup struct{}

func (fakeStreamTaskLookup) Get(_ context.Context, taskID string) (*persistence.Task, error) {
	if taskID == "missing" {
		return nil, persistence.ErrNotFound
	}
	proj := "other"
	if taskID == "demo-task" {
		proj = "demo"
	}
	return &persistence.Task{ID: taskID, ProjectID: proj}, nil
}

// fakeSubscriber is a LiveSubscriber whose channel the test drives. A
// pre-buffered channel lets a single-threaded test push events then watch the
// handler drain + translate them before the synthetic terminator closes it.
type fakeSubscriber struct {
	ch       chan livepubsub.LiveEvent
	subErr   error
	canceled *bool
}

func (f *fakeSubscriber) Subscribe(_ string, _ int64) (<-chan livepubsub.LiveEvent, func(), error) {
	if f.subErr != nil {
		return nil, nil, f.subErr
	}
	return f.ch, func() {
		if f.canceled != nil {
			*f.canceled = true
		}
	}, nil
}

// wireSSEForTest installs streamDeps and restores it after the test.
func wireSSEForTest(t *testing.T, d *SSEDeps) {
	t.Helper()
	prev := streamDeps
	WireSSE(d)
	t.Cleanup(func() { streamDeps = prev })
}

// --- pure translation helpers (sse.go, all at 0% before) -------

func TestStatusFromKind_Mapping(t *testing.T) {
	cases := map[string]string{
		livepubsub.KindStepStarted:    "working",
		livepubsub.KindResumed:        "working",
		livepubsub.KindForked:         "working",
		livepubsub.KindProjectSpawned: "working",
		livepubsub.KindStepCompleted:  "completed",
		livepubsub.KindClosed:         "completed",
		livepubsub.KindPaused:         "input-required",
		"some_unknown_kind":           "working", // default arm
	}
	for kind, want := range cases {
		if got := statusFromKind(kind); got != want {
			t.Errorf("statusFromKind(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestIsTerminalKind_OnlyClosed(t *testing.T) {
	if !isTerminalKind(livepubsub.KindClosed) {
		t.Errorf("KindClosed must be terminal")
	}
	for _, k := range []string{
		livepubsub.KindStepStarted,
		livepubsub.KindStepCompleted,
		livepubsub.KindPaused,
		livepubsub.KindOutcomeRecorded,
		"anything_else",
	} {
		if isTerminalKind(k) {
			t.Errorf("kind %q must NOT be terminal", k)
		}
	}
}

func TestTranslateAndWrite_StatusFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	ev := livepubsub.LiveEvent{Kind: livepubsub.KindStepStarted, Payload: map[string]any{"step": "s1"}}
	translateAndWrite(rec, ev, "t1", false)
	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: status\n") {
		t.Errorf("expected status event, got:\n%s", body)
	}
	if !strings.Contains(body, `"state":"working"`) {
		t.Errorf("expected working state in frame:\n%s", body)
	}
	if !strings.Contains(body, `"taskId":"t1"`) {
		t.Errorf("expected taskId in frame:\n%s", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("SSE frame must end with a blank line:\n%q", body)
	}
}

func TestTranslateAndWrite_OutcomeBecomesArtifact(t *testing.T) {
	rec := httptest.NewRecorder()
	ev := livepubsub.LiveEvent{Kind: livepubsub.KindOutcomeRecorded, Payload: map[string]any{"result": "ok"}}
	translateAndWrite(rec, ev, "t1", false)
	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: artifact\n") {
		t.Errorf("outcome_recorded must translate to an artifact frame, got:\n%s", body)
	}
	if strings.Contains(body, `"state"`) {
		t.Errorf("artifact frame must not carry a status state field:\n%s", body)
	}
}

func TestTranslateAndWrite_UnknownKindIsRunningStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	ev := livepubsub.LiveEvent{Kind: "totally_unknown", Payload: nil}
	translateAndWrite(rec, ev, "t1", true)
	body := rec.Body.String()
	// Unknown kinds fall to the default arm: a status frame with "running".
	if !strings.HasPrefix(body, "event: status\n") || !strings.Contains(body, `"state":"running"`) {
		t.Errorf("unknown kind should emit a running status frame, got:\n%s", body)
	}
	if !strings.Contains(body, `"final":true`) {
		t.Errorf("final flag should propagate, got:\n%s", body)
	}
}

func TestWriteSSEFrame_CanonicalShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEFrame(rec, "status", map[string]any{"a": 1})
	want := "event: status\ndata: {\"a\":1}\n\n"
	if rec.Body.String() != want {
		t.Errorf("frame = %q, want %q", rec.Body.String(), want)
	}
}

func TestWriteSSEFrame_UnmarshalablePayloadIsDropped(t *testing.T) {
	rec := httptest.NewRecorder()
	// A channel can't be JSON-marshaled → marshal errors → nothing written.
	writeSSEFrame(rec, "status", map[string]any{"bad": make(chan int)})
	if rec.Body.Len() != 0 {
		t.Errorf("un-marshalable payload should write nothing, got %q", rec.Body.String())
	}
}

func TestStartSSE_HeadersAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	startSSE(rec)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control missing")
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering missing (nginx buffering must be disabled)")
	}
}

// --- handleTaskStream (0% before) ------------------------------

func TestHandleTaskStream_MethodGuard(t *testing.T) {
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: make(chan livepubsub.LiveEvent)}
	wireSSEForTest(t, &SSEDeps{Tasks: fakeStreamTaskLookup{}, Executions: fakeExecLookup{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleTaskStream_NotConfiguredWhenNoSubscriber(t *testing.T) {
	h, _ := newTestHandler()
	// LiveSubscriber nil → 503 even though streamDeps is wired.
	wireSSEForTest(t, &SSEDeps{Tasks: fakeStreamTaskLookup{}, Executions: fakeExecLookup{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleTaskStream_MissingTaskIs404(t *testing.T) {
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: make(chan livepubsub.LiveEvent)}
	wireSSEForTest(t, &SSEDeps{Tasks: fakeStreamTaskLookup{}, Executions: fakeExecLookup{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/missing", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandleTaskStream_OutOfScopeTaskIs404 is the SSE-side SSRF/IDOR guard: a
// task that exists but belongs to a different project must read as 404 so the
// endpoint never confirms the existence of another project's task.
func TestHandleTaskStream_OutOfScopeTaskIs404(t *testing.T) {
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: make(chan livepubsub.LiveEvent)}
	wireSSEForTest(t, &SSEDeps{Tasks: fakeStreamTaskLookup{}, Executions: fakeExecLookup{}})
	rec := httptest.NewRecorder()
	// "other-task" → project "other", routed under the "demo" agent.
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/other-task", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("out-of-scope task: status = %d, want 404", rec.Code)
	}
}

// TestHandleTaskStream_NoExecutionYet exercises the "task created but no
// execution row yet" branch: a single `submitted` status frame, then close.
func TestHandleTaskStream_NoExecutionYet(t *testing.T) {
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: make(chan livepubsub.LiveEvent)}
	// Executions lookup returns nil exec → early single-frame branch.
	wireSSEForTest(t, &SSEDeps{Tasks: fakeStreamTaskLookup{}, Executions: fakeExecLookup{exec: nil}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"state":"submitted"`) {
		t.Errorf("expected a single submitted frame, got:\n%s", body)
	}
}

// TestHandleTaskStream_SubscribeError surfaces a subscribe failure as a 500.
func TestHandleTaskStream_SubscribeError(t *testing.T) {
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{subErr: context.DeadlineExceeded}
	wireSSEForTest(t, &SSEDeps{
		Tasks:      fakeStreamTaskLookup{},
		Executions: fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestHandleTaskStream_StreamsUntilTerminal drives the full streaming loop:
// push a working event + a synthetic terminator, and confirm both frames land
// and the handler returns (cancel called) once the terminal frame is written.
func TestHandleTaskStream_StreamsUntilTerminal(t *testing.T) {
	canceled := false
	ch := make(chan livepubsub.LiveEvent, 2)
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindStepStarted, Payload: map[string]any{"step": "s1"}}
	ch <- livepubsub.LiveEvent{Kind: livepubsub.KindClosed}
	sub := &fakeSubscriber{ch: ch, canceled: &canceled}

	h, _ := newTestHandler()
	h.LiveSubscriber = sub
	wireSSEForTest(t, &SSEDeps{
		Tasks:      fakeStreamTaskLookup{},
		Executions: fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil)
	// The loop exits on the KindClosed terminal frame, so this returns.
	h.HandleAgentRoute(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"state":"working"`) {
		t.Errorf("expected working frame from step_started:\n%s", body)
	}
	// KindClosed → completed + final=true.
	if !strings.Contains(body, `"state":"completed"`) || !strings.Contains(body, `"final":true`) {
		t.Errorf("expected terminal completed/final frame:\n%s", body)
	}
	if !canceled {
		t.Errorf("subscription cancel func must run on stream exit (deferred cancel)")
	}
}

// TestHandleTaskStream_ClientDisconnect proves the loop honours a canceled
// request context (client hangup) and returns without writing a terminal
// frame. The events channel stays open (never closed, no terminator).
func TestHandleTaskStream_ClientDisconnect(t *testing.T) {
	canceled := false
	sub := &fakeSubscriber{ch: make(chan livepubsub.LiveEvent), canceled: &canceled}

	h, _ := newTestHandler()
	h.LiveSubscriber = sub
	wireSSEForTest(t, &SSEDeps{
		Tasks:      fakeStreamTaskLookup{},
		Executions: fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone before the loop starts
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil).WithContext(ctx)
	h.HandleAgentRoute(rec, req)

	if !canceled {
		t.Errorf("cancel func must run when the client disconnects")
	}
}

// --- well-known multi-agent / nil-base coverage ----------------

// TestListPublishedAgents_NilRegistry hits the nil-registry guard arm of
// listPublishedAgents via HandleWellKnown — an empty index, never a panic.
func TestHandleWellKnown_NilRegistry(t *testing.T) {
	h := &Handler{Logger: zerolog.Nop()} // Registry nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	h.HandleWellKnown(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"agents": []`) {
		t.Errorf("nil registry should yield an empty agents list:\n%s", rec.Body.String())
	}
}

// TestHandleWellKnown_MalformedSubPath covers the "wrong number of path
// segments" 404 arm (one segment, not project/workflow).
func TestHandleWellKnown_MalformedSubPath(t *testing.T) {
	h, _ := newTestHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json/only-one-segment", nil)
	h.HandleWellKnown(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
