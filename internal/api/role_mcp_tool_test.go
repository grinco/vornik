package api

import "testing"

// TestMCPRoleToolAllowed pins the CLOSED-WORLD semantics of the server-side
// role MCP-tool gate (handlers.go roleAllowsMCPTool / mcpRoleToolAllowed).
//
// A role with a non-empty allowlist may invoke ONLY what it lists. MCP tools
// must be granted explicitly — by name, bare segment, or an mcp__* /
// mcp__server__* wildcard the operator writes deliberately. There is no
// fail-open by omission (a deliberately-narrow built-in-only role must NOT
// reach project MCP tools like broker place_order).
//
// Regression context (2026-06-20): the janka `researcher` listed only built-in
// tools, so it could not call mcp__scraper__web_fetch and every portal scan got
// a daemon-level FORBIDDEN → stale RAG. The fix is to GRANT the tool in the
// role config (deployed + distributed presets), which the cases below pin.
func TestMCPRoleToolAllowed(t *testing.T) {
	builtinOnly := []string{"file_read", "file_write", "run_shell", "grep", "memory_search", "current_time"}
	// The FIX applied to research roles: built-ins + the explicit scraper grant.
	researcherFixed := append(append([]string{}, builtinOnly...), "mcp__scraper__web_fetch", "mcp__scraper__ical_events")
	// Trading role: explicit, least-privilege broker tools (real ibkr pattern).
	tradingRO := []string{"current_time", "memory_search", "mcp__broker__get_quote", "mcp__broker__get_positions"}
	serverWildcard := []string{"current_time", "mcp__scraper__*"}
	allMCPWildcard := []string{"file_read", "mcp__*"}

	cases := []struct {
		name      string
		allowed   []string
		tool      string
		wantAllow bool
	}{
		// Closed-world: built-in-only role does NOT get MCP tools (no fail-open).
		{"builtin-only role DENIED scraper web_fetch", builtinOnly, "mcp__scraper__web_fetch", false},
		// The config fix grants it explicitly.
		{"researcher with grant allows web_fetch", researcherFixed, "mcp__scraper__web_fetch", true},
		{"researcher with grant allows ical_events", researcherFixed, "mcp__scraper__ical_events", true},
		// B2 preserved: explicit MCP allowlist denies unlisted tools.
		{"trading role denies unlisted place_order", tradingRO, "mcp__broker__place_order", false},
		{"trading role allows listed get_quote", tradingRO, "mcp__broker__get_quote", true},
		// Server wildcard grants that server, nothing else.
		{"server wildcard allows scraper tool", serverWildcard, "mcp__scraper__web_fetch", true},
		{"server wildcard denies other server", serverWildcard, "mcp__broker__place_order", false},
		// Global mcp wildcard defers all MCP to the project layer.
		{"mcp__* wildcard allows any mcp tool", allMCPWildcard, "mcp__broker__place_order", true},
		// Built-in gating unchanged.
		{"builtin not in list is denied", builtinOnly, "run_shell_unlisted", false},
		{"builtin in list is allowed", builtinOnly, "grep", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpRoleToolAllowed(tc.allowed, tc.tool); got != tc.wantAllow {
				t.Fatalf("mcpRoleToolAllowed(%v, %q) = %v, want %v", tc.allowed, tc.tool, got, tc.wantAllow)
			}
		})
	}
}
