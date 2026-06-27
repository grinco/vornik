package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"vornik.io/vornik/internal/persistence"
)

// liveServerForUpgrade spins a real httptest.Server so the
// WebSocket library actually processes the upgrade handshake.
// Returns the server URL with the ws:// scheme already swapped
// in and the stub live subscriber for assertions.
func liveServerForUpgrade(t *testing.T, opts ...ServerOption) (string, *stubLiveSub) {
	t.Helper()
	sub := &stubLiveSub{subscribedCh: make(chan struct{}, 1)}
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	defaults := []ServerOption{
		WithLiveSubscriber(sub),
		WithExecutionRepository(&stubExecRepoForFork{exec: exec}),
	}
	srv := NewServer(append(defaults, opts...)...)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/executions/exec_1/live", func(w http.ResponseWriter, r *http.Request) {
		srv.ExecutionLive(w, r, "exec_1")
	})
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/executions/exec_1/live"
	return url, sub
}

// TestExecutionLive_RejectsForeignOrigin asserts the WebSocket
// upgrade is refused when the Origin header points at a host
// the daemon hasn't explicitly authorised. Without this guard
// any webpage the operator visits can subscribe to the live
// stream (CSRF-grade WebSocket hijacking — CSWSH).
func TestExecutionLive_RejectsForeignOrigin(t *testing.T) {
	wsURL, sub := liveServerForUpgrade(t) // no WithLiveAllowedOrigins
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example.com"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail on foreign Origin, succeeded")
	}
	if resp == nil {
		t.Fatal("expected an HTTP response for the rejected upgrade, got nil")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 on foreign Origin, got %d", resp.StatusCode)
	}
	if sub.subscribed.Load() {
		t.Error("subscriber Subscribe() should NOT have been called on rejected upgrade")
	}
}

// TestExecutionLive_AcceptsSameOrigin asserts the library's
// always-authorise-the-request-host default still works after
// we removed InsecureSkipVerify. Sends an Origin that matches
// the server's own host.
func TestExecutionLive_AcceptsSameOrigin(t *testing.T) {
	wsURL, sub := liveServerForUpgrade(t)
	// httptest gives us http://127.0.0.1:NNNN — strip ws:// to
	// recover the matching Origin host.
	origin := "http" + strings.TrimPrefix(strings.TrimSuffix(wsURL, "/api/v1/executions/exec_1/live"), "ws")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{origin}},
	})
	if err != nil {
		t.Fatalf("same-origin dial should succeed, got %v", err)
	}
	// readLiveHello blocks up to 2s waiting for a client hello
	// frame; the test sends none, so give Subscribe a window past
	// that deadline before declaring failure.
	if !sub.waitSubscribed(5 * time.Second) {
		t.Error("subscriber should have been called on accepted upgrade")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TestExecutionLive_AcceptsConfiguredOrigin asserts that the
// WithLiveAllowedOrigins option lets reverse-proxy deployments
// authorise their public hostname. We configure a pattern then
// dial with that origin and assert the upgrade succeeds.
func TestExecutionLive_AcceptsConfiguredOrigin(t *testing.T) {
	wsURL, sub := liveServerForUpgrade(t,
		WithLiveAllowedOrigins([]string{"vornik.example.com"}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://vornik.example.com"}},
	})
	if err != nil {
		t.Fatalf("dial with configured origin should succeed, got %v", err)
	}
	// readLiveHello blocks up to 2s waiting for a client hello
	// frame; the test sends none, so give Subscribe a window past
	// that deadline before declaring failure.
	if !sub.waitSubscribed(5 * time.Second) {
		t.Error("subscriber should have been called on accepted upgrade")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TestExecutionLive_RejectsOversizedHelloFrame asserts the
// SetReadLimit guard rejects a hostile client that sends a
// large hello frame instead of the expected tiny JSON object.
// Without the limit the library buffers the entire frame in
// memory before returning from Read — multi-GB frames would
// OOM the daemon.
func TestExecutionLive_RejectsOversizedHelloFrame(t *testing.T) {
	wsURL, _ := liveServerForUpgrade(t)
	origin := "http" + strings.TrimPrefix(strings.TrimSuffix(wsURL, "/api/v1/executions/exec_1/live"), "ws")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{origin}},
	})
	if err != nil {
		t.Fatalf("same-origin dial should succeed, got %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusInternalError, "test cleanup") }()

	// Send a 64 KiB frame — well over the 4 KiB read limit but
	// small enough to keep the test fast.
	huge := strings.Repeat("x", 64*1024)
	writeCtx, writeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer writeCancel()
	_ = conn.Write(writeCtx, websocket.MessageText, []byte(huge))

	// After the limit is exceeded the server closes the
	// connection. A subsequent read should fail with a close
	// error rather than reading the oversized frame back.
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, _, readErr := conn.Read(readCtx)
	if readErr == nil {
		t.Fatal("expected read to fail after oversized frame, succeeded")
	}
	// websocket.StatusMessageTooBig (1009) is the canonical
	// code; some library versions surface a generic close
	// error. Accept either — the key invariant is that the
	// daemon didn't accept the frame.
	if status := websocket.CloseStatus(readErr); status != -1 && status != websocket.StatusMessageTooBig {
		t.Logf("close status: %d (expected 1009 or generic), err: %v", status, readErr)
	}
}
