package api

// End-to-end integration tests for the live-execution WebSocket
// + the SSE streaming surface. Unlike the existing per-handler
// tests, these spin up a full httptest.Server + the production
// middleware chain (auth + metrics + access log) so any future
// middleware that wraps the response writer without forwarding
// http.Hijacker / http.Flusher gets caught here BEFORE the
// browser-side regression hits the operator.
//
// Discovered the metrics statusWriter Hijack passthrough bug
// (fixed 2026-05-23) was invisible to the unit tests because
// they invoked handlers directly. These tests dial through a
// real TCP listener and exercise the full upgrade.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// recordingLiveSub is a minimal LiveSubscriber that streams a
// caller-supplied event sequence into the WebSocket handler so
// the test can verify frames arrive end-to-end.
type recordingLiveSub struct {
	mu     sync.Mutex
	events chan livepubsub.LiveEvent
	cancel bool
	subbed bool
}

func newRecordingLiveSub() *recordingLiveSub {
	return &recordingLiveSub{events: make(chan livepubsub.LiveEvent, 4)}
}

func (s *recordingLiveSub) Subscribe(executionID string, fromSeq int64) (<-chan livepubsub.LiveEvent, func(), error) {
	s.mu.Lock()
	s.subbed = true
	s.mu.Unlock()
	return s.events, func() { s.cancel = true }, nil
}

func (s *recordingLiveSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	return s.events, func() {}, nil
}

func (s *recordingLiveSub) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	s.events <- livepubsub.LiveEvent{
		ExecutionID: executionID,
		Kind:        kind,
		Payload:     payload,
		Timestamp:   time.Now().UTC(),
	}
	return 0
}

// liveIntegrationServer spins the full HTTP stack: the api
// server's router + applyMiddleware chain + metrics middleware
// (the one that broke WS upgrades) + access log wrapper. The
// returned base URL is the httptest.Server's; auth is left
// disabled so the test doesn't need to mint an API key.
func liveIntegrationServer(t *testing.T) (*httptest.Server, *recordingLiveSub, *persistence.Execution) {
	t.Helper()
	exec := &persistence.Execution{
		ID:        "exec_integ_1",
		TaskID:    "task_integ_1",
		ProjectID: "proj-integ",
		Status:    persistence.ExecutionStatusRunning,
	}
	sub := newRecordingLiveSub()
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithLiveSubscriber(sub),
		WithExecutionRepository(&stubExecRepoForFork{exec: exec}),
		// metricsRegistry triggers the metrics middleware in
		// applyMiddleware — that's the wrapper we want to exercise.
		WithMetricsRegistry(prometheus.NewRegistry()),
	)
	cfg := &config.Config{Server: config.ServerConfig{Address: "127.0.0.1:0"}}
	router := NewRouter(server, cfg)
	hs := httptest.NewServer(router.Handler())
	t.Cleanup(hs.Close)
	return hs, sub, exec
}

// TestIntegration_LiveWebSocket_UpgradeAndFrame is the smoke
// test that would have caught the statusWriter Hijack
// regression: dial through the full middleware chain, expect
// the upgrade to succeed (101), publish an event server-side,
// read it back through the socket.
func TestIntegration_LiveWebSocket_UpgradeAndFrame(t *testing.T) {
	hs, sub, exec := liveIntegrationServer(t)
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/executions/" + exec.ID + "/live"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		// Same-origin Origin: httptest's hostname.
		HTTPHeader: http.Header{"Origin": []string{hs.URL}},
	})
	if err != nil {
		t.Fatalf("WS dial failed: %v — this almost certainly means a middleware wrapper dropped Hijack support", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "test done") }()

	// Send hello.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"last_seq":0}`)); err != nil {
		t.Fatalf("hello write: %v", err)
	}

	// Server-side publish; the WebSocket handler should fan it
	// out to our subscription channel and forward to the client.
	sub.Publish(ctx, exec.ID, "step_started", map[string]string{"step_id": "research"})

	// Read at least one frame within the timeout.
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	mt, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("WS read: %v", err)
	}
	if mt != websocket.MessageText {
		t.Errorf("expected text frame, got %v", mt)
	}
	if !strings.Contains(string(data), "step_started") {
		t.Errorf("frame missing event kind: %s", string(data))
	}
}

// TestIntegration_LiveWebSocket_TerminalExecutionClosesCleanly:
// the server should accept the upgrade even for a terminal-
// status execution, then close cleanly. Catches a regression
// where the server hangs the socket indefinitely instead of
// tearing it down. We don't assert on the exact frame shape
// (the handler may emit a synthetic close marker or just close
// the underlying conn) — only on the close happening within
// a bounded window.
func TestIntegration_LiveWebSocket_TerminalExecutionClosesCleanly(t *testing.T) {
	exec := &persistence.Execution{
		ID:        "exec_term",
		TaskID:    "task_term",
		ProjectID: "proj-integ",
		Status:    persistence.ExecutionStatusCompleted,
	}
	sub := newRecordingLiveSub()
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithLiveSubscriber(sub),
		WithExecutionRepository(&stubExecRepoForFork{exec: exec}),
		WithMetricsRegistry(prometheus.NewRegistry()),
	)
	cfg := &config.Config{}
	router := NewRouter(server, cfg)
	hs := httptest.NewServer(router.Handler())
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/executions/" + exec.ID + "/live"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{hs.URL}},
	})
	if err != nil {
		t.Fatalf("WS dial on terminal exec failed: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Read frames until close — exit either on a server-side
	// close (Read returns an error) or a synthetic "closed"
	// frame. Either path within the deadline is acceptable;
	// the failure mode is the read hanging forever.
	readCtx, c2 := context.WithTimeout(ctx, 4*time.Second)
	defer c2()
	for {
		if _, _, err := conn.Read(readCtx); err != nil {
			// Expected terminal: the server tore down the conn
			// or the read window expired (which would mean the
			// handler is hanging — that's still useful to
			// notice).
			if readCtx.Err() != nil {
				t.Errorf("terminal-status WebSocket did not close within the deadline (handler hung)")
			}
			return
		}
	}
}

// TestIntegration_StreamingEndpointFlushPassthrough confirms
// SSE-style streaming through the metrics middleware still
// flushes per chunk. Mounts a minimal in-test handler that
// writes + flushes; reads via http.Client.
func TestIntegration_StreamingEndpointFlushPassthrough(t *testing.T) {
	// Build a server with the metrics middleware in the chain,
	// then mount a custom SSE-shaped handler so we can verify
	// flush behaviour independently of the existing endpoints.
	apiM := NewAPIMetrics(prometheus.NewRegistry())
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("inside-handler: ResponseWriter must expose http.Flusher — middleware dropped Flush")
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
		flusher.Flush()
	})
	hs := httptest.NewServer(apiM.Middleware(mux))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 32)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "first") {
		t.Errorf("first chunk not delivered before second flush; got %q", string(buf[:n]))
	}
}
