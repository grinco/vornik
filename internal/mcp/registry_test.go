package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubMCPServer returns an httptest server that speaks just enough
// of MCP-over-SSE-message endpoint shape for Connect → tools/list to
// succeed against it. `tools` becomes the advertised catalog. Counts
// every JSON-RPC envelope it answers so tests can verify the
// registry's TTL throttling behaviour.
func stubMCPServer(t *testing.T, tools []Tool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req jsonRPCRequest
		_ = json.Unmarshal(body, &req)

		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"protocolVersion":"2024-11-05"}`)
		case "tools/list":
			payload, _ := json.Marshal(toolsListResult{Tools: tools})
			resp.Result = payload
		default:
			resp.Result = json.RawMessage(`{}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestRegistry_RefreshAll_PopulatesReachableServers covers the happy
// path: every configured server connects, tools/list succeeds, the
// snapshot reports reachable=true with the advertised catalog.
func TestRegistry_RefreshAll_PopulatesReachableServers(t *testing.T) {
	scraper, _ := stubMCPServer(t, []Tool{
		{Name: "web_fetch", Description: "Fetch a URL"},
		{Name: "ical_events", Description: "Pull events from an iCal feed"},
	})
	gmail, _ := stubMCPServer(t, []Tool{
		{Name: "search_emails"},
	})

	reg := NewRegistry([]ServerConfig{
		{Name: "scraper", Transport: "sse", URL: scraper.URL},
		{Name: "gmail", Transport: "sse", URL: gmail.URL},
	}, 0, zerolog.Nop())

	require.Equal(t, 2, reg.ServerCount())

	reg.RefreshAll(context.Background())

	snap := reg.Snapshot(context.Background())
	require.Len(t, snap, 2)
	// Stable sort by name — assertions can reference fixed indices.
	require.Equal(t, "gmail", snap[0].Name)
	require.Equal(t, "scraper", snap[1].Name)

	assert.True(t, snap[0].Reachable)
	assert.Empty(t, snap[0].Error)
	assert.Len(t, snap[0].Tools, 1)

	assert.True(t, snap[1].Reachable)
	assert.Len(t, snap[1].Tools, 2)
	assert.Equal(t, "Fetch a URL", snap[1].Tools[0].Description)
}

// TestRegistry_RefreshAll_RecordsUnreachable verifies an
// unreachable server stays in the snapshot with reachable=false
// + an error string — it isn't silently dropped (operators need
// to see broken wiring).
func TestRegistry_RefreshAll_RecordsUnreachable(t *testing.T) {
	good, _ := stubMCPServer(t, []Tool{{Name: "ok"}})

	reg := NewRegistry([]ServerConfig{
		{Name: "good", Transport: "sse", URL: good.URL},
		{Name: "broken", Transport: "sse", URL: "http://127.0.0.1:1"}, // refused
	}, 0, zerolog.Nop())

	reg.RefreshAll(context.Background())

	snap := reg.Snapshot(context.Background())
	require.Len(t, snap, 2)

	// Order: alphabetical → "broken" first.
	require.Equal(t, "broken", snap[0].Name)
	assert.False(t, snap[0].Reachable)
	assert.NotEmpty(t, snap[0].Error, "operators must see why broken server failed")
	assert.Nil(t, snap[0].Tools)

	require.Equal(t, "good", snap[1].Name)
	assert.True(t, snap[1].Reachable)
}

// TestRegistry_Snapshot_PreSeededBeforeRefresh confirms that
// Snapshot returns a row for every configured server even before
// RefreshAll has been invoked — important because the API
// handler MUST NOT block on the first connect. The pre-seed row
// is reachable=false with the "not yet refreshed" sentinel so
// the UI doesn't display a stale-success state.
func TestRegistry_Snapshot_PreSeededBeforeRefresh(t *testing.T) {
	reg := NewRegistry([]ServerConfig{
		{Name: "scraper", Transport: "sse", URL: "http://example.invalid"},
	}, 0, zerolog.Nop())

	snap := reg.Snapshot(context.Background())
	require.Len(t, snap, 1)
	require.Equal(t, "scraper", snap[0].Name)
	require.False(t, snap[0].Reachable)
	require.Contains(t, snap[0].Error, "not yet refreshed")
}

// TestRegistry_Snapshot_DoesNotBlockOnStale stages a stub server
// behind a configurable delay and asserts Snapshot returns
// immediately even when a refresh is overdue. The stale entry is
// returned as-is; the spawned goroutine handles the slow connect.
//
// This is the key API contract the rollout brief calls out: the
// HTTP endpoint MUST NOT wait on slow MCP servers.
func TestRegistry_Snapshot_DoesNotBlockOnStale(t *testing.T) {
	// Slow server: 500ms before responding. If Snapshot blocked, the
	// test would exceed 100ms.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		body, _ := io.ReadAll(r.Body)
		var req jsonRPCRequest
		_ = json.Unmarshal(body, &req)
		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"protocolVersion":"2024-11-05"}`)
		case "tools/list":
			payload, _ := json.Marshal(toolsListResult{Tools: []Tool{{Name: "slow_tool"}}})
			resp.Result = payload
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer slow.Close()

	// TTL=1ns means every Snapshot call considers the entry stale.
	reg := NewRegistry([]ServerConfig{
		{Name: "slow", Transport: "sse", URL: slow.URL},
	}, time.Nanosecond, zerolog.Nop())

	start := time.Now()
	snap := reg.Snapshot(context.Background())
	elapsed := time.Since(start)

	require.Len(t, snap, 1)
	require.Less(t, elapsed, 100*time.Millisecond, "Snapshot must not block on slow refresh")
}

// TestRegistry_AsyncRefresh_DedupesInFlight stages a server with an
// observable per-call hit counter; calling Snapshot N times back-to-
// back when an async refresh is already in flight should kick off
// at most one connect, not N. The dedupe protects a wedged server
// from being hammered.
func TestRegistry_AsyncRefresh_DedupesInFlight(t *testing.T) {
	// Block the server until we let it complete. Counts every
	// connect attempt so we can assert the dedupe behavior.
	var hits atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		body, _ := io.ReadAll(r.Body)
		var req jsonRPCRequest
		_ = json.Unmarshal(body, &req)
		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"protocolVersion":"2024-11-05"}`)
		case "tools/list":
			payload, _ := json.Marshal(toolsListResult{Tools: []Tool{}})
			resp.Result = payload
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	reg := NewRegistry([]ServerConfig{
		{Name: "wedged", Transport: "sse", URL: srv.URL},
	}, time.Nanosecond, zerolog.Nop())

	// Fire 10 snapshots in parallel — every one sees the stale
	// pre-seed row and would, without dedupe, spawn a refresh.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.Snapshot(context.Background())
		}()
	}
	wg.Wait()

	// Give the spawned goroutines a moment to enter the refresh map.
	time.Sleep(50 * time.Millisecond)
	close(release)
	// Allow the in-flight refresh to drain.
	time.Sleep(100 * time.Millisecond)

	// initialize + tools/list = 2 hits per successful refresh. With
	// dedupe we expect exactly one refresh; without it we'd see
	// many more.
	got := hits.Load()
	require.LessOrEqual(t, got, int64(4), "async refresh must dedupe; got %d server hits", got)
}

// TestRegistry_EmptyServers_ReturnsEmptySnapshot is the no-config
// case — operator hasn't declared a daemon-level mcp block. The
// surface must still be safe to call; just returns [].
func TestRegistry_EmptyServers_ReturnsEmptySnapshot(t *testing.T) {
	reg := NewRegistry(nil, 0, zerolog.Nop())
	require.Equal(t, 0, reg.ServerCount())
	require.Empty(t, reg.Snapshot(context.Background()))
}

// TestShortenError clips outsize errors at 256 bytes. Defends the
// cached snapshot from a hostile MCP server replying with a gigabyte
// of stack trace.
func TestShortenError(t *testing.T) {
	require.Equal(t, "", shortenError(nil))

	long := make([]byte, 2048)
	for i := range long {
		long[i] = 'x'
	}
	out := shortenError(stringError(long))
	// "…" is a 3-byte UTF-8 rune, so the upper bound is 256 + 3 = 259.
	require.LessOrEqual(t, len(out), 259)
}

type stringError []byte

func (s stringError) Error() string { return string(s) }
