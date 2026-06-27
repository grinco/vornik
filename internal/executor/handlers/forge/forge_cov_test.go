package forge

import (
	"encoding/json"
	"strings"
	"testing"

	forgeapi "vornik.io/vornik/internal/forge"
)

// TestForgeCov_TitleForJobWithTitle covers the branch where the job
// carries a non-empty title (the empty-title fallback is covered by
// TestBranchAndTitle).
func TestForgeCov_TitleForJobWithTitle(t *testing.T) {
	bug := forgeapi.ForgeJob{Number: 12, Title: "Crash on empty input", Labels: []string{"bug"}}
	if got := titleForJob(bug); got != "Fix #12: Crash on empty input" {
		t.Errorf("title with text = %q", got)
	}
	feat := forgeapi.ForgeJob{Number: 13, Title: "Add export", Labels: []string{"feature"}}
	if got := titleForJob(feat); got != "Implement #13: Add export" {
		t.Errorf("feature title with text = %q", got)
	}
}

// TestForgeCov_BodyForJobWithBodyAndTruncation covers the body-
// present branch and the >600-char truncation arm.
func TestForgeCov_BodyForJobWithBodyAndTruncation(t *testing.T) {
	short := forgeapi.ForgeJob{Number: 5, Body: "Please fix the parser."}
	b := bodyForJob(short)
	if !strings.Contains(b, "Closes #5.") {
		t.Errorf("body missing closes line: %q", b)
	}
	if !strings.Contains(b, "**Requested:** Please fix the parser.") {
		t.Errorf("body missing requested section: %q", b)
	}

	long := forgeapi.ForgeJob{Number: 6, Body: strings.Repeat("x", 800)}
	bl := bodyForJob(long)
	if !strings.Contains(bl, "…") {
		t.Errorf("long body should be truncated with an ellipsis: %q", bl)
	}
	// The requested excerpt must be capped at 600 chars + ellipsis.
	if strings.Count(bl, "x") > 600 {
		t.Errorf("truncation did not cap the body at 600 chars")
	}
}

// TestForgeCov_RenderRemaining covers the non-string-item (JSON
// fallback) branch and the empty-trimmed-item skip.
func TestForgeCov_RenderRemaining(t *testing.T) {
	out := renderRemaining([]any{
		"add a test",            // plain string
		"   ",                   // whitespace-only → skipped
		map[string]any{"id": 1}, // non-string → JSON-encoded
		"",                      // empty → skipped
	})
	if !strings.Contains(out, "- add a test") {
		t.Errorf("string item missing: %q", out)
	}
	if !strings.Contains(out, `{"id":1}`) {
		t.Errorf("non-string item not JSON-encoded: %q", out)
	}
	// Two renderable items → exactly two bullet lines.
	if n := strings.Count(out, "- "); n != 2 {
		t.Errorf("expected 2 bullets (empties skipped), got %d in %q", n, out)
	}
}

// TestForgeCov_ReviewFromObjectGuards covers reviewFromObject's
// empty-input guard and the bare-object (no "review" wrapper) parse
// path.
func TestForgeCov_ReviewFromObjectGuards(t *testing.T) {
	if r := reviewFromObject(nil); r != nil {
		t.Error("empty input should return nil")
	}
	// A review object with no usable text → nil.
	if r := reviewFromObject([]byte(`{"review":{"approved":true}}`)); r != nil {
		t.Error("review with no feedback/summary should return nil")
	}
	// Bare object (no "review" wrapper) carrying feedback → parsed.
	bare := []byte(`{"approved":false,"feedback":"needs work"}`)
	r := reviewFromObject(bare)
	if r == nil || r.Feedback != "needs work" {
		t.Errorf("bare review object should parse, got %+v", r)
	}
}

// TestForgeCov_RenderStructuredReviewEmptyText covers the branch
// where the structured review exists but carries no feedback/summary
// (renderStructuredReview returns ok=false). Routed via reviewBodyEvent
// so the caller falls back to the raw body.
func TestForgeCov_RenderStructuredReviewEmptyText(t *testing.T) {
	// A review with only an approved bool and no text → renderer
	// returns ok=false; reviewBodyEvent falls back to the raw body.
	prev, _ := json.Marshal(map[string]any{
		"body":   "raw fallback body",
		"review": map[string]any{"approved": true}, // no feedback/summary
	})
	body, _ := reviewBodyEvent(prev, false)
	if body != "raw fallback body" {
		t.Errorf("empty structured review should fall back to raw body, got %q", body)
	}
}

// TestForgeCov_RenderStructuredReviewWithRemaining exercises the full
// render path including the approved tickbox + remaining bullet list,
// so renderStructuredReview's remaining-items arm is covered.
func TestForgeCov_RenderStructuredReviewWithRemaining(t *testing.T) {
	out, ok := renderStructuredReview(json.RawMessage(`{"review":{"approved":false,"feedback":"fix it","summary":"changes","remaining":["item one","item two"]}}`), "")
	if !ok {
		t.Fatal("expected a rendered review")
	}
	if !strings.Contains(out, "Changes requested") {
		t.Errorf("missing changes-requested header: %q", out)
	}
	if !strings.Contains(out, "**Remaining:**") || !strings.Contains(out, "- item one") {
		t.Errorf("missing remaining list: %q", out)
	}
	if !strings.Contains(out, "**Summary:** changes") {
		t.Errorf("missing summary: %q", out)
	}
}
