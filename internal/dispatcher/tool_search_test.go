// Tests for the 2026.7.0 F12 tool deferred-loading + tool_search.
// Anchors:
//
//   - The threshold gate (<=N MCP tools → no deferral)
//   - tool_search appears in the visible set when deferral is on
//   - Search match expands names into the per-session set
//   - Subsequent allTools calls in the same session see the
//     expanded names
//   - chatID=0 falls back to "everything visible" so sub-agents
//     and per-task code paths keep their legacy behaviour
//
// Scoring is exercised separately via toolHit / scoreTools so a
// future rank tweak can move in a focused test.

package dispatcher

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// makeMCPTool is a tiny constructor that keeps test setup
// short. Name + description is all the search ranker reads.
func makeMCPTool(name, desc string) chat.Tool {
	return chat.Tool{
		Type: "function",
		Function: chat.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

// TestEffectiveDeferralThreshold_DegradedTierForcesLow — when the
// chat session's context budget is DEGRADING or worse, the threshold
// gets clamped to DegradedDeferredToolThreshold so deferral kicks in
// regardless of the configured catalog size. Operators don't have to
// retune the MCP threshold to handle context exhaustion — it
// auto-degrades from the same signal that gates the tier badge.
func TestEffectiveDeferralThreshold_DegradedTierForcesLow(t *testing.T) {
	cases := []struct {
		tier chat.ContextTier
		want int
	}{
		{chat.TierPeak, 50},     // not degraded — caller's threshold honored
		{chat.TierGood, 50},     // not degraded — caller's threshold honored
		{chat.TierDegrading, 1}, // degraded — clamped to 1
		{chat.TierPoor, 1},      // degraded — clamped to 1
	}
	for _, c := range cases {
		if got := effectiveDeferralThreshold(50, c.tier); got != c.want {
			t.Errorf("tier=%s threshold=50 → got %d, want %d", c.tier, got, c.want)
		}
	}
}

// TestApplyDeferredLoading_DegradedTierShrinksVisibleSetEvenBelowThreshold
// — the integration: a small MCP catalog (well below the default
// threshold) still gets deferred when the tier is degraded. This is
// the property that prevents context exhaustion from cascading into a
// runaway tool-call loop on overlong sessions.
func TestApplyDeferredLoading_DegradedTierShrinksVisibleSetEvenBelowThreshold(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	// 5 MCP tools — well below the default threshold of 20.
	mcp := []chat.Tool{
		makeMCPTool("mcp__a__one", "x"),
		makeMCPTool("mcp__a__two", "x"),
		makeMCPTool("mcp__a__three", "x"),
		makeMCPTool("mcp__a__four", "x"),
		makeMCPTool("mcp__a__five", "x"),
	}
	store := newExpandedToolStore()

	// PEAK tier: threshold honored → everything visible.
	peakThreshold := effectiveDeferralThreshold(DefaultDeferredToolThreshold, chat.TierPeak)
	peakResult := applyDeferredLoading(builtin, mcp, store, "99", peakThreshold, nil)
	if !containsToolByName(peakResult, "mcp__a__one") {
		t.Errorf("PEAK tier should leave 5-tool catalog fully visible: %v", peakResult)
	}

	// DEGRADING tier: threshold clamped to 1 → deferral kicks in.
	degradedThreshold := effectiveDeferralThreshold(DefaultDeferredToolThreshold, chat.TierDegrading)
	degradedResult := applyDeferredLoading(builtin, mcp, store, "99", degradedThreshold, nil)
	if !containsToolByName(degradedResult, ToolSearchName) {
		t.Errorf("DEGRADING tier must inject tool_search; got %v", degradedResult)
	}
	for _, m := range mcp {
		if containsToolByName(degradedResult, m.Function.Name) {
			t.Errorf("DEGRADING tier must hide MCP tool %q (no expansion yet)", m.Function.Name)
		}
	}
}

// TestApplyDeferredLoading_BelowThresholdReturnsEverything
// pins the contract: below the threshold there's no schema
// churn, the model sees every MCP tool as before.
func TestApplyDeferredLoading_BelowThresholdReturnsEverything(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	mcp := []chat.Tool{makeMCPTool("mcp__a__one", "x"), makeMCPTool("mcp__a__two", "y")}
	store := newExpandedToolStore()
	got := applyDeferredLoading(builtin, mcp, store, "99", 20, nil)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (builtin + every MCP tool)", len(got))
	}
	for _, want := range []string{"list_projects", "mcp__a__one", "mcp__a__two"} {
		if !containsToolByName(got, want) {
			t.Errorf("expected tool %q in output", want)
		}
	}
}

// TestApplyDeferredLoading_AboveThresholdHidesMCPAndSurfacesSearch
// — the canonical deferral case. Operator wires more than the
// threshold's worth of MCP tools; the visible set shrinks to
// the built-ins plus tool_search.
func TestApplyDeferredLoading_AboveThresholdHidesMCPAndSurfacesSearch(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	mcp := make([]chat.Tool, 25)
	for i := range mcp {
		mcp[i] = makeMCPTool("mcp__a__"+string(rune('a'+i%26)), "x")
	}
	store := newExpandedToolStore()
	got := applyDeferredLoading(builtin, mcp, store, "99", 20, nil)
	if !containsToolByName(got, "list_projects") {
		t.Error("built-in tools must remain visible above threshold")
	}
	if !containsToolByName(got, ToolSearchName) {
		t.Error("tool_search must be injected when deferral kicks in")
	}
	// MCP tools should be hidden.
	for _, m := range mcp {
		if containsToolByName(got, m.Function.Name) {
			t.Errorf("MCP tool %q must be hidden until expanded; deferred-loading regressed", m.Function.Name)
		}
	}
}

// TestApplyDeferredLoading_ExpandedToolsSurface — after the
// session has expanded a couple of MCP tools (via tool_search),
// they show up in subsequent allTools calls alongside the
// built-ins and tool_search.
func TestApplyDeferredLoading_ExpandedToolsSurface(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	mcp := make([]chat.Tool, 25)
	for i := range mcp {
		mcp[i] = makeMCPTool("mcp__a__"+string(rune('a'+i%26)), "x")
	}
	store := newExpandedToolStore()
	store.expand("42", []string{"mcp__a__a", "mcp__a__c"})

	got := applyDeferredLoading(builtin, mcp, store, "42", 20, nil)
	if !containsToolByName(got, "mcp__a__a") || !containsToolByName(got, "mcp__a__c") {
		t.Error("expanded MCP tools must surface in the visible set for subsequent turns")
	}
	if containsToolByName(got, "mcp__a__b") {
		t.Error("non-expanded MCP tool must stay hidden")
	}
}

// TestApplyDeferredLoading_ChatIDZeroSkipsDeferral — sub-agent
// / per-task code paths that don't carry a chat session pass
// chatID=0; deferral has no session to anchor to, so it falls
// back to legacy "everything visible". Important so the per-task
// agent isn't accidentally starved of tools it should see.
func TestApplyDeferredLoading_EmptySessionKeySkipsDeferral(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	mcp := make([]chat.Tool, 25)
	for i := range mcp {
		mcp[i] = makeMCPTool("mcp__a__"+string(rune('a'+i%26)), "x")
	}
	store := newExpandedToolStore()
	// Even above threshold, an EMPTY session key disables deferral. That is
	// the sub-agent / per-task path, which has no session to anchor
	// expansions to. Note "0" is NOT empty — a Telegram chat id of 0 never
	// reaches here, and a channel session key is never empty.
	got := applyDeferredLoading(builtin, mcp, store, "", 20, nil)
	for _, m := range mcp {
		if !containsToolByName(got, m.Function.Name) {
			t.Errorf("an empty session key must see every MCP tool; %q missing", m.Function.Name)
			break
		}
	}
}

// TestScoreTools_NameMatchWinsOverDescription pins the rank
// shape: name hits weigh more than description hits (3:1).
// Tool A scores in BOTH name and description; tool B scores
// only in description; tool C doesn't match. Top hit must be
// A, B must rank below A, and C must be dropped entirely.
func TestScoreTools_NameMatchWinsOverDescription(t *testing.T) {
	cat := []chat.Tool{
		// name has "send" + "email"; desc has "send" + "email"
		// → score = (3+3) name + (1+1) desc = 8
		makeMCPTool("mcp__gmail__send_email", "Send email via Gmail"),
		// name has nothing; desc has "send" + "email" → score = 2
		makeMCPTool("mcp__notify__telegram", "Notify someone via Telegram. Used to send email-style alerts."),
		// no overlap → dropped
		makeMCPTool("mcp__calendar__list_events", "List calendar events."),
	}
	hits := scoreTools(cat, "send email")
	if len(hits) != 2 {
		t.Fatalf("expected exactly 2 hits (calendar must be dropped), got %d: %+v", len(hits), hits)
	}
	if hits[0].tool.Function.Name != "mcp__gmail__send_email" {
		t.Errorf("top hit = %q, want mcp__gmail__send_email", hits[0].tool.Function.Name)
	}
	if hits[1].tool.Function.Name != "mcp__notify__telegram" {
		t.Errorf("second hit = %q, want mcp__notify__telegram (desc-only match)", hits[1].tool.Function.Name)
	}
	if hits[0].score <= hits[1].score {
		t.Errorf("name+desc hit (%v) must outscore desc-only hit (%v) — 3:1 weight regressed", hits[0].score, hits[1].score)
	}
}

// TestScoreTools_ZeroScoreToolsDropped — a search that has no
// overlap with a given tool must NOT return that tool. Pins
// the "no false-positive surface" property.
func TestScoreTools_ZeroScoreToolsDropped(t *testing.T) {
	cat := []chat.Tool{
		makeMCPTool("mcp__weather__forecast", "Weather forecast"),
		makeMCPTool("mcp__gmail__send_email", "Send email via Gmail"),
	}
	hits := scoreTools(cat, "gmail")
	if len(hits) != 1 || hits[0].tool.Function.Name != "mcp__gmail__send_email" {
		t.Errorf("zero-score tools must be excluded; got %+v", hits)
	}
}

// TestScoreTools_EmptyQueryReturnsNil — defensive: an empty
// query string returns nil rather than dumping every tool with
// score=0 (which the model would interpret as "everything
// matches").
func TestScoreTools_EmptyQueryReturnsNil(t *testing.T) {
	cat := []chat.Tool{makeMCPTool("x", "y")}
	if got := scoreTools(cat, "   "); got != nil {
		t.Errorf("empty query must return nil, got %+v", got)
	}
}

// TestExpandedToolStore_PersistsAcrossCalls — the store must
// keep state across calls so tool_search's expansion sticks
// into the next allTools call.
func TestExpandedToolStore_PersistsAcrossCalls(t *testing.T) {
	s := newExpandedToolStore()
	s.expand("42", []string{"a", "b"})
	s.expand("42", []string{"c"})
	for _, want := range []string{"a", "b", "c"} {
		if !s.contains("42", want) {
			t.Errorf("expand-then-contains lost %q", want)
		}
	}
	if s.contains("99", "a") {
		t.Error("expansion must be scoped to its chatID")
	}
}

// TestExpandedToolStore_NilSafeAndEmptySessionKey — nil receiver + an empty
// session key are common in the test helpers; the store must degrade cleanly
// rather than panic.
//
// The no-session sentinel is the EMPTY STRING, not "0". It was the integer 0
// while this keyed on Telegram chat ids; "0" is now an ordinary key (and
// unreachable in practice — deferralSessionKey only emits a numeric form for a
// non-zero ChatID).
func TestExpandedToolStore_NilSafeAndEmptySessionKey(t *testing.T) {
	var nilStore *expandedToolStore
	nilStore.expand("1", []string{"a"})
	if nilStore.contains("1", "a") {
		t.Error("nil store must not retain anything")
	}
	s := newExpandedToolStore()
	s.expand("", []string{"a"})
	if s.contains("", "a") {
		t.Error("an empty session key must be a no-op so session-less paths can pass it")
	}
}

// TestExpandedToolStore_ResetWipesSession — /new should be able
// to wipe the per-session set. The wiring lives in the bot;
// here we just lock in the primitive.
func TestExpandedToolStore_ResetWipesSession(t *testing.T) {
	s := newExpandedToolStore()
	s.expand("42", []string{"a"})
	s.reset("42")
	if s.contains("42", "a") {
		t.Error("reset must drop the session's expanded set")
	}
}

// TestExpandedToolStore_ResetEdgeCases — chatID=0 and nil receiver
// are silent no-ops; non-existing chatID is a silent no-op too.
func TestExpandedToolStore_ResetEdgeCases(t *testing.T) {
	t.Run("nil receiver no-op", func(t *testing.T) {
		var s *expandedToolStore
		// Just verify it doesn't panic — there's nothing to assert.
		s.reset("42")
	})
	t.Run("empty session key no-op", func(t *testing.T) {
		s := newExpandedToolStore()
		s.expand("42", []string{"a"})
		s.reset("") // the no-session sentinel: nothing to wipe
		if !s.contains("42", "a") {
			t.Error("reset(\"\") must not affect real sessions")
		}
	})
	t.Run("non-existing chatID is a silent no-op", func(t *testing.T) {
		s := newExpandedToolStore()
		s.reset("9999") // not registered
		// No state to check; just ensure no panic.
	})
}

// TestToolSearch_ExpandsAndReturnsMatches is the end-to-end
// happy path: wire a stub MCP manager with a real catalog,
// run toolSearch, assert the matched names show up in the
// expanded set + the response text lists them.
func TestToolSearch_ExpandsAndReturnsMatches(t *testing.T) {
	te := newExecutor(withMCPCatalog(func(string) []chat.Tool {
		return []chat.Tool{
			makeMCPTool("mcp__gmail__send_email", "Send an email via Gmail."),
			makeMCPTool("mcp__gmail__list_inbox", "List recent inbox messages."),
			makeMCPTool("mcp__weather__forecast", "Weather forecast for the day."),
		}
	}))
	te.expanded = newExpandedToolStore()
	res := te.toolSearch(`{"query":"gmail send"}`, "snake", "42")
	if !strings.Contains(res.Content, "mcp__gmail__send_email") {
		t.Errorf("response must list the top match, got %q", res.Content)
	}
	if !te.expanded.contains("42", "mcp__gmail__send_email") {
		t.Error("tool_search must expand top hit into the per-session set")
	}
}

// TestToolSearch_NoMatchExplains — when nothing scores, the
// model receives a clear "no matches" message rather than an
// empty string (which it might interpret as a tool failure).
func TestToolSearch_NoMatchExplains(t *testing.T) {
	te := newExecutor(withMCPCatalog(func(string) []chat.Tool {
		return []chat.Tool{makeMCPTool("mcp__weather__forecast", "Weather forecast.")}
	}))
	te.expanded = newExpandedToolStore()
	res := te.toolSearch(`{"query":"calendar"}`, "snake", "42")
	if !strings.Contains(res.Content, "No tools matched") {
		t.Errorf("expected friendly no-match copy, got %q", res.Content)
	}
}

// TestToolSearch_MissingQueryRejected — defensive arg parsing.
func TestToolSearch_MissingQueryRejected(t *testing.T) {
	te := newExecutor()
	res := te.toolSearch(`{}`, "snake", "42")
	if !strings.Contains(res.Content, "query is required") {
		t.Errorf("got %q", res.Content)
	}
}

// TestToolSearch_NoMCPManagerSurfacesFriendlyMessage — when
// the project has no MCP wired the tool returns a useful
// message rather than dispatching against nil.
func TestToolSearch_NoMCPManagerSurfacesFriendlyMessage(t *testing.T) {
	te := newExecutor() // no MCP catalog
	res := te.toolSearch(`{"query":"x"}`, "snake", "42")
	if !strings.Contains(res.Content, "not available") && !strings.Contains(res.Content, "No MCP tools") {
		t.Errorf("expected friendly empty-state copy, got %q", res.Content)
	}
}

// containsToolByName / newExecutor / withMCPCatalog are helper
// shims for these tests. Defined separately so the test
// file stays self-contained.
func containsToolByName(tools []chat.Tool, name string) bool {
	for _, t := range tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

// stubMCPCatalog implements MCPExecutor for the tests that
// need a programmable Tools() return.
type stubMCPCatalog struct {
	tools func(projectID string) []chat.Tool
}

func (s *stubMCPCatalog) Tools(projectID string) []chat.Tool {
	if s.tools == nil {
		return nil
	}
	return s.tools(projectID)
}
func (s *stubMCPCatalog) Execute(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

func withMCPCatalog(fn func(projectID string) []chat.Tool) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.mcpManager = &stubMCPCatalog{tools: fn} }
}

// TestDeferralSessionKey_ChannelSessionsGetDeferral is the regression test for
// the 2026-08-05 report: "the dispatcher keeps insisting it doesn't see the
// atlassian mcp or have the tooling for mcp search".
//
// Request.ChatID is documented as the platform's NUMERIC chat identifier, used
// by tools that send files back, with "leave 0 for channels that lack one" —
// and Slack, email and GitHub all leave it 0. Deferred loading then read that
// same field as "is there a session here at all?", so every one of those
// channels was misclassified as a sub-agent invocation. Two consequences, both
// invisible:
//
//  1. tool_search was never advertised, because it is only added on the
//     above-threshold path. The model was told to search and had no searcher.
//  2. Deferral never engaged, so the FULL catalog shipped every turn — 33 MCP
//     tool descriptions on the reporting project, which is the token cost
//     deferral exists to avoid.
//
// A channel session id is a perfectly good session identity; it just isn't a
// number. The key is now derived, so Telegram keeps working off ChatID and
// genuinely session-less callers still opt out.
func TestDeferralSessionKey_ChannelSessionsGetDeferral(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{{
		name: "slack channel session",
		req:  Request{OriginatingChannel: "slack", OriginatingSessionID: "T123/C_general#1785367141.211839"},
		want: "slack:T123/C_general#1785367141.211839",
	}, {
		name: "email thread session",
		req:  Request{OriginatingChannel: "email", OriginatingSessionID: "<root@example.com>"},
		want: "email:<root@example.com>",
	}, {
		name: "telegram falls back to the numeric chat id",
		req:  Request{ChatID: 559741208},
		want: "559741208",
	}, {
		name: "channel session wins over a numeric id",
		req:  Request{OriginatingChannel: "slack", OriginatingSessionID: "S1", ChatID: 42},
		want: "slack:S1",
	}, {
		name: "sub-agent: no session at all",
		req:  Request{},
		want: "",
	}, {
		name: "whitespace-only session id is not a session",
		req:  Request{OriginatingChannel: "slack", OriginatingSessionID: "   "},
		want: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deferralSessionKey(tc.req); got != tc.want {
				t.Errorf("deferralSessionKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyDeferredLoading_ChannelSessionSurfacesSearch — the consequence that
// actually bit the operator: with a channel session key, an over-threshold
// catalog must hide the MCP tools AND advertise tool_search. Before the fix a
// Slack session produced neither.
func TestApplyDeferredLoading_ChannelSessionSurfacesSearch(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	mcp := make([]chat.Tool, 33) // easeit-companion's real count
	for i := range mcp {
		mcp[i] = makeMCPTool("mcp__svc__t"+string(rune('a'+i%26)), "d")
	}
	key := deferralSessionKey(Request{OriginatingChannel: "slack", OriginatingSessionID: "T1/C1#1"})
	got := applyDeferredLoading(builtin, mcp, newExpandedToolStore(), key, 20, nil)

	if !containsToolByName(got, ToolSearchName) {
		t.Fatalf("a Slack session must be offered %s — without it the model is told to search and has no searcher", ToolSearchName)
	}
	if len(got) != len(builtin)+1 {
		t.Errorf("visible=%d, want %d (builtin + tool_search); MCP tools must be deferred", len(got), len(builtin)+1)
	}
}

// atlassianCatalog reproduces the real 16-tool atlassian palette from the
// 2026-08-05 incident, descriptions included verbatim where they matter. The
// point of the fixture is what ISN'T there: no tool named or described with
// "jira" returns a cloudId, and every Jira tool requires one.
func atlassianCatalog() []chat.Tool {
	return []chat.Tool{
		makeMCPTool("mcp__atlassian__atlassianUserInfo", "Get current user info"),
		makeMCPTool("mcp__atlassian__getAccessibleAtlassianResources",
			"Get cloudId to make tool calls. When a link is provided (e.g. https://site.atlassian.net/*), "+
				"try passing the site hostname as cloudId to other tools first; if that fails, use this tool "+
				"to list accessible resources."),
		makeMCPTool("mcp__atlassian__getCompassComponent", "Get a Compass component by ID"),
		makeMCPTool("mcp__atlassian__getCompassComponents", "Get a list of Compass components"),
		makeMCPTool("mcp__atlassian__getJiraIssue", "Get issue details"),
		makeMCPTool("mcp__atlassian__getJiraIssueRemoteIssueLinks", "Get remote links"),
		makeMCPTool("mcp__atlassian__getJiraIssueTypeMetaWithFields", "Get field metadata"),
		makeMCPTool("mcp__atlassian__getJiraProjectIssueTypesMetadata", "Get issue types"),
		makeMCPTool("mcp__atlassian__getTransitionsForJiraIssue", "Get transitions"),
		makeMCPTool("mcp__atlassian__getVisibleJiraProjects", "Get projects"),
		makeMCPTool("mcp__atlassian__lookupJiraAccountId", "Lookup user IDs"),
		makeMCPTool("mcp__atlassian__searchJiraIssuesUsingJql",
			"Search issues with JQL, total counts only when explicitly requested."),
		makeMCPTool("mcp__atlassian__getConfluencePage", "Get a Confluence page"),
		makeMCPTool("mcp__atlassian__searchConfluence", "Search Confluence"),
		makeMCPTool("mcp__atlassian__createJiraIssue", "Create an issue"),
		makeMCPTool("mcp__atlassian__addCommentToJiraIssue", "Add a comment"),
	}
}

// TestToolSearch_DomainQueryStillUnlocksTheServersBootstrapTool is the
// regression test for the 2026-08-05 channel incident.
//
// The operator asked for unassigned in-progress Jira issues. The model searched
// "Jira", got 8 Jira-NAMED tools, and every call then failed with "cloudId
// Required". The only tool that yields a cloudId is
// getAccessibleAtlassianResources, whose name and description contain no form of
// "jira" — so no amount of retrying that query could ever reach it. The model
// reasoned correctly from what it could see and concluded the tool did not
// exist, then asked the operator for a cloudId the operator had no way to know.
//
// The same model in a DM minutes earlier searched "atlassian", which ranked the
// bootstrap tool inside the cut, and succeeded. That difference was luck, and it
// is what made this look like a DM-vs-channel bug.
func TestToolSearch_DomainQueryStillUnlocksTheServersBootstrapTool(t *testing.T) {
	te := newExecutor(withMCPCatalog(func(string) []chat.Tool { return atlassianCatalog() }))
	te.expanded = newExpandedToolStore()

	res := te.toolSearch(`{"query":"Jira"}`, "easeit-companion", "slack:T0/C0#main")

	const bootstrap = "mcp__atlassian__getAccessibleAtlassianResources"
	if !te.expanded.contains("slack:T0/C0#main", bootstrap) {
		t.Errorf("a Jira query must leave the cloudId tool CALLABLE — every other "+
			"atlassian tool requires an argument only it returns:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, bootstrap) {
		t.Errorf("the cloudId tool must also be VISIBLE in the result, or the model "+
			"has no reason to look for it:\n%s", res.Content)
	}
	// The whole server is one account, so the rest comes along too.
	if !te.expanded.contains("slack:T0/C0#main", "mcp__atlassian__searchConfluence") {
		t.Error("matching a server must unlock its other tools")
	}
}

// TestToolSearch_ReportsTheTotalNotTheTruncatedCount — "Found 8 matching
// tool(s)" was printed after truncating to 8, so a model that had hit the cap
// was told it had seen everything. A cap the caller cannot detect is a cap the
// caller cannot work around.
func TestToolSearch_ReportsTheTotalNotTheTruncatedCount(t *testing.T) {
	te := newExecutor(withMCPCatalog(func(string) []chat.Tool { return atlassianCatalog() }))
	te.expanded = newExpandedToolStore()

	res := te.toolSearch(`{"query":"Jira","limit":3}`, "easeit-companion", "s1")

	if strings.Contains(res.Content, "Found 3 matching tool(s)") {
		t.Errorf("the header must not report the truncated count as the total:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "showing the top 3") {
		t.Errorf("truncation must be stated so the model knows to raise limit:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "limit") {
		t.Errorf("the result must name the escape hatch:\n%s", res.Content)
	}
}

// A single-server catalog below the cap must keep the plain wording — the
// truncation notice should appear only when something was actually cut.
func TestToolSearch_NoTruncationNoticeWhenNothingWasCut(t *testing.T) {
	te := newExecutor(withMCPCatalog(func(string) []chat.Tool {
		return []chat.Tool{
			makeMCPTool("mcp__gmail__send_email", "Send an email via Gmail."),
			makeMCPTool("mcp__gmail__list_inbox", "List recent inbox messages."),
		}
	}))
	te.expanded = newExpandedToolStore()
	res := te.toolSearch(`{"query":"gmail"}`, "snake", "s1")
	if strings.Contains(res.Content, "showing the top") {
		t.Errorf("nothing was truncated, so no notice belongs here:\n%s", res.Content)
	}
}

// Unlocking a whole server must not leak ACROSS servers: a gmail query must not
// make the atlassian catalog callable.
func TestToolSearch_ServerUnlockDoesNotCrossServers(t *testing.T) {
	te := newExecutor(withMCPCatalog(func(string) []chat.Tool {
		return append(atlassianCatalog(),
			makeMCPTool("mcp__gmail__send_email", "Send an email via Gmail."))
	}))
	te.expanded = newExpandedToolStore()
	te.toolSearch(`{"query":"gmail send"}`, "snake", "s1")

	if te.expanded.contains("s1", "mcp__atlassian__getJiraIssue") {
		t.Error("a gmail query must not unlock the atlassian server")
	}
	if !te.expanded.contains("s1", "mcp__gmail__send_email") {
		t.Error("the matched tool must still be unlocked")
	}
}
