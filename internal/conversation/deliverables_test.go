package conversation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRenderDeliverableLinks_Empty — no links → empty string,
// so callers can append unconditionally without checking the
// slice length themselves.
func TestRenderDeliverableLinks_Empty(t *testing.T) {
	assert.Equal(t, "", RenderDeliverableLinks(nil))
	assert.Equal(t, "", RenderDeliverableLinks([]DeliverableLink{}))
}

// TestRenderDeliverableLinks_WithURLs — each link renders as
// "Download: <name> — <URL>"; the block opens with the
// "Produced files:" header so the chat client groups it.
func TestRenderDeliverableLinks_WithURLs(t *testing.T) {
	got := RenderDeliverableLinks([]DeliverableLink{
		{Name: "deliverable.md", URL: "https://example.com/ui/projects/p/artifacts/raw?path=deliverable.md"},
		{Name: "summary.txt", URL: "https://example.com/ui/projects/p/artifacts/raw?path=summary.txt"},
	})
	assert.Contains(t, got, "Produced files:")
	assert.Contains(t, got, "Download: deliverable.md — https://example.com/ui/projects/p/artifacts/raw?path=deliverable.md")
	assert.Contains(t, got, "Download: summary.txt — https://example.com/ui/projects/p/artifacts/raw?path=summary.txt")
	// Fallback "shell access" trailer is suppressed when URLs are present.
	assert.NotContains(t, got, "shell access")
}

// TestRenderDeliverableLinks_NoURLEmitsShellAccessNotice — when
// no link has a URL (baseURL was unset / artifact UI disabled)
// the block still surfaces the filename but explains the
// operator needs shell access.
func TestRenderDeliverableLinks_NoURLEmitsShellAccessNotice(t *testing.T) {
	got := RenderDeliverableLinks([]DeliverableLink{
		{Name: "deliverable.md"},
		{Name: "summary.txt"},
	})
	assert.Contains(t, got, "Download: deliverable.md")
	assert.Contains(t, got, "Download: summary.txt")
	assert.Contains(t, got, "no artifact UI configured")
	assert.Contains(t, got, "shell access")
	// Tightening: ensure no stray https:// crept in when URLs are blank.
	assert.False(t, strings.Contains(got, "https://"),
		"empty URLs must not render protocol fragments")
}

// TestRenderDeliverableLinks_SkipsBlankNames — defensive: a
// blank entry (could happen if produced_files included an empty
// string) should not produce a "Download: " orphan line.
func TestRenderDeliverableLinks_SkipsBlankNames(t *testing.T) {
	got := RenderDeliverableLinks([]DeliverableLink{
		{Name: "", URL: "https://x"},
		{Name: "real.md", URL: "https://example.com/r"},
	})
	assert.Contains(t, got, "Download: real.md")
	assert.NotContains(t, got, "Download:  —")
	assert.NotContains(t, got, "Download: \n")
}

// TestBuildDeliverableLinks_WithBaseURL — links carry an
// absolute URL pointing at the project-scoped artifacts/raw
// endpoint, with the filename URL-escaped.
func TestBuildDeliverableLinks_WithBaseURL(t *testing.T) {
	got := BuildDeliverableLinks("https://vornik.example.com",
		"p1", []string{"deliverable.md", "out/summary.txt"})
	if assert.Len(t, got, 2) {
		assert.Equal(t, "deliverable.md", got[0].Name)
		assert.Equal(t, "https://vornik.example.com/ui/projects/p1/artifacts/raw?path=deliverable.md", got[0].URL)
		assert.Equal(t, "out/summary.txt", got[1].Name)
		// Forward slashes in the path are query-escaped.
		assert.Equal(t, "https://vornik.example.com/ui/projects/p1/artifacts/raw?path=out%2Fsummary.txt", got[1].URL)
	}
}

// TestBuildDeliverableLinks_TrailingSlashOnBaseURL — operators
// sometimes configure WebUIBaseURL with a trailing slash; the
// helper trims it so the constructed URL doesn't end up with a
// double-slash before the path.
func TestBuildDeliverableLinks_TrailingSlashOnBaseURL(t *testing.T) {
	got := BuildDeliverableLinks("https://vornik.example.com/", "p1", []string{"x.md"})
	if assert.Len(t, got, 1) {
		assert.Equal(t, "https://vornik.example.com/ui/projects/p1/artifacts/raw?path=x.md", got[0].URL)
	}
}

// TestBuildDeliverableLinks_NoBaseURL — empty base URL leaves
// every link with URL="" so the renderer falls back to the
// shell-access notice.
func TestBuildDeliverableLinks_NoBaseURL(t *testing.T) {
	got := BuildDeliverableLinks("", "p1", []string{"deliverable.md"})
	if assert.Len(t, got, 1) {
		assert.Equal(t, "deliverable.md", got[0].Name)
		assert.Empty(t, got[0].URL)
	}
}

// TestBuildDeliverableLinks_NoProjectID — empty project id
// also disables URL emission (the raw endpoint is project-
// scoped). Channels still see the filenames.
func TestBuildDeliverableLinks_NoProjectID(t *testing.T) {
	got := BuildDeliverableLinks("https://vornik.example.com", "", []string{"a.md"})
	if assert.Len(t, got, 1) {
		assert.Empty(t, got[0].URL)
	}
}

// TestBuildDeliverableLinks_SkipsBlanks — defensive: blank
// names are dropped, not emitted as zero-Name links the
// renderer would have to filter again.
func TestBuildDeliverableLinks_SkipsBlanks(t *testing.T) {
	got := BuildDeliverableLinks("https://x", "p", []string{"", "  ", "real.md"})
	if assert.Len(t, got, 1) {
		assert.Equal(t, "real.md", got[0].Name)
	}
}
