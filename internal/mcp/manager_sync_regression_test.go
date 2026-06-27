package mcp

import (
	"context"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These are the regressions for the 2026-06-04 bug-sweep finding: the
// config-reload activator tore the live MCP manager down with Close()
// and then re-dialled every server, so for the duration of the
// reconnects (up to 30s per server) every in-flight and incoming
// Execute/Tools call — in unrelated projects too — failed with "not
// connected". SyncProjects builds the new catalog first, swaps under
// one write-lock acquisition, and closes the displaced clients after.

// TestSyncProjects_NoEmptyWindowDuringReconnect — while the new
// clients are still dialling, consumers must keep seeing the complete
// OLD catalog; after the swap they see the complete new one.
func TestSyncProjects_NoEmptyWindowDuringReconnect(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()

	connectStarted := make(chan struct{})
	releaseConnect := make(chan struct{})
	var startOnce sync.Once
	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		startOnce.Do(func() { close(connectStarted) })
		<-releaseConnect // a slow re-dial — the outage window pre-fix
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())
	// Pre-existing healthy catalog (the pre-reload state).
	mgr.mu.Lock()
	mgr.clients["p1"] = map[string]*Client{
		"gmail": newFakeClient(ServerConfig{Name: "gmail"}, []Tool{{Name: "send"}}),
	}
	mgr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		mgr.SyncProjects(context.Background(), map[string][]ServerConfig{
			"p1": {{Name: "gmail"}},
		})
		close(done)
	}()

	<-connectStarted
	// Mid-reconnect: the old catalog must still serve. Pre-fix
	// (Close-then-redial) this window returned nothing.
	tools := mgr.Tools("p1")
	require.NotEmpty(t, tools, "catalog empty during reconnect — the reload outage window is back")
	require.Contains(t, tools[0].Function.Name, "send", "the OLD catalog serves until the swap")

	close(releaseConnect)
	<-done

	// After the swap: the new catalog, atomically.
	tools = mgr.Tools("p1")
	require.Len(t, tools, 1)
	require.Contains(t, tools[0].Function.Name, "ping")
}

// TestSyncProjects_DropsUndeclaredProjects — a project that no longer
// declares MCP servers loses its clients on the sync (the old reload
// achieved this via the blanket Close; the swap must too).
func TestSyncProjects_DropsUndeclaredProjects(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()
	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())
	mgr.mu.Lock()
	mgr.clients["kept"] = map[string]*Client{"gmail": newFakeClient(ServerConfig{Name: "gmail"}, []Tool{{Name: "send"}})}
	mgr.clients["removed"] = map[string]*Client{"slack": newFakeClient(ServerConfig{Name: "slack"}, []Tool{{Name: "post"}})}
	mgr.mu.Unlock()

	mgr.SyncProjects(context.Background(), map[string][]ServerConfig{
		"kept": {{Name: "gmail"}},
		"":     {{Name: "ghost"}}, // empty projectID is ignored, not dialled
	})

	require.NotEmpty(t, mgr.Tools("kept"), "declared project keeps a catalog")
	require.Empty(t, mgr.Tools("removed"), "undeclared project must be dropped")
	require.Equal(t, 1, mgr.ProjectCount())
}

// TestSyncProjects_FailedDialKeepsPartialSuccess — a server that fails
// to dial is skipped (existing convention); the rest of the project
// still comes up.
func TestSyncProjects_FailedDialKeepsPartialSuccess(t *testing.T) {
	orig := connectFn
	defer func() { connectFn = orig }()
	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if cfg.Name == "broken" {
			return nil, context.DeadlineExceeded
		}
		return newFakeClient(cfg, []Tool{{Name: "ping"}}), nil
	}

	mgr := NewManager(zerolog.Nop())
	mgr.SyncProjects(context.Background(), map[string][]ServerConfig{
		"p1": {{Name: "broken"}, {Name: "gmail"}},
	})

	tools := mgr.Tools("p1")
	require.Len(t, tools, 1, "healthy server connects despite a sibling dial failure")
	require.Contains(t, tools[0].Function.Name, "gmail")
}
