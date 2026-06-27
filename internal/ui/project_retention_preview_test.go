// Package ui: tests for the RetentionPreviewCounts shape + the
// Total helper that powers the headline "X rows pruned next sweep"
// in project_detail.html.
package ui

import (
	"testing"
)

// TestRetentionPreviewCounts_Total sums every counted table EXCEPT
// ArtifactFiles (a derivative count surfaced separately). Pins the
// arithmetic so a renamed field can't silently drop from the
// headline number.
func TestRetentionPreviewCounts_Total(t *testing.T) {
	c := RetentionPreviewCounts{
		TaskLLMUsage:  10,
		ToolAudit:     20,
		Tasks:         5,
		Executions:    7,
		Artifacts:     3,
		ArtifactFiles: 99, // not summed — derivative of Artifacts
		TaskMessages:  2,
		MemoryChunks:  4,
	}
	got := c.Total()
	want := 10 + 20 + 5 + 7 + 3 + 2 + 4
	if got != want {
		t.Errorf("Total = %d, want %d", got, want)
	}
}

// TestRetentionPreviewCounts_TotalZero — the zero-value renders
// "0 rows pruned next sweep" in the template, which is the
// "nothing in the trailing-window backlog" happy state.
func TestRetentionPreviewCounts_TotalZero(t *testing.T) {
	var c RetentionPreviewCounts
	if got := c.Total(); got != 0 {
		t.Errorf("zero-value Total = %d, want 0", got)
	}
}
