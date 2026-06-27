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
	peakResult := applyDeferredLoading(builtin, mcp, store, 99, peakThreshold)
	if !containsToolByName(peakResult, "mcp__a__one") {
		t.Errorf("PEAK tier should leave 5-tool catalog fully visible: %v", peakResult)
	}

	// DEGRADING tier: threshold clamped to 1 → deferral kicks in.
	degradedThreshold := effectiveDeferralThreshold(DefaultDeferredToolThreshold, chat.TierDegrading)
	degradedResult := applyDeferredLoading(builtin, mcp, store, 99, degradedThreshold)
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
	got := applyDeferredLoading(builtin, mcp, store, 99, 20)
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
	got := applyDeferredLoading(builtin, mcp, store, 99, 20)
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
	store.expand(42, []string{"mcp__a__a", "mcp__a__c"})

	got := applyDeferredLoading(builtin, mcp, store, 42, 20)
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
func TestApplyDeferredLoading_ChatIDZeroSkipsDeferral(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("list_projects", "list")}
	mcp := make([]chat.Tool, 25)
	for i := range mcp {
		mcp[i] = makeMCPTool("mcp__a__"+string(rune('a'+i%26)), "x")
	}
	store := newExpandedToolStore()
	// Even above threshold, chatID=0 should disable deferral.
	got := applyDeferredLoading(builtin, mcp, store, 0, 20)
	for _, m := range mcp {
		if !containsToolByName(got, m.Function.Name) {
			t.Errorf("chatID=0 must see every MCP tool; %q missing", m.Function.Name)
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
	s.expand(42, []string{"a", "b"})
	s.expand(42, []string{"c"})
	for _, want := range []string{"a", "b", "c"} {
		if !s.contains(42, want) {
			t.Errorf("expand-then-contains lost %q", want)
		}
	}
	if s.contains(99, "a") {
		t.Error("expansion must be scoped to its chatID")
	}
}

// TestExpandedToolStore_NilSafeAndZeroChatID — nil receiver +
// chatID=0 are common in the test helpers; the store must
// degrade cleanly rather than panic.
func TestExpandedToolStore_NilSafeAndZeroChatID(t *testing.T) {
	var nilStore *expandedToolStore
	nilStore.expand(1, []string{"a"})
	if nilStore.contains(1, "a") {
		t.Error("nil store must not retain anything")
	}
	s := newExpandedToolStore()
	s.expand(0, []string{"a"})
	if s.contains(0, "a") {
		t.Error("chatID=0 must be a no-op so sub-agent paths can pass it")
	}
}

// TestExpandedToolStore_ResetWipesSession — /new should be able
// to wipe the per-session set. The wiring lives in the bot;
// here we just lock in the primitive.
func TestExpandedToolStore_ResetWipesSession(t *testing.T) {
	s := newExpandedToolStore()
	s.expand(42, []string{"a"})
	s.reset(42)
	if s.contains(42, "a") {
		t.Error("reset must drop the session's expanded set")
	}
}

// TestExpandedToolStore_ResetEdgeCases — chatID=0 and nil receiver
// are silent no-ops; non-existing chatID is a silent no-op too.
func TestExpandedToolStore_ResetEdgeCases(t *testing.T) {
	t.Run("nil receiver no-op", func(t *testing.T) {
		var s *expandedToolStore
		// Just verify it doesn't panic — there's nothing to assert.
		s.reset(42)
	})
	t.Run("chatID=0 no-op", func(t *testing.T) {
		s := newExpandedToolStore()
		s.expand(42, []string{"a"})
		s.reset(0) // does nothing
		if !s.contains(42, "a") {
			t.Error("reset(0) must not affect other chat IDs")
		}
	})
	t.Run("non-existing chatID is a silent no-op", func(t *testing.T) {
		s := newExpandedToolStore()
		s.reset(9999) // not registered
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
	res := te.toolSearch(`{"query":"gmail send"}`, "snake", 42)
	if !strings.Contains(res.Content, "mcp__gmail__send_email") {
		t.Errorf("response must list the top match, got %q", res.Content)
	}
	if !te.expanded.contains(42, "mcp__gmail__send_email") {
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
	res := te.toolSearch(`{"query":"calendar"}`, "snake", 42)
	if !strings.Contains(res.Content, "No tools matched") {
		t.Errorf("expected friendly no-match copy, got %q", res.Content)
	}
}

// TestToolSearch_MissingQueryRejected — defensive arg parsing.
func TestToolSearch_MissingQueryRejected(t *testing.T) {
	te := newExecutor()
	res := te.toolSearch(`{}`, "snake", 42)
	if !strings.Contains(res.Content, "query is required") {
		t.Errorf("got %q", res.Content)
	}
}

// TestToolSearch_NoMCPManagerSurfacesFriendlyMessage — when
// the project has no MCP wired the tool returns a useful
// message rather than dispatching against nil.
func TestToolSearch_NoMCPManagerSurfacesFriendlyMessage(t *testing.T) {
	te := newExecutor() // no MCP catalog
	res := te.toolSearch(`{"query":"x"}`, "snake", 42)
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
