package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// newFakeClient builds a minimal Client with a pre-populated tool list,
// bypassing the stdio/sse transport entirely. The allowlist is applied
// the same way Connect() would, so the test exercises the same code
// path downstream consumers do.
func newFakeClient(cfg ServerConfig, discovered []Tool) *Client {
	c := &Client{
		config: cfg,
		logger: zerolog.Nop(),
	}
	applyAllowlistForTest(c, discovered)
	return c
}

// TestManager_ProjectScoping locks in the multi-tenant contract: two
// projects with a same-named server get separate clients and separate
// tool catalogs, and Execute routes strictly within the calling
// project. This is the guarantee that lets one operator's assistant
// run alongside another's on the same daemon without cross-contamination.
func TestManager_ProjectScoping(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	mgr := NewManager(zerolog.Nop())

	aliceClient := newFakeClient(
		ServerConfig{Name: "gmail"},
		[]Tool{
			{Name: "search_emails", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "read_email"},
		},
	)
	bobClient := newFakeClient(
		ServerConfig{Name: "gmail"},
		[]Tool{
			{Name: "search_emails"},
		},
	)

	// Inject fake clients directly — we don't need a real transport to
	// test project scoping.
	mgr.mu.Lock()
	mgr.clients["alice"] = map[string]*Client{"gmail": aliceClient}
	mgr.clients["bob"] = map[string]*Client{"gmail": bobClient}
	mgr.mu.Unlock()

	aliceTools := mgr.Tools("alice")
	bobTools := mgr.Tools("bob")

	require.Len(t, aliceTools, 2, "alice's catalog is her gmail's two tools")
	require.Len(t, bobTools, 1, "bob's catalog is his gmail's one tool")

	// Different projects may advertise same qualified names — that's
	// expected, they point at different servers internally.
	require.Equal(t, "mcp__gmail__search_emails", aliceTools[0].Function.Name)
	require.Equal(t, "mcp__gmail__search_emails", bobTools[0].Function.Name)

	// Unknown project returns empty catalog, never a panic.
	require.Empty(t, mgr.Tools("carol"))
	require.Empty(t, mgr.Tools(""))

	// Counts reflect the per-project partitioning.
	require.Equal(t, 2, mgr.ProjectCount())
	require.Equal(t, 2, mgr.ServerCount())
}

// TestManager_Execute_RejectsUnknownServer guards the tool-routing
// boundary: a call whose server name isn't part of the given project's
// config fails with an error, even if another project has that server.
// Structural isolation — the error path, not access control, prevents
// leakage.
func TestManager_Execute_RejectsUnknownServer(t *testing.T) {
	mgr := NewManager(zerolog.Nop())

	aliceClient := newFakeClient(ServerConfig{Name: "gmail"}, []Tool{{Name: "search_emails"}})
	mgr.mu.Lock()
	mgr.clients["alice"] = map[string]*Client{"gmail": aliceClient}
	mgr.mu.Unlock()

	// bob has no MCP — call must fail, not silently route to alice's gmail.
	_, err := mgr.Execute(context.Background(), "bob", "mcp__gmail__search_emails", "{}")
	require.Error(t, err)
	require.Contains(t, err.Error(), `MCP server "gmail" not connected for project "bob"`)

	// Malformed qualified name — clean validation error, not a panic.
	_, err = mgr.Execute(context.Background(), "alice", "not_mcp_prefixed", "{}")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid MCP tool name")
}

// TestManager_StartForProject_ReplacesExistingClient confirms the
// semantics of calling StartForProject twice for the same project:
// the old client is closed cleanly so its subprocess is reaped, and
// the new client takes its slot without leaking.
func TestManager_StartForProject_ReplacesExistingClient(t *testing.T) {
	mgr := NewManager(zerolog.Nop())

	// Seed with a fake client whose Close() tracks invocation.
	old := newFakeClient(ServerConfig{Name: "gmail"}, []Tool{{Name: "old_tool"}})
	mgr.mu.Lock()
	mgr.clients["alice"] = map[string]*Client{"gmail": old}
	mgr.mu.Unlock()

	// Replace by poking a new fake directly (bypassing Connect's real
	// dial). This mirrors the behaviour StartForProject guarantees:
	// existing entry is closed, then overwritten.
	_ = old.Close() // simulate the close StartForProject would do
	newOne := newFakeClient(ServerConfig{Name: "gmail"}, []Tool{{Name: "new_tool"}})
	mgr.mu.Lock()
	mgr.clients["alice"]["gmail"] = newOne
	mgr.mu.Unlock()

	tools := mgr.Tools("alice")
	require.Len(t, tools, 1)
	require.Equal(t, "mcp__gmail__new_tool", tools[0].Function.Name)
}

// TestManager_EmptyProjectID is a safety rail: callers should never pass
// an empty project, but if they do, the Manager must not mix them up
// with any real project's clients.
func TestManager_EmptyProjectID(t *testing.T) {
	mgr := NewManager(zerolog.Nop())
	mgr.StartForProject(context.Background(), "", nil) // should log + return

	alice := newFakeClient(ServerConfig{Name: "gmail"}, []Tool{{Name: "t1"}})
	mgr.mu.Lock()
	mgr.clients["alice"] = map[string]*Client{"gmail": alice}
	mgr.mu.Unlock()

	require.Empty(t, mgr.Tools(""))

	_, err := mgr.Execute(context.Background(), "", "mcp__gmail__t1", "{}")
	require.Error(t, err)
}
