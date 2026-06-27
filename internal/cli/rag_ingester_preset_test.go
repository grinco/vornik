package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompanionIngestPreset_RagIngesterRoleHardens pins the
// 2026-05-28 ingest-task post-mortem fixes:
//
//   - Grep is NOT in allowedTools. The role's job is mechanical
//     file copy; granting grep let a real task burn 24/60
//     iterations on content exploration before hitting the cap.
//
//   - The prompt warns against exploration explicitly. Without
//     this, agents reach for grep/glob/document_* MCP tools to
//     "find" sources the payload already named — same failure
//     mode that B-10 fixed for /vornik-upload.
//
// Both invariants are easy to lose in a future prompt tidy-up;
// the test runs against the embedded preset bytes so any edit
// that re-introduces the old allowlist or softens the prompt
// trips on commit.
func TestCompanionIngestPreset_RagIngesterRoleHardens(t *testing.T) {
	raw, err := readPreset("companion-ingest")
	require.NoError(t, err)
	text := raw

	roleStart := strings.Index(text, `- name: "rag-ingester"`)
	require.NotEqual(t, -1, roleStart, "rag-ingester role must exist in the preset")
	roleEnd := strings.Index(text[roleStart:], "---")
	require.NotEqual(t, -1, roleEnd, "front-matter terminator must follow the role list")
	roleBlock := text[roleStart : roleStart+roleEnd]

	allowedStart := strings.Index(roleBlock, "allowedTools:")
	require.NotEqual(t, -1, allowedStart, "allowedTools block must exist")
	// Bound the allowlist scan at the next top-level key so we
	// don't accidentally pick up a `- "grep"` from a sibling role.
	allowedEnd := strings.Index(roleBlock[allowedStart:], "delegationAllowed")
	require.NotEqual(t, -1, allowedEnd, "delegationAllowed must close the permissions block")
	allowlist := roleBlock[allowedStart : allowedStart+allowedEnd]

	assert.NotContains(t, allowlist, `"grep"`,
		"rag-ingester must NOT grant grep: it's not used by any documented step and historically burned 24/60 iterations on content exploration")
	assert.Contains(t, allowlist, `"file_read"`, "rag-ingester needs file_read")
	assert.Contains(t, allowlist, `"file_write"`, "rag-ingester needs file_write")
	assert.Contains(t, allowlist, `"glob"`, "rag-ingester needs glob for source_dir/source_glob resolution")
	assert.Contains(t, allowlist, `"memory_search"`, "rag-ingester needs memory_search for dedup")

	promptStart := strings.Index(text, "### rag-ingester")
	require.NotEqual(t, -1, promptStart, "rag-ingester prompt section must exist")
	promptBody := text[promptStart:]

	for _, marker := range []string{
		"mechanical",                 // job framing
		"No exploration",             // anti-exploration ban
		"Do not grep",                // explicit grep ban (defense-in-depth alongside allowlist)
		"mcp__vornik__document_",     // explicit ban on read-side MCP tools
		"Fail fast on missing files", // missing-source contract
		"context.inputArtifacts",     // contract anchor: agents read from staged-in container paths, NOT from prompt text
		"One pass over",              // single-pass discipline
		"Iteration budget",           // budget awareness
		"NEVER read source paths from the prompt text", // hard guard against legacy source_paths contract
	} {
		assert.Contains(t, promptBody, marker,
			"rag-ingester prompt must contain anti-exploration marker %q", marker)
	}

	// Ensure the prompt is also explicit about NOT calling read-side
	// MCP document tools (mcp__vornik__document_get_outline /
	// _read_section / _get_metadata). A 2026-05-28 task burned 24
	// iterations on those; an explicit ban is the load-bearing fix.
	docToolBanIdx := strings.Index(promptBody, "mcp__vornik__document_")
	require.NotEqual(t, -1, docToolBanIdx)
	contextWindow := promptBody[max(0, docToolBanIdx-150):docToolBanIdx]
	assert.True(t,
		strings.Contains(contextWindow, "Do not") ||
			strings.Contains(contextWindow, "do not"),
		"mention of mcp__vornik__document_ tools must be a prohibition, not a recommendation; got context: %q", contextWindow)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
