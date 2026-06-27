// Package ui: hermetic tests for TaskEventsStream. Uses
// context.WithCancel + a publish goroutine to drive the SSE loop
// deterministically; no sleeps, no real-world tickers.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nonFlushingResponseWriter implements http.ResponseWriter without
// http.Flusher so we can hit the "streaming unsupported" branch.
type nonFlushingResponseWriter struct {
	header http.Header
	code   int
	body   *strings.Builder
}

func (n *nonFlushingResponseWriter) Header() http.Header {
	if n.header == nil {
		n.header = make(http.Header)
	}
	return n.header
}

func (n *nonFlushingResponseWriter) Write(p []byte) (int, error) {
	if n.body == nil {
		n.body = &strings.Builder{}
	}
	return n.body.Write(p)
}

func (n *nonFlushingResponseWriter) WriteHeader(code int) {
	n.code = code
}

func TestTaskEventsStream_NoSSEBus(t *testing.T) {
	srv := &Server{} // sseBus nil
	req := httptest.NewRequest(http.MethodGet, "/tasks/t1/events", nil)
	rec := httptest.NewRecorder()
	srv.TaskEventsStream(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestTaskEventsStream_BadTaskID(t *testing.T) {
	srv := NewServer() // NewServer allocates sseBus
	cases := []struct{ path string }{
		{"/tasks//events"},    // empty id
		{"/tasks/a/b/events"}, // id contains /
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.TaskEventsStream(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400 for %q", rec.Code, tc.path)
			}
		})
	}
}

func TestTaskEventsStream_StreamingUnsupported(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/tasks/t1/events", nil)
	rec := &nonFlushingResponseWriter{}
	srv.TaskEventsStream(rec, req)
	if rec.code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.code)
	}
}

func TestTaskEventsStream_HelloAndEvent_ThenContextCancel(t *testing.T) {
	srv := NewServer()
	// Cancellable context — we'll cancel after one event lands.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/tasks/t1/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Fire a publish on the bus shortly after the handler starts.
	// The publish is non-blocking; the subscribe inside the handler
	// runs on the main goroutine, so we kick a goroutine that
	// retries Publish until at least one subscriber has registered.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Wait until the subscriber exists. The handler subscribes
		// before flushing the hello, so we can publish straight
		// away after a tiny yield. Bounded wait — give up after
		// 200ms in case the test runs single-threaded.
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			srv.sseBus.mu.RLock()
			if len(srv.sseBus.subscribers["t1"]) > 0 {
				srv.sseBus.mu.RUnlock()
				break
			}
			srv.sseBus.mu.RUnlock()
			// Yield so the main handler goroutine can register.
			time.Sleep(1 * time.Millisecond)
		}
		srv.sseBus.Publish("t1", SSEEvent{Kind: "status", Data: "RUNNING"})
		// Give the handler time to flush the event then cancel.
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	srv.TaskEventsStream(rec, req)
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: hello") {
		t.Errorf("missing hello event: %s", body)
	}
	if !strings.Contains(body, "event: status") {
		t.Errorf("missing status event: %s", body)
	}
	if !strings.Contains(body, "data: RUNNING") {
		t.Errorf("missing event data: %s", body)
	}
}

func TestTaskEventsStream_DoubleNewlineInDataIsEscaped(t *testing.T) {
	srv := NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/tasks/t2/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			srv.sseBus.mu.RLock()
			if len(srv.sseBus.subscribers["t2"]) > 0 {
				srv.sseBus.mu.RUnlock()
				break
			}
			srv.sseBus.mu.RUnlock()
			time.Sleep(1 * time.Millisecond)
		}
		srv.sseBus.Publish("t2", SSEEvent{Kind: "message", Data: "line1\n\nline2"})
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	srv.TaskEventsStream(rec, req)
	<-done

	body := rec.Body.String()
	// "\n\n" must have been collapsed to a single "\n" so the SSE
	// frame doesn't terminate early.
	if strings.Contains(body, "line1\n\nline2") {
		t.Errorf("expected double-newline collapsed: %q", body)
	}
	if !strings.Contains(body, "line1\nline2") {
		t.Errorf("expected single-newline form: %q", body)
	}
}
