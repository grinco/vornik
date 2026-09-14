package verifier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// ZERO COVERAGE IS NOT A CLEAN RESULT.
//
// Finding D of the 2026-08-26 silent-controls audit, the last case it could not
// reach: when every in-scope audit entry is unclassifiable — `unclassified > 0`
// while successful, blocked and excused are all zero — verifyNoStatus429 took
// the "nothing in scope at all" early return and reported nothing. The step
// passed, and it passed identically whether the scraper had never run or had
// run 2,651 times with every row unreadable. In production 2,651 of 4,481
// web_fetch rows were unreadable at once, after a blind 4096-char tool_output
// slice cut the JSON mid-object, so this is the observed case and not a corner.
//
// The generalisation the audit stated: a control with a coverage boundary must
// publish that boundary. "Zero findings" and "zero coverage" must not render
// identically. BACKLOG 2026-09-03, closed 2026-09-13 at severity WARN — visible
// and non-blocking, the middle option of the three the item listed.

// unreadableRow is an in-scope scraper entry that classifyAuditEntry cannot
// read: valid-looking tool, output that is not JSON and carries no anchored
// marker to fall back on.
func unreadableRow() *persistence.ToolAuditEntry {
	return &persistence.ToolAuditEntry{
		ToolName:   "mcp__scraper__web_fetch",
		ToolOutput: `{"status":200,"content":"the response was cut mid-obj`,
	}
}

func TestNoStatus429_ZeroCoverageIsReportedAtWarn(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{AuditEntries: []*persistence.ToolAuditEntry{
		unreadableRow(), unreadableRow(), unreadableRow(),
	}}

	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "three unreadable in-scope entries is ZERO COVERAGE and must not render as a clean pass")
	assert.Equal(t, SeverityWarn, v.Severity,
		"zero coverage is visible and non-blocking by default; promoting it to fail is the operator's call")
	assert.False(t, v.Terminal, "a coverage report must never stop a retry")
	assert.Contains(t, v.Detail, "3", "the report must publish HOW MANY entries it could not read")
	assert.Contains(t, v.Detail, "0", "and that nothing was classified")
}

// The operator can promote it, which is the whole reason it rides on a
// Violation rather than a log line.
func TestNoStatus429_ZeroCoverageSeverityIsOverridable(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit", Severity: "fail"}
	in := Input{AuditEntries: []*persistence.ToolAuditEntry{unreadableRow()}}

	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Equal(t, SeverityFail, v.Severity)
}

// GENUINE ABSENCE STILL REPORTS NOTHING. This is the line the fix must not
// cross: a step whose scraper was never supposed to run must not acquire a
// finding, or every configured step gets one. "The scraper was supposed to run
// and didn't" is must_contain_url's job, not this verifier's.
func TestNoStatus429_NoEntriesAtAllStillReportsNothing(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}

	v, err := Run(context.Background(), cfg, Input{})
	require.NoError(t, err)
	assert.Nil(t, v, "no audit entries at all is absence, not zero coverage")

	// Out-of-scope entries are absence too: nothing in scope was examined
	// because nothing in scope exists.
	v, err = Run(context.Background(), Config{
		Type:   "no_status_429_in_audit",
		Params: map[string]any{"tools": []any{"mcp__scraper__web_fetch"}},
	}, Input{AuditEntries: []*persistence.ToolAuditEntry{
		{ToolName: "file_read", ToolOutput: "not json either"},
	}})
	require.NoError(t, err)
	assert.Nil(t, v, "an entry outside the configured tool scope is not coverage this verifier lost")
}

// A step that DID classify entries keeps its existing verdict shape: the
// coverage report is the zero-classified case only, so no currently-passing
// step acquires a warning because one row was unreadable.
func TestNoStatus429_PartialCoverageIsNotTheZeroCoverageCase(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{AuditEntries: []*persistence.ToolAuditEntry{
		unreadableRow(),
		{ToolName: "mcp__scraper__web_fetch", ToolOutput: `{"status":200,"final_url":"https://ok.example","block_reason":""}`},
	}}

	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "one readable success means the verifier had coverage; the unclassified count rides on a real violation when there is one")
}
