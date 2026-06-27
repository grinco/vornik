// Package api: tests for the accessRecorder's protocol-upgrade
// surface — Flush (SSE / NDJSON) and Hijack (websocket). The
// canonical happy path is exercised in access_log_test.go; this file
// pins the lesser-used branches.
package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushOnlyWriter implements http.Flusher but not http.Hijacker.
type flushOnlyWriter struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushOnlyWriter) Flush() { f.flushed = true }

// nonFlushingWriter implements neither Flusher nor Hijacker — the
// no-op fallback path.
type nonFlushingWriter struct {
	http.ResponseWriter
}

// hijackerWriter implements http.Hijacker.
type hijackerWriter struct {
	http.ResponseWriter
	wantConn net.Conn
	wantBuf  *bufio.ReadWriter
	wantErr  error
	called   bool
}

func (h *hijackerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.called = true
	return h.wantConn, h.wantBuf, h.wantErr
}

// --- Flush -----------------------------------------------------------

func TestAccessRecorder_Flush_ForwardsWhenSupported(t *testing.T) {
	inner := &flushOnlyWriter{ResponseWriter: httptest.NewRecorder()}
	rec := &accessRecorder{ResponseWriter: inner}
	rec.Flush()
	if !inner.flushed {
		t.Errorf("expected Flush to forward to wrapped writer")
	}
}

func TestAccessRecorder_Flush_NoopWhenNotSupported(t *testing.T) {
	inner := &nonFlushingWriter{ResponseWriter: httptest.NewRecorder()}
	rec := &accessRecorder{ResponseWriter: inner}
	// must not panic when the wrapped writer is not a Flusher
	rec.Flush()
}

// --- Hijack ----------------------------------------------------------

func TestAccessRecorder_Hijack_ForwardsWhenSupported(t *testing.T) {
	wantErr := errors.New("synthetic")
	inner := &hijackerWriter{ResponseWriter: httptest.NewRecorder(), wantErr: wantErr}
	rec := &accessRecorder{ResponseWriter: inner}
	_, _, err := rec.Hijack()
	if !inner.called {
		t.Errorf("expected Hijack to forward to wrapped writer")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped writer's error, got %v", err)
	}
}

func TestAccessRecorder_Hijack_ReturnsSentinelWhenNotSupported(t *testing.T) {
	inner := &nonFlushingWriter{ResponseWriter: httptest.NewRecorder()}
	rec := &accessRecorder{ResponseWriter: inner}
	_, _, err := rec.Hijack()
	if err == nil {
		t.Fatalf("expected error from un-hijackable writer")
	}
	if !errors.Is(err, errAccessRecorderNoHijack) {
		t.Errorf("expected errAccessRecorderNoHijack, got %v", err)
	}
}

// --- WriteHeader idempotency ----------------------------------------

// Pins WriteHeader's idempotency: a second call must not change the
// captured status or forward a second header to the wrapped writer.
// This is the previously-uncovered branch in WriteHeader.
func TestAccessRecorder_WriteHeader_OnlyFiresOnce(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &accessRecorder{ResponseWriter: inner}
	rec.WriteHeader(http.StatusTeapot)
	rec.WriteHeader(http.StatusOK) // should be ignored
	if rec.status != http.StatusTeapot {
		t.Errorf("status: got %d, want 418 (second WriteHeader must be ignored)", rec.status)
	}
	if inner.Code != http.StatusTeapot {
		t.Errorf("wrapped Code: got %d, want 418", inner.Code)
	}
}
