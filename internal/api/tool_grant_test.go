package api

import (
	"strings"
	"testing"
)

// The grant policy, per registry design §10.1–§10.4.
//
// The security argument is that a grant can only SUBTRACT, so an injected "grant
// place_order to the next step" cannot succeed. These tests pin that property
// directly rather than trusting the arrangement of the code.

func TestEvaluateToolGrant_AcceptsWithinCeiling(t *testing.T) {
	out := EvaluateToolGrant(
		[]string{"mcp__scraper__web_fetch"},
		[]string{"mcp__scraper__web_fetch", "mcp__broker__quote"},
	)
	if len(out.RefusedNames) != 0 {
		t.Fatalf("refused %v for a grant inside the ceiling", out.RefusedNames)
	}
	if len(out.Accepted) != 1 {
		t.Errorf("accepted %v, want the one requested tool", out.Accepted)
	}
}

// TestEvaluateToolGrant_RefusesOutsideCeiling is the escalation attempt — the shape
// a prompt injection would take.
func TestEvaluateToolGrant_RefusesOutsideCeiling(t *testing.T) {
	out := EvaluateToolGrant(
		[]string{"mcp__scraper__web_fetch", "mcp__broker__place_order"},
		[]string{"mcp__scraper__web_fetch"},
	)
	if len(out.Accepted) != 0 {
		t.Errorf("accepted %v from a request that named a tool outside the ceiling; the "+
			"whole request must be refused, or a lead proceeds believing it scoped a step "+
			"when it partly did not", out.Accepted)
	}
	if len(out.RefusedNames) != 1 || out.RefusedNames[0] != "mcp__broker__place_order" {
		t.Errorf("audit names = %v, want the offending tool recorded for the operator",
			out.RefusedNames)
	}
}

// TestEvaluateToolGrant_MessageDoesNotLeakCeilingContents is the probing defence.
// The lead's context can contain injected text; echoing refused names lets that text
// enumerate the ceiling one guess at a time (§10.3(4)).
func TestEvaluateToolGrant_MessageDoesNotLeakCeilingContents(t *testing.T) {
	out := EvaluateToolGrant(
		[]string{"mcp__broker__place_order", "mcp__homeassistant__ha_manage_backup"},
		[]string{"mcp__scraper__web_fetch"},
	)
	for _, secret := range []string{"place_order", "ha_manage_backup", "web_fetch", "scraper", "broker"} {
		if strings.Contains(out.Message, secret) {
			t.Errorf("agent-visible message %q contains %q — an injected prompt can then "+
				"enumerate the ceiling by probing one name at a time", out.Message, secret)
		}
	}
	if out.Message == "" {
		t.Error("no message: silent refusal teaches the lead the grant succeeded")
	}
}

// TestGrantedTools_CannotWidenBeyondCeiling is the load-bearing property. Whatever a
// grant asks for, the advertised set stays within the ceiling.
func TestGrantedTools_CannotWidenBeyondCeiling(t *testing.T) {
	all := tools("mcp__broker__quote", "mcp__broker__place_order", "mcp__scraper__web_fetch")
	ceiling := []string{"mcp__broker__quote"}

	hostile := [][]string{
		{"mcp__broker__place_order"},          // a tool outside the ceiling
		{"mcp__*"},                            // a wildcard grab
		{"mcp__scraper__web_fetch", "mcp__*"}, // mixed
	}
	for _, grant := range hostile {
		got := toolNames(GrantedTools(all, ceiling, grant))
		for _, name := range got {
			if name != "mcp__broker__quote" {
				t.Errorf("grant %v advertised %q, outside the ceiling %v — the subtract-only "+
					"invariant is broken and an injected grant could widen a step's reach",
					grant, name, ceiling)
			}
		}
	}
}

func TestGrantedTools_NarrowsWithinCeiling(t *testing.T) {
	all := tools("a", "b", "c")
	got := toolNames(GrantedTools(all, []string{"a", "b"}, []string{"a"}))
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("advertised %v, want just the granted subset", got)
	}
}

// TestGrantedTools_NoGrantIsCeilingOnly pins the ship-inert default: without a grant,
// behaviour is exactly the pre-feature ceiling.
func TestGrantedTools_NoGrantIsCeilingOnly(t *testing.T) {
	all := tools("a", "b", "c")
	withGrant := toolNames(GrantedTools(all, []string{"a", "b"}, nil))
	ceilingOnly := toolNames(advertisedTools(all, []string{"a", "b"}))
	if strings.Join(withGrant, ",") != strings.Join(ceilingOnly, ",") {
		t.Errorf("no-grant path advertised %v but the ceiling alone gives %v; the feature "+
			"must be inert until a grant exists", withGrant, ceilingOnly)
	}
}

func TestMayEscalate_BoundedPerStep(t *testing.T) {
	if !MayEscalate(nil) {
		t.Error("a step with no grant yet must be allowed to escalate")
	}
	g := &ToolGrant{StepID: "s1"}
	for i := 0; i < maxEscalationsPerStep; i++ {
		if !MayEscalate(g) {
			t.Fatalf("refused escalation %d of %d", i+1, maxEscalationsPerStep)
		}
		g.Escalations++
	}
	if MayEscalate(g) {
		t.Errorf("escalation %d allowed past the limit; an injected prompt could otherwise "+
			"force unbounded escalate cycles, each audited", g.Escalations+1)
	}
}

// TestCeilingHash_OrderIndependentAndDistinguishing: the hash exists so a reviewer
// can tell "never in the ceiling" from "the ceiling was tightened after the grant".
// A reordered allowlist is the same ceiling; a different membership is not.
func TestCeilingHash_OrderIndependentAndDistinguishing(t *testing.T) {
	a := CeilingHash([]string{"x", "y"})
	if b := CeilingHash([]string{"y", "x"}); a != b {
		t.Errorf("reordering changed the hash (%s vs %s); a reordered allowlist is the "+
			"same ceiling and would read as drift", a, b)
	}
	if c := CeilingHash([]string{"x"}); a == c {
		t.Error("a tightened ceiling hashed identically — drift becomes invisible in the audit")
	}
	if CeilingHash(nil) != "unrestricted" {
		t.Error("an empty ceiling should be recorded as unrestricted, not as a digest of nothing")
	}
}
