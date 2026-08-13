package api

import (
	"testing"

	"vornik.io/vornik/internal/chat"
)

// Role allowlists must shrink the ADVERTISED tool list, not only refuse calls.
//
// Measured 2026-08-12: `easeit-companion` advertises 58 MCP tools — 47,750 bytes
// of JSON schema, ~11,937 tokens — to EVERY role on EVERY iteration of the tool
// loop, including roles like `vision` and `writer`. That is the largest movable
// component of an agent step's prompt (registry design §10).
//
// Before this, `allowedTools` existed in three places and every one was
// invoke-time: it reached the agent as `allowedTools` in the input contract, §09
// required the runtime to refuse unlisted tools, and the daemon re-enforced it on
// /mcp/call (Finding B2). Tokens are not spent when a tool is invoked; they are
// spent when it is advertised, and nothing filtered the advertised list by role.
//
// WHY NOT mcp.json. The design first named buildMCPConfig as the hook. That file
// is written only for the local-subprocess fallback (`shouldWriteMCPConfig`
// returns false whenever VORNIK_API_URL is set — the normal daemon-proxy mode),
// so filtering it would have saved nothing in the deployment that motivated the
// change. GET /projects/{id}/mcp/tools is what the agent's mcp-bridge reads.

func tools(names ...string) []chat.Tool {
	out := make([]chat.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, chat.Tool{Type: "function", Function: chat.ToolFunction{Name: n}})
	}
	return out
}

func toolNames(in []chat.Tool) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, t.Function.Name)
	}
	return out
}

func TestAdvertisedTools_FiltersToTheRoleAllowlist(t *testing.T) {
	all := tools("mcp__broker__quote", "mcp__broker__place_order", "mcp__scraper__web_fetch")

	got := toolNames(advertisedTools(all, []string{"mcp__broker__quote"}))

	if len(got) != 1 || got[0] != "mcp__broker__quote" {
		t.Errorf("advertised %v, want only the allowlisted tool — every extra schema is "+
			"re-sent on every iteration of the tool loop", got)
	}
}

// TestAdvertisedTools_EmptyAllowlistIsPassthrough pins the fail-open rule. Every
// role in the deployment currently declares no allowlist, so narrowing by default
// would silently break running projects.
func TestAdvertisedTools_EmptyAllowlistIsPassthrough(t *testing.T) {
	all := tools("a", "b", "c")

	if got := advertisedTools(all, nil); len(got) != 3 {
		t.Errorf("advertised %v for a role declaring no allowlist; fail-open is what lets "+
			"this ship without reconfiguring every swarm first", toolNames(got))
	}
	if got := advertisedTools(all, []string{}); len(got) != 3 {
		t.Error("an empty (non-nil) allowlist must also pass through")
	}
}

// TestAdvertisedTools_HonoursWildcards keeps the advertised set consistent with
// the operator's written intent — a role granted mcp__scraper__* should SEE the
// scraper tools, not merely be allowed to call ones it was never shown.
func TestAdvertisedTools_HonoursWildcards(t *testing.T) {
	all := tools("mcp__scraper__web_fetch", "mcp__scraper__ical_events", "mcp__broker__place_order")

	got := toolNames(advertisedTools(all, []string{"mcp__scraper__*"}))

	if len(got) != 2 {
		t.Errorf("advertised %v, want both scraper tools and not the broker one", got)
	}
}

// TestAdvertisedTools_NeverExceedsInvokePermission is the load-bearing law.
//
// Advertisement and invocation must be decided by the SAME policy. If they could
// disagree, one direction advertises a tool that /mcp/call then refuses (the agent
// burns an iteration on a guaranteed 403), and the other advertises less than it
// permits (harmless but confusing). Deriving both from mcpRoleToolAllowed makes
// the first impossible by construction; this test pins it against future drift.
func TestAdvertisedTools_NeverExceedsInvokePermission(t *testing.T) {
	all := tools(
		"file_read", "run_shell", "grep",
		"mcp__broker__quote", "mcp__broker__place_order",
		"mcp__scraper__web_fetch", "mcp__vornik__recall",
	)
	allowlists := [][]string{
		{"file_read"},
		{"mcp__broker__quote"},
		{"mcp__scraper__*"},
		{"mcp__*"},
		{"file_read", "grep", "mcp__vornik__recall"},
	}
	for _, allowed := range allowlists {
		for _, adv := range advertisedTools(all, allowed) {
			name := adv.Function.Name
			if !mcpRoleToolAllowed(allowed, name) {
				t.Errorf("allowlist %v advertises %q but the invoke gate refuses it — an "+
					"agent would spend an iteration to earn a guaranteed 403", allowed, name)
			}
		}
	}
}

// TestAdvertisedTools_NilToolsIsSafe: a project with no MCP servers must not
// panic through the new filter.
func TestAdvertisedTools_NilToolsIsSafe(t *testing.T) {
	if got := advertisedTools(nil, []string{"anything"}); len(got) != 0 {
		t.Errorf("expected no tools, got %v", toolNames(got))
	}
}
