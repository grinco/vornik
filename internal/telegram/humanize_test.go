package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestHumanize_PlainTextPassthrough — the helper must NOT touch
// strings that aren't JSON. Markdown, prose, code blocks all fall
// through unchanged so existing happy-path notifications keep
// working.
func TestHumanize_PlainTextPassthrough(t *testing.T) {
	cases := []string{
		"Task done — implemented the new endpoint.",
		"# Markdown\n\nWith **emphasis**.",
		"```go\nfunc main(){}\n```",
		"  multi-line  \n  prose with whitespace  ",
	}
	for _, c := range cases {
		t.Run(c[:min(len(c), 30)], func(t *testing.T) {
			got := humanizeTaskMessage(c)
			assert.Equal(t, strings.TrimSpace(c), got,
				"plain text must pass through unchanged (modulo trim)")
		})
	}
}

// TestHumanize_ReviewerApprovedExtractsFeedback — the headline
// case from the operator report. Reviewer agents emit
// {"approved": true, "feedback": "..."} as their LLM content.
// The bot used to paste the raw JSON; now it shows "✓ Approved"
// + the feedback text.
func TestHumanize_ReviewerApprovedExtractsFeedback(t *testing.T) {
	in := `{"approved": true, "feedback": "Looks good — tests cover the new code path and the migration is reversible."}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "✓ Approved", "approved=true must surface as a check glyph + label")
	assert.Contains(t, got, "Looks good", "feedback content must be the body of the summary")
	assert.NotContains(t, got, "{", "raw JSON braces must not survive humanization")
}

// TestHumanize_ReviewerRejectedExtractsFeedback — same shape, the
// other branch. ✗ Rejected + feedback explanation.
func TestHumanize_ReviewerRejectedExtractsFeedback(t *testing.T) {
	in := `{"approved": false, "feedback": "Missing tests for the error path."}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "✗ Rejected")
	assert.Contains(t, got, "Missing tests")
}

// TestHumanize_PrefersSummaryOverMessage — priority order matters.
// When both `summary` and `message` are present the more concise
// `summary` wins (scout/research roles emit both, and `summary` is
// the operator-friendly version).
func TestHumanize_PrefersSummaryOverMessage(t *testing.T) {
	in := `{"summary": "Found 3 candidates.", "message": "I have completed a thorough investigation of the codebase and identified the following candidate locations for the change..."}`
	got := humanizeTaskMessage(in)
	assert.Equal(t, "Found 3 candidates.", got,
		"summary must win over message when both present")
}

// TestHumanize_FallsBackToBulletsWhenNoProseField — agents with
// no recognised prose field still produce something readable.
// Top-level scalars become a bullet list rather than the operator
// seeing raw JSON.
func TestHumanize_FallsBackToBulletsWhenNoProseField(t *testing.T) {
	in := `{"score": 95, "tests_passed": true, "duration_sec": 42.5}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "• score: 95",
		"integer-shaped float must drop trailing zeros")
	assert.Contains(t, got, "• tests_passed: true")
	assert.Contains(t, got, "• duration_sec: 42.5")
	assert.NotContains(t, got, "{", "fallback must not include raw JSON braces")
}

// TestHumanize_BulletListSkipsMachineNoise — toolAudit, usage,
// diagnostics, etc. are machine-only fields that shouldn't bloat
// the bullet list. The bot's job is to surface what the operator
// cares about; structural metadata gets filtered.
func TestHumanize_BulletListSkipsMachineNoise(t *testing.T) {
	in := `{"score": 95, "toolAudit": [{"tool":"x"}], "usage": {"prompt_tokens": 100}, "diagnostics": {}}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "score")
	assert.NotContains(t, got, "toolAudit", "machine-noise field must not surface")
	assert.NotContains(t, got, "usage", "token usage isn't operator-facing in the bullet list")
}

// TestHumanize_LongStringValuesTruncated — a single field with a
// 1000-char string shouldn't blow out the entire chat message. The
// bullet-list path truncates at 120 chars.
func TestHumanize_LongStringValuesTruncated(t *testing.T) {
	huge := strings.Repeat("x", 500)
	in := `{"detail": "` + huge + `"}`
	got := humanizeTaskMessage(in)
	assert.Less(t, len(got), 200, "bulleted long string must be truncated")
	assert.Contains(t, got, "…")
}

// TestHumanize_TruncationPreservesValidUTF8 — regression for a bug
// where formatValue truncated at byte offset 120 and split multi-
// byte runes (CJK / emoji / Cyrillic), producing invalid UTF-8
// that Telegram's API rejects. The fix uses []rune-aware
// truncation; this test fails on the old code because the byte-
// sliced output isn't valid UTF-8.
func TestHumanize_TruncationPreservesValidUTF8(t *testing.T) {
	// Each Chinese character is 3 bytes in UTF-8. 60 of them =
	// 180 bytes, which exceeds the byte-cap on a naive slice.
	cjk := strings.Repeat("文", 200)
	in := `{"detail": "` + cjk + `"}`
	got := humanizeTaskMessage(in)
	assert.True(t, utf8.ValidString(got),
		"truncated body must remain valid UTF-8 — Telegram rejects messages otherwise")
	// Should contain ~120 runes (the cap) plus the ellipsis,
	// surrounding bullet, etc. — substantially fewer than the
	// 200 we put in.
	assert.Less(t, len([]rune(got)), 180,
		"truncation must reduce the rune count, not just byte length")

	// Same shape for emoji which are 4 bytes in UTF-8.
	emoji := strings.Repeat("🔒", 200)
	in = `{"detail": "` + emoji + `"}`
	got = humanizeTaskMessage(in)
	assert.True(t, utf8.ValidString(got), "emoji truncation must keep valid UTF-8")
}

// TestHumanize_StatusFieldDecorates — status="ok" or status="failed"
// without an approved field. Decoration glyph attaches.
func TestHumanize_StatusFieldDecorates(t *testing.T) {
	got := humanizeTaskMessage(`{"status": "ok", "summary": "All checks passed."}`)
	assert.Contains(t, got, "✓ Ok", "status=ok must produce a check glyph")
	assert.Contains(t, got, "All checks passed.")

	got = humanizeTaskMessage(`{"status": "failed", "error": "Tests failed."}`)
	assert.Contains(t, got, "✗ Failed")
	// Error field is in the prose priority list, so it surfaces.
	assert.Contains(t, got, "Tests failed.")
}

// TestHumanize_NestedObjectCollapses — a top-level value that's
// itself an object renders as "{N fields}" rather than nested
// JSON. Keeps the bullet list one level deep.
func TestHumanize_NestedObjectCollapses(t *testing.T) {
	in := `{"score": 95, "metadata": {"reviewer": "alice", "duration": 12}}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "{2 fields}", "nested object must collapse to shape descriptor")
}

// TestHumanize_ArrayCollapses — same shape on the array side.
// "[N items]" instead of pasting the array.
func TestHumanize_ArrayCollapses(t *testing.T) {
	in := `{"score": 95, "files_changed": ["a.go", "b.go", "c.go"]}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "[3 items]")
}

// TestHumanize_EmptyMessageReturnsEmpty — defensive. NotifyTask
// already short-circuits on empty input but the helper must not
// panic if called with an empty string anyway.
func TestHumanize_EmptyMessageReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", humanizeTaskMessage(""))
	assert.Equal(t, "", humanizeTaskMessage("   "))
}

// TestHumanize_MalformedJSONFallsBackToText — JSON that starts
// with `{` but doesn't parse cleanly must pass through as text.
// Operators occasionally see partial / truncated JSON in failure
// messages; surfacing them as text is better than dropping the
// signal.
func TestHumanize_MalformedJSONFallsBackToText(t *testing.T) {
	in := `{"approved": true, "feedback": "missing closing brace`
	got := humanizeTaskMessage(in)
	// Trimmed but otherwise unchanged.
	assert.Equal(t, in, got)
}

// TestHumanize_JSONArrayPassesThrough — the helper only special-
// cases JSON objects. Arrays are valid JSON but uncommon as
// reviewer outputs; let them flow as text rather than guessing
// what to summarise.
func TestHumanize_JSONArrayPassesThrough(t *testing.T) {
	in := `["a", "b", "c"]`
	got := humanizeTaskMessage(in)
	assert.Equal(t, in, got)
}

// TestHumanize_PrefixCombinesWithBulletList — when there's no
// prose field but the status decoration fires, the operator
// still gets the headline glyph above the bullet list.
func TestHumanize_PrefixCombinesWithBulletList(t *testing.T) {
	in := `{"approved": true, "score": 95, "categories": ["a", "b"]}`
	got := humanizeTaskMessage(in)
	assert.Contains(t, got, "✓ Approved", "headline glyph fires from approved=true")
	assert.Contains(t, got, "• score: 95", "bullet list still renders below")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
