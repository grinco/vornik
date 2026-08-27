package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// `playbook list` renders two vocabularies into one table as of 2026-08-26 —
// 23 task classes describing a whole task, and 19 step classes describing one
// step of one execution (Finding B, docs/audits/2026-08-26-silent-controls-audit.md).
// Without the scope column an operator sees 42 rows with no way to tell which
// lifetime a class describes, which is a worse surface than the 23-row table
// it replaces.
func TestPlaybookListRow_RendersScope(t *testing.T) {
	got := playbookListRow(playbookEntryCLI{
		Class: "container_non_zero_exit",
		Scope: "step",
		Cause: "NOT A DIAGNOSIS. This is the residual bucket.",
	})
	parts := strings.Split(got, "\t")
	assert.Equal(t, "container_non_zero_exit", parts[0])
	assert.Equal(t, "step", parts[1], "scope must render between class and cause")
	assert.Contains(t, parts[2], "residual bucket")
}

// The cause column is trimmed to one line for the index view; the full text is
// available via `playbook show`. A multi-line cause must not break the table.
func TestPlaybookListRow_TrimsCauseToOneLine(t *testing.T) {
	got := playbookListRow(playbookEntryCLI{
		Class: "TIMEOUT",
		Scope: "task",
		Cause: "first line\nsecond line that must not appear",
	})
	assert.NotContains(t, got, "second line")
	assert.Contains(t, got, "first line")
}

// An entry with no scope must still render — the wire shape is shared with
// older daemons, and a missing scope is not a reason to drop the row.
func TestPlaybookListRow_MissingScopeStillRenders(t *testing.T) {
	got := playbookListRow(playbookEntryCLI{Class: "UNKNOWN", Cause: "no pattern matched"})
	assert.Contains(t, got, "UNKNOWN")
	assert.Contains(t, got, "no pattern matched")
}
