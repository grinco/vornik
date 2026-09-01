package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// deadStdioClient builds a client that behaves exactly like one whose stdio
// reader has exited: the dead latch is set, so every callStdio fails fast with
// ErrClientNotReading without touching a pipe.
func deadStdioClient(name string) *Client {
	c := &Client{
		config: ServerConfig{Name: name, Transport: "stdio", Command: "irrelevant"},
		logger: zerolog.Nop(),
	}
	c.deadReason.Store(fmt.Errorf("mcp server %s closed stdout before responding", name))
	c.dead.Store(true)
	return c
}

// swapDialSeams overrides the package dial/close seams for one test.
func swapDialSeams(t *testing.T, connect func(context.Context, ServerConfig, zerolog.Logger) (*Client, error)) {
	t.Helper()
	origConnect, origClose := connectFn, closeFn
	t.Cleanup(func() { connectFn, closeFn = origConnect, origClose })
	connectFn = connect
	closeFn = func(*Client) error { return nil } // the fakes own no subprocess
}

// TestExecute_RedialsAStdioServerThatDied is the regression test for the
// 2026-08-05 incident.
//
// Three consecutive drive_search calls failed in ~7µs each with "mcp client
// google-workspace is no longer reading responses: mcp server google-workspace
// closed stdout before responding". The dead latch was terminal for the
// daemon's lifetime, so the server stayed unreachable until a restart — and the
// assistant, reading an error that sounds like a malformed call, concluded it
// had misclicked its own tool and moved on.
func TestExecute_RedialsAStdioServerThatDied(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		return healthyClient(t, cfg.Name, "healed"), nil
	})

	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"google-workspace": deadStdioClient("google-workspace")}

	out, err := mgr.Execute(context.Background(), "proj", "mcp__google-workspace__drive_search", `{"query":"x"}`)
	require.NoError(t, err, "a dead connection must be re-dialled, not returned as a permanent failure")
	require.Equal(t, "healed", out)
	require.Equal(t, int64(1), dials.Load(), "exactly one re-dial")

	// And the catalog now holds the live client, so the NEXT call needs no
	// re-dial at all — the connection is genuinely healed, not papered over
	// once per call.
	out, err = mgr.Execute(context.Background(), "proj", "mcp__google-workspace__drive_search", `{"query":"y"}`)
	require.NoError(t, err)
	require.Equal(t, "healed", out)
	require.Equal(t, int64(1), dials.Load(), "the healed client must be reused")
}

// TestExecute_RedialFailureExplainsItselfToTheModel — when the server really is
// gone, the message has to read as an infrastructure fault. The raw wording
// ("no longer reading responses") describes a pipe, and a model reasonably
// hears "you called me wrong" and starts varying its arguments.
func TestExecute_RedialFailureExplainsItselfToTheModel(t *testing.T) {
	swapDialSeams(t, func(_ context.Context, _ ServerConfig, _ zerolog.Logger) (*Client, error) {
		return nil, errors.New("exec: \"gws-mcp\": executable file not found in $PATH")
	})

	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"gws": deadStdioClient("gws")}

	_, err := mgr.Execute(context.Background(), "proj", "mcp__gws__drive_search", `{}`)
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "is not running", "the model must be told the SERVER is down")
	require.Contains(t, msg, "not a problem with the arguments",
		"without this the model blames its own call and retries variations")
	require.Contains(t, msg, "executable file not found", "keep the operator-facing cause")
}

// TestExecute_ConcurrentCallsAgainstADeadServerRedialOnce — the failure mode
// that makes naive reconnect logic worse than none. A dead stdio server is
// usually noticed by several in-flight calls at once; one subprocess per caller
// would be a fork bomb against an already-unhealthy server.
func TestExecute_ConcurrentCallsAgainstADeadServerRedialOnce(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		return healthyClient(t, cfg.Name, "ok"), nil
	})

	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"gws": deadStdioClient("gws")}

	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = mgr.Execute(context.Background(), "proj", "mcp__gws__drive_search", `{}`)
		}()
	}
	wg.Wait()

	require.Equal(t, int64(1), dials.Load(),
		"25 concurrent callers must produce ONE new subprocess, not one each")
}

// A redial races config reload in production: the tool call notices the old
// client is dead while SyncProjects is building and swapping the new catalog.
// If the reload removes only this server (but keeps the project), the redial
// must not put the stale server -- and its stale credentials -- back into the
// newly activated catalog.
func TestExecute_RedialDoesNotResurrectServerRemovedByReload(t *testing.T) {
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	closedFresh := make(chan struct{}, 1)
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		close(dialStarted)
		<-releaseDial
		return healthyClient(t, cfg.Name, "healed"), nil
	})
	closeFn = func(*Client) error {
		closedFresh <- struct{}{}
		return nil
	}

	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"gws": deadStdioClient("gws")}

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), "proj", "mcp__gws__drive_search", `{}`)
		done <- err
	}()

	<-dialStarted
	// Simulate the atomic catalog swap performed by a reload that keeps the
	// project but removes this one server.
	mgr.mu.Lock()
	mgr.clients["proj"] = map[string]*Client{}
	mgr.mu.Unlock()
	close(releaseDial)

	require.Error(t, <-done, "the call must observe that the server was removed")
	require.Empty(t, mgr.Tools("proj"), "a stale redial must not resurrect removed tools")
	select {
	case <-closedFresh:
	case <-time.After(time.Second):
		t.Fatal("the stale client built by the losing redial was not closed")
	}
}

// A failure that is NOT a dead connection must never trigger a re-dial —
// re-dialling on a bad argument or a vendor 4xx would kill a healthy server.
func TestExecute_OrdinaryToolFailureDoesNotRedial(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		return healthyClient(t, cfg.Name, "ok"), nil
	})

	mgr := NewManager(zerolog.Nop())
	// A HEALTHY client whose allowed_tools excludes the tool: CallTool fails
	// before any transport work, which is an ordinary failure, not a dead pipe.
	mgr.clients["proj"] = map[string]*Client{"gws": restrictedClient(t, "gws")}

	_, err := mgr.Execute(context.Background(), "proj", "mcp__gws__drive_search", `{}`)
	require.Error(t, err)
	require.Zero(t, dials.Load(), "an ordinary tool failure must not re-dial a healthy server")
}

// healthyClient is a live client (over the sse transport, which needs no
// subprocess) whose single tool returns the given text.
func healthyClient(t *testing.T, name, text string) *Client {
	t.Helper()
	return managerCovSSEClient(t, name, func(string) map[string]any {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	})
}

// restrictedClient is live and well, but its allowed_tools excludes the tool
// the test calls — so CallTool fails without the connection being at fault.
func restrictedClient(t *testing.T, name string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"tools": []map[string]any{{"name": "do_thing"}}},
		})
	}))
	t.Cleanup(srv.Close)
	c, err := Connect(context.Background(), ServerConfig{
		Name: name, Transport: "sse", URL: srv.URL,
		AllowedTools: []string{"some_other_tool"},
	}, zerolog.Nop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}
