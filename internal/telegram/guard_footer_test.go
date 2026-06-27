package telegram

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/outputguard"
)

// TestRenderGuardFooter_EmptyReturnsEmpty — happy-path tool
// calls produce no warnings; the footer must be empty so the
// chat stays clean.
func TestRenderGuardFooter_EmptyReturnsEmpty(t *testing.T) {
	if got := renderGuardFooter(nil); got != "" {
		t.Errorf("renderGuardFooter(nil) = %q, want empty", got)
	}
	if got := renderGuardFooter([]dispatcher.GuardWarning{}); got != "" {
		t.Errorf("renderGuardFooter([]) = %q, want empty", got)
	}
}

// TestGuardFooterPostprocessor_NoWarningsPassthrough — no
// findings → the postprocessor returns Result.Text untouched.
// Slice 2 wiring: this is the common case (~99% of turns), so a
// no-op fast-path keeps the chat clean.
func TestGuardFooterPostprocessor_NoWarningsPassthrough(t *testing.T) {
	pp := GuardFooterPostprocessor()
	got := pp(dispatcher.Result{Text: "hello"})
	if got != "hello" {
		t.Errorf("postprocessor(empty warnings) = %q, want 'hello' verbatim", got)
	}
}

// TestGuardFooterPostprocessor_AppendsFooter — when warnings are
// present, the postprocessor appends the rendered footer below a
// blank line. Confirms the wiring the receiver path depends on.
func TestGuardFooterPostprocessor_AppendsFooter(t *testing.T) {
	pp := GuardFooterPostprocessor()
	r := dispatcher.Result{
		Text: "core reply",
		GuardWarnings: []dispatcher.GuardWarning{{
			Tool: "web_fetch", MaxSeverity: outputguard.SeverityHigh,
			Kinds: []string{"injection_instruction"}, Redacted: true,
		}},
	}
	got := pp(r)
	if !strings.Contains(got, "core reply") {
		t.Errorf("postprocessor lost Result.Text: %q", got)
	}
	if !strings.Contains(got, "Output guard flagged") {
		t.Errorf("postprocessor did not append guard footer: %q", got)
	}
}

// TestGuardFooterPostprocessor_EmptyTextStillSurfacesFooter —
// degenerate case: dispatcher returned empty Text but produced
// warnings (e.g. the assistant called only a tool, no prose
// reply). The footer should still surface so the operator sees
// the finding.
func TestGuardFooterPostprocessor_EmptyTextStillSurfacesFooter(t *testing.T) {
	pp := GuardFooterPostprocessor()
	r := dispatcher.Result{
		GuardWarnings: []dispatcher.GuardWarning{{
			Tool: "fetch", MaxSeverity: outputguard.SeverityWarn,
			Kinds: []string{"credential_pattern"},
		}},
	}
	got := pp(r)
	if !strings.Contains(got, "credential_pattern") {
		t.Errorf("empty-text postprocessor: %q, want guard footer", got)
	}
}

// TestRenderGuardFooter_SingleHighRedacted — canonical case:
// one tool, one HIGH finding, redacted. Footer must surface the
// tool name + kind + the "auto-redacted" suffix so the operator
// sees both signals at a glance.
func TestRenderGuardFooter_SingleHighRedacted(t *testing.T) {
	w := []dispatcher.GuardWarning{{
		Tool: "web_fetch", MaxSeverity: outputguard.SeverityHigh,
		Kinds: []string{"injection_instruction"}, Redacted: true,
	}}
	got := renderGuardFooter(w)
	for _, want := range []string{
		"Output guard flagged 1 finding",
		"web_fetch",
		"injection_instruction",
		"auto-redacted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("footer missing %q: %s", want, got)
		}
	}
}

// TestRenderGuardFooter_MultipleToolsDeduped — two tools both
// fire findings with overlapping kinds. The footer must list
// each tool once and each kind once, both in alphabetical order
// for stable rendering.
func TestRenderGuardFooter_MultipleToolsDeduped(t *testing.T) {
	w := []dispatcher.GuardWarning{
		{Tool: "web_fetch", MaxSeverity: outputguard.SeverityWarn,
			Kinds: []string{"encoded_payload"}},
		{Tool: "file_read", MaxSeverity: outputguard.SeverityHigh,
			Kinds: []string{"injection_instruction", "encoded_payload"}, Redacted: true},
	}
	got := renderGuardFooter(w)
	// Tools alphabetised: file_read before web_fetch.
	if !strings.Contains(got, "file_read, web_fetch") {
		t.Errorf("tools not alphabetised in footer: %s", got)
	}
	// Kinds alphabetised + deduped.
	if !strings.Contains(got, "encoded_payload, injection_instruction") {
		t.Errorf("kinds not alphabetised/deduped: %s", got)
	}
	if !strings.Contains(got, "2 findings") {
		t.Errorf("count missing: %s", got)
	}
	if !strings.Contains(got, "auto-redacted") {
		t.Errorf("redaction suffix missing when at least one HIGH was redacted: %s", got)
	}
}

// TestRenderGuardFooter_NoRedactionDropsSuffix — when nothing
// was redacted (all findings WARN, or RedactHigh disabled), the
// "(auto-redacted)" suffix must NOT appear. Operators can tell
// from the footer alone whether content was rewritten.
func TestRenderGuardFooter_NoRedactionDropsSuffix(t *testing.T) {
	w := []dispatcher.GuardWarning{{
		Tool: "web_fetch", MaxSeverity: outputguard.SeverityWarn,
		Kinds: []string{"injection_system_marker"}, Redacted: false,
	}}
	got := renderGuardFooter(w)
	if got == "" {
		t.Fatalf("WARN finding should produce a footer; got empty")
	}
	if strings.Contains(got, "redacted") {
		t.Errorf("redacted suffix shown despite Redacted=false: %s", got)
	}
}

// TestRenderGuardFooter_InfoOnlyWarningDropped — a pure-Info
// warning (e.g. encoded_payload triggered by a long URL query
// string) must NOT reach the chat footer. INFO findings stay
// in the audit log / GuardWarnings for the UI, but the chat
// surface is operator-actionable Warn+ only.
//
// This is the bug-fix anchor: before the filter, scrapers
// hitting long URLs would spam the chat with "⚠ Output guard
// flagged 1 finding on mcp__scraper__web_fetch: encoded_payload".
func TestRenderGuardFooter_InfoOnlyWarningDropped(t *testing.T) {
	w := []dispatcher.GuardWarning{{
		Tool: "mcp__scraper__web_fetch", MaxSeverity: outputguard.SeverityInfo,
		Kinds: []string{"encoded_payload"}, Redacted: false,
	}}
	got := renderGuardFooter(w)
	if got != "" {
		t.Errorf("pure-Info warning must produce empty footer; got %q", got)
	}
}

// TestRenderGuardFooter_InfoMixedWithWarnSurvives — when a
// warning carries both an Info-rank kind AND a Warn+ kind, the
// MaxSeverity stays at Warn so the warning survives the filter
// and the footer lists both kinds. This pins the "drop pure-Info
// only" contract.
func TestRenderGuardFooter_InfoMixedWithWarnSurvives(t *testing.T) {
	w := []dispatcher.GuardWarning{{
		Tool: "web_fetch", MaxSeverity: outputguard.SeverityWarn,
		Kinds: []string{"encoded_payload", "injection_system_marker"},
	}}
	got := renderGuardFooter(w)
	if got == "" {
		t.Fatal("Warn-level warning with mixed kinds must produce a footer")
	}
	for _, want := range []string{"encoded_payload", "injection_system_marker", "web_fetch"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in footer; got %s", want, got)
		}
	}
}

// TestRenderGuardFooter_InfoOnlyMixedAcrossWarningsDropped —
// when the slice mixes a pure-Info warning with a Warn+ warning,
// only the Warn+ one reaches the footer; the count reflects the
// surviving warnings, not the pre-filter total.
func TestRenderGuardFooter_InfoOnlyMixedAcrossWarningsDropped(t *testing.T) {
	w := []dispatcher.GuardWarning{
		{Tool: "mcp__scraper__web_fetch", MaxSeverity: outputguard.SeverityInfo,
			Kinds: []string{"encoded_payload"}},
		{Tool: "file_read", MaxSeverity: outputguard.SeverityWarn,
			Kinds: []string{"injection_system_marker"}},
	}
	got := renderGuardFooter(w)
	if !strings.Contains(got, "1 finding") {
		t.Errorf("count should be 1 (post-filter), got: %s", got)
	}
	if strings.Contains(got, "mcp__scraper__web_fetch") {
		t.Errorf("Info-only tool should not appear in footer: %s", got)
	}
	if !strings.Contains(got, "file_read") {
		t.Errorf("Warn-level tool must appear in footer: %s", got)
	}
}

// TestDedupePreserveOrder — single-pass dedupe on sorted input.
func TestDedupePreserveOrder(t *testing.T) {
	if got := dedupePreserveOrder([]string{"a", "a", "b", "b", "c"}); len(got) != 3 ||
		got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("dedupe wrong: %v", got)
	}
	// Single-element + empty paths.
	if got := dedupePreserveOrder([]string{"a"}); len(got) != 1 {
		t.Errorf("single-element: %v", got)
	}
	if got := dedupePreserveOrder(nil); got != nil {
		t.Errorf("nil should return nil: %v", got)
	}
}
