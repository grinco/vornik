package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// TestA2ACallCov_NilStep covers the nil-step guard.
func TestA2ACallCov_NilStep(t *testing.T) {
	e := &Executor{}
	if _, err := e.handleA2ACallStep(context.Background(), "s", nil); err == nil {
		t.Fatal("nil step should error")
	}
}

// TestA2ACallCov_SubmitResponseUnparseable covers the parse-submit-
// response error branch: the partner returns 200 with a body that
// isn't valid JSON.
func TestA2ACallCov_SubmitResponseUnparseable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !contains(err.Error(), "parse submit response") {
		t.Fatalf("expected parse-submit error, got %v", err)
	}
}

// TestA2ACallCov_SubmitMissingFields covers the "submit response
// missing taskId or streamUrl" guard.
func TestA2ACallCov_SubmitMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 with an empty JSON object → both taskId + streamUrl empty.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !contains(err.Error(), "missing taskId or streamUrl") {
		t.Fatalf("expected missing-fields error, got %v", err)
	}
}

// TestA2ACallCov_StreamURLResolveError covers the handler branch
// where resolveStreamURL fails: the partner returns a relative
// stream URL that doesn't start with '/'.
func TestA2ACallCov_StreamURLResolveError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"taskId":"t1","status":"submitted","streamUrl":"relative-no-slash"}`))
	}))
	defer srv.Close()
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !contains(err.Error(), "resolve stream URL") {
		t.Fatalf("expected stream-URL resolve error, got %v", err)
	}
}

// TestA2ACallCov_CanceledTerminalState covers the "canceled"
// terminal-state arm of the result switch (distinct from "failed").
func TestA2ACallCov_CanceledTerminalState(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("status", `{"taskId":"task-fake-1","state":"canceled","final":true}`),
	})
	defer partner.Close()
	e := &Executor{}
	res, err := e.handleA2ACallStep(context.Background(), "s", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL(), Prompt: "x",
	})
	if err == nil || res == nil || res.State != "canceled" {
		t.Fatalf("expected canceled terminal error, got res=%#v err=%v", res, err)
	}
}

// TestA2ACallCov_StreamContextCancel covers the consumeA2ASSEStream
// ctx.Done() branch: the parent context is cancelled mid-stream
// while the partner is still emitting (slow drip), so the consumer
// returns ctx.Err() before a final frame arrives.
func TestA2ACallCov_StreamContextCancel(t *testing.T) {
	// Server that holds the SSE connection open without ever sending
	// a final frame.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Emit one non-final status frame then stall.
		_, _ = w.Write([]byte(sseFrame("status", `{"taskId":"t","state":"working","final":false}`)))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	state, _, err := consumeA2ASSEStream(ctx, srv.URL, "")
	if err == nil {
		t.Fatal("expected ctx-cancel error from stalled stream")
	}
	if state != "working" {
		t.Errorf("expected last-seen state 'working', got %q", state)
	}
}

// TestA2ACallCov_StreamHTTPError covers the non-2xx stream-connect
// branch of consumeA2ASSEStream.
func TestA2ACallCov_StreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()
	_, _, err := consumeA2ASSEStream(context.Background(), srv.URL, "key")
	if err == nil || !contains(err.Error(), "stream HTTP 410") {
		t.Fatalf("expected stream HTTP 410 error, got %v", err)
	}
}

// TestA2ACallCov_StreamMessagePartsAndComment exercises the
// scan-error drain path + comment-line skip + multi-line data
// accumulation. The server emits a comment keepalive, a message
// with parts, then closes WITHOUT a final frame and without a
// trailing blank line, so the last frame is flushed via the
// scanCh-closed branch.
func TestA2ACallCov_StreamFlushOnClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Comment/keepalive line (ignored), a status frame, then a
		// message frame with no trailing blank line before close.
		_, _ = w.Write([]byte(": keepalive\n"))
		_, _ = w.Write([]byte(sseFrame("status", `{"taskId":"t","state":"completed","final":false}`)))
		_, _ = w.Write([]byte("event: message\ndata: {\"text\":\"final words\"}\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Return (close) without a terminating blank line.
	}))
	defer srv.Close()
	state, text, err := consumeA2ASSEStream(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("clean close after a status frame should not error: %v", err)
	}
	if state != "completed" {
		t.Errorf("state = %q, want completed", state)
	}
	if text != "final words" {
		t.Errorf("text = %q, want 'final words'", text)
	}
}
