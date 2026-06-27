package api

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hijackableResponseWriter is a minimal stand-in for the real
// net/http server's ResponseWriter — it implements Hijack so
// tests can verify a wrapping middleware preserves the
// interface assertion. httptest.ResponseRecorder does NOT
// implement Hijack, so we can't use it for this check.
type hijackableResponseWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

// TestStatusWriter_PreservesHijack pins the regression that
// broke the live-execution WebSocket: when the metrics
// middleware wraps the response writer, the wrapper MUST still
// expose http.Hijacker so the WebSocket library can take over
// the underlying TCP connection. Without this, /api/v1/
// executions/{id}/live returns 501 "Not Implemented" on every
// upgrade attempt.
func TestStatusWriter_PreservesHijack(t *testing.T) {
	inner := &hijackableResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: inner, status: 200}

	hj, ok := http.ResponseWriter(sw).(http.Hijacker)
	if !ok {
		t.Fatalf("statusWriter must implement http.Hijacker (WS upgrade fails otherwise)")
	}
	if _, _, err := hj.Hijack(); err != nil {
		t.Errorf("Hijack passthrough should succeed: %v", err)
	}
	if !inner.hijacked {
		t.Errorf("inner ResponseWriter's Hijack should have been called")
	}
}

// TestStatusWriter_ExposesFlush mirrors the Hijack test for
// SSE / streaming responses. Without Flush, /ui/tasks/<id>/
// logs/stream would buffer indefinitely.
func TestStatusWriter_ExposesFlush(t *testing.T) {
	inner := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: inner, status: 200}
	if _, ok := http.ResponseWriter(sw).(http.Flusher); !ok {
		t.Errorf("statusWriter must implement http.Flusher for SSE / streaming")
	}
}
