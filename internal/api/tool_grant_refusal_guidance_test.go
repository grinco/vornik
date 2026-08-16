package api

import (
	"strings"
	"testing"
)

// Regression, measured 2026-08-16: 58 of 98 grant requests ever made were
// REFUSED (59%), and the refused names were overwhelmingly tools the role does
// not hold — mcp__scraper__fetch_url (21), memory_search (12),
// mcp__scraper__ical_events (10) — including invented variants of the same
// imagined tool (fetch_url / web_fetch / fetch_markdown / scrape_url).
//
// One adaptive `route` step made SIX attempts, narrowing 4 tools → 3 → 2, and
// still failed: it was brute-force searching its own ceiling. Every attempt is
// a full LLM round trip replaying a growing conversation, which is what turned
// a workflow-selection step into 8 calls and 66-90k prompt tokens, and on a
// Bedrock fallback is also what exhausted the step timeout. The task then
// finalised without selected_workflow and failed.
//
// The refusal must not name what was refused — the lead's context can contain
// injected text, and echoing names lets it enumerate the ceiling by probing.
// But it CAN state the invariant and a count. The agent's own tool list is
// already visible to it, so neither reveals anything it cannot see.
func TestEvaluateToolGrant_RefusalTellsTheAgentHowToSucceed(t *testing.T) {
	ceiling := []string{"file_read", "grep", "glob"}
	out := EvaluateToolGrant([]string{"file_read", "file_write", "memory_search"}, ceiling)

	if len(out.Accepted) != 0 {
		t.Fatalf("a request naming anything outside the ceiling is refused whole, got accepted=%v", out.Accepted)
	}

	msg := out.Message
	if msg == "" {
		t.Fatal("refusal must carry a message")
	}

	// It must state the invariant the model keeps violating: a grant NARROWS
	// tools already held, it cannot acquire new ones.
	lowered := strings.ToLower(msg)
	if !strings.Contains(lowered, "narrow") || !strings.Contains(lowered, "already") {
		t.Errorf("refusal must state that a grant can only narrow tools the role already holds; got %q", msg)
	}

	// A count is safe — one integer, not the ceiling's membership — and tells
	// the agent how far off it is instead of leaving it to bisect.
	if !strings.Contains(msg, "2") {
		t.Errorf("refusal should say HOW MANY names were outside the set (2 here); got %q", msg)
	}

	// THE SECURITY BOUNDARY. Refused names must never reach the agent.
	for _, leaked := range []string{"file_write", "memory_search"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("refusal leaked the refused name %q — injected text could enumerate the ceiling by probing", leaked)
		}
	}
	// Nor may it leak the ceiling's own membership.
	for _, leaked := range []string{"grep", "glob"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("refusal leaked ceiling member %q", leaked)
		}
	}

	// The audit trail still gets everything.
	if len(out.RefusedNames) != 2 {
		t.Errorf("audit must retain both refused names, got %v", out.RefusedNames)
	}
}

// A clean grant stays clean and silent.
func TestEvaluateToolGrant_AcceptedGrantHasNoRefusalMessage(t *testing.T) {
	out := EvaluateToolGrant([]string{"file_read"}, []string{"file_read", "grep"})
	if len(out.Accepted) != 1 || out.Message != "" || len(out.RefusedNames) != 0 {
		t.Fatalf("a subset grant must be accepted silently: accepted=%v msg=%q refused=%v",
			out.Accepted, out.Message, out.RefusedNames)
	}
}

// The tool's own parameter description must not invite the model to list what
// it WANTS: "tools this step needs" plus an mcp__server__tool format hint is
// what produced invented scraper names. It must point at the tools already
// available instead.
func TestGrantStepTools_ParameterDescriptionPointsAtHeldTools(t *testing.T) {
	p := &ToolGrantProvider{Grants: &fakeGrantStore{}}
	tools := p.Tools("")
	if len(tools) != 1 {
		t.Fatalf("expected exactly one advertised tool, got %d", len(tools))
	}
	params := strings.ToLower(string(tools[0].Function.Parameters))
	if !strings.Contains(params, "already") {
		t.Errorf("the tools parameter must say the names come from what the role already has; got %s", params)
	}
	if strings.Contains(params, "this step needs") {
		t.Errorf("phrasing invites listing desired tools rather than held ones; got %s", params)
	}
}
