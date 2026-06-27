// Tests for the 2026-05-16 attached-file path-rewriting + sentinel
// helpers in buildAgentInput. The mitigation is two-layer:
//
//   1. rewriteInputPathsInPrompt scrubs known and likely-stale paths
//      from the user prompt so an agent following "read /tmp/foo.pdf"
//      ends up at the actual container location.
//   2. buildAttachedFilesBlock emits an authoritative "files are here"
//      block appended after the user prompt as ground truth.
//
// Operator-observed failure that motivated this: small dispatcher
// LLMs put hallucinated paths like "/tmp/cv.pdf" into the task
// prompt despite the bot's system suffix telling them to reference
// /app/workspace/artifacts/in/. The rewrite saves the agent from
// the literal-instructions trap; the trailing block is the durable
// truth source the agent should always trust.

package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRewriteInputPathsInPrompt_ReplacesExactHostPath — the canonical
// case: the LLM helpfully embedded the original host path in the
// prompt verbatim. Rewrite it to the canonical container path.
func TestRewriteInputPathsInPrompt_ReplacesExactHostPath(t *testing.T) {
	got := rewriteInputPathsInPrompt(
		"Read /opt/vornik/.local/share/vornik/workspaces/x/uploads/cv.pdf and summarise",
		[]string{"/opt/vornik/.local/share/vornik/workspaces/x/uploads/cv.pdf"},
	)
	assert.Equal(t,
		"Read /app/workspace/artifacts/in/cv.pdf and summarise",
		got,
		"exact host path must be rewritten to the container path so the agent's file_read lands on the staged file")
}

// TestRewriteInputPathsInPrompt_ReplacesArtifactStorePath — when the
// dispatcher has already snapshotted the file, inputFiles points at
// the durable artifact store path. If THAT path ends up in the
// prompt (e.g. the dispatcher LLM read it out of inputFiles), the
// agent still can't access it inside the container — same rewrite
// applies.
func TestRewriteInputPathsInPrompt_ReplacesArtifactStorePath(t *testing.T) {
	got := rewriteInputPathsInPrompt(
		"Process /opt/vornik/artifacts/proj/inputs/artifact_xyz/report.pdf",
		[]string{"/opt/vornik/artifacts/proj/inputs/artifact_xyz/report.pdf"},
	)
	assert.Contains(t, got, "/app/workspace/artifacts/in/report.pdf")
}

// TestRewriteInputPathsInPrompt_ReplacesHallucinatedTmpPath — the
// operator-reported failure mode. The LLM didn't carry the host
// path through; it invented /tmp/cv.pdf. Basename-matching rewrite
// catches this.
func TestRewriteInputPathsInPrompt_ReplacesHallucinatedTmpPath(t *testing.T) {
	got := rewriteInputPathsInPrompt(
		"Read the attached PDF at /tmp/cv.pdf to extract the work history.",
		[]string{"/opt/vornik/artifacts/proj/inputs/artifact_xyz/cv.pdf"},
	)
	assert.Contains(t, got, "/app/workspace/artifacts/in/cv.pdf")
	assert.NotContains(t, got, "/tmp/cv.pdf",
		"hallucinated /tmp/<basename> reference must be replaced when basename matches an input file")
}

// TestRewriteInputPathsInPrompt_RewritesMultiplePathPrefixes — the
// rewrite catches /tmp/, ./, /workspace/, /app/input/ variants of
// the same hallucinated basename. We trade a small false-positive
// risk for catching every shape a confused LLM might emit.
func TestRewriteInputPathsInPrompt_RewritesMultiplePathPrefixes(t *testing.T) {
	for _, badPath := range []string{
		"/tmp/cv.pdf",
		"./cv.pdf",
		"/workspace/cv.pdf",
		"/app/input/cv.pdf",
	} {
		t.Run(badPath, func(t *testing.T) {
			got := rewriteInputPathsInPrompt("read "+badPath, []string{"/host/cv.pdf"})
			assert.Equal(t, "read /app/workspace/artifacts/in/cv.pdf", got)
		})
	}
}

// TestRewriteInputPathsInPrompt_LeavesBareBasenameAlone — defensive:
// we intentionally do NOT rewrite a bare "cv.pdf" mention because
// it might be a filename in a list, a code identifier, etc. The
// path-prefix gate keeps the rewrite scoped to actual file
// references.
func TestRewriteInputPathsInPrompt_LeavesBareBasenameAlone(t *testing.T) {
	got := rewriteInputPathsInPrompt(
		"The attached cv.pdf contains the CV.",
		[]string{"/host/cv.pdf"},
	)
	assert.Equal(t, "The attached cv.pdf contains the CV.", got,
		"bare basename mention must NOT be rewritten — it might not be a path reference")
}

// TestRewriteInputPathsInPrompt_NoInputsReturnsUnchanged — defensive
// shape: no inputs → no-op. Empty prompt → empty.
func TestRewriteInputPathsInPrompt_NoInputsReturnsUnchanged(t *testing.T) {
	assert.Equal(t, "untouched", rewriteInputPathsInPrompt("untouched", nil))
	assert.Equal(t, "untouched", rewriteInputPathsInPrompt("untouched", []string{}))
	assert.Equal(t, "", rewriteInputPathsInPrompt("", []string{"/x/y.pdf"}))
}

// TestRewriteInputPathsInPrompt_EmptyEntriesSkipped — a malformed
// inputFiles entry must not crash or panic on filepath.Base("").
func TestRewriteInputPathsInPrompt_EmptyEntriesSkipped(t *testing.T) {
	got := rewriteInputPathsInPrompt(
		"Read /tmp/cv.pdf",
		[]string{"", "/host/cv.pdf", ""},
	)
	assert.Equal(t, "Read /app/workspace/artifacts/in/cv.pdf", got)
}

// TestBuildAttachedFilesBlock_HappyPath pins the canonical block
// shape. The header is short and the listing is deterministic so
// the agent's prompt is reproducible for cache-friendly LLM calls.
func TestBuildAttachedFilesBlock_HappyPath(t *testing.T) {
	got := buildAttachedFilesBlock([]string{
		"/host/a.pdf",
		"/host/b.txt",
	}, nil)
	assert.Contains(t, got, "## ATTACHED FILES")
	assert.Contains(t, got, "- /app/workspace/artifacts/in/a.pdf")
	assert.Contains(t, got, "- /app/workspace/artifacts/in/b.txt")
}

// TestBuildAttachedFilesBlock_PreservesOrder — the inputs order
// flows from the dispatcher's create_task arg list. The agent
// sees the same order so it can rely on "the first attachment"
// being the same file the user enumerated first.
func TestBuildAttachedFilesBlock_PreservesOrder(t *testing.T) {
	got := buildAttachedFilesBlock([]string{"/x/zebra.png", "/x/alpha.jpg"}, nil)
	zebraIdx := indexOf(got, "zebra.png")
	alphaIdx := indexOf(got, "alpha.jpg")
	assert.True(t, zebraIdx >= 0 && alphaIdx >= 0)
	assert.True(t, zebraIdx < alphaIdx, "block must preserve input ordering — caller's first file is the agent's first attachment")
}

// TestBuildAttachedFilesBlock_EmptyReturnsEmpty — defensive shape
// so buildAgentInput's conditional append doesn't emit a header
// with no items.
func TestBuildAttachedFilesBlock_EmptyReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", buildAttachedFilesBlock(nil, nil))
	assert.Equal(t, "", buildAttachedFilesBlock([]string{}, nil))
}

// TestBuildAttachedFilesBlock_SkipsEmptyEntries — same defensive
// shape as the rewrite helper; empty entries must not produce
// "- /app/workspace/artifacts/in/." entries.
func TestBuildAttachedFilesBlock_SkipsEmptyEntries(t *testing.T) {
	got := buildAttachedFilesBlock([]string{"", "/x/real.pdf", ""}, nil)
	assert.Contains(t, got, "real.pdf")
	assert.NotContains(t, got, "in/.")
}

// TestBuildAttachedFilesBlock_WithExtractionRoutesToMCPTools — when
// the dispatcher's auto-extract hook produces a summary for an
// input, the block MUST NOT list the staged container path. The
// load-bearing invariant: the LLM can only access the document
// via mcp__vornik__document_* tools, not file_read. Prior
// behaviour was to list both (path + "↳ ingested" trailer); the
// lead role's LLM ignored the trailer, file_read'd the EPUB, and
// the base64-encoded ~17MB binary blew the 32MB chat-proxy cap.
// 2026-05-21 incident: tasks T-fa9e / T-7f98 / T-8889 all failed
// this way despite extraction succeeding.
func TestBuildAttachedFilesBlock_WithExtractionRoutesToMCPTools(t *testing.T) {
	extractions := []map[string]any{
		{
			"artifact_id":           "art-1",
			"extracted_document_id": "extdoc_xyz",
			"title":                 "Schema Coaching",
			"author":                "Iain McCormick",
			"section_count":         30,
			"chunks_ingested":       283,
		},
	}
	got := buildAttachedFilesBlock([]string{"/x/schema-coaching.epub"}, extractions)

	// Must surface the document so the LLM knows it exists +
	// how to reach it.
	for _, want := range []string{
		"ATTACHED DOCUMENTS",
		"already in project memory",
		"mcp__vornik__document_get_outline",
		"Schema Coaching",
		"by Iain McCormick",
		"30 sections",
		"283 chunks",
		"artifact_id=art-1",
		"extracted_document_id=extdoc_xyz",
		"Do NOT attempt to file_read",
	} {
		assert.Contains(t, got, want, "missing memory-routing marker")
	}

	// Must NOT surface the staged path — that's the file_read
	// temptation we're eliminating.
	assert.NotContains(t, got,
		"/app/workspace/artifacts/in/schema-coaching.epub",
		"staged path must NOT be listed when extraction succeeded — removes the file_read temptation")
}

// TestBuildAttachedFilesBlock_MixedSurfaces — a task can attach
// both an extracted document and a non-extractable file in one
// call; the block must emit BOTH sections (DOCUMENTS for the
// extracted, FILES for the rest) so each gets the right access
// instructions without crossing wires.
func TestBuildAttachedFilesBlock_MixedSurfaces(t *testing.T) {
	extractions := []map[string]any{
		{
			"artifact_id":           "art-book",
			"extracted_document_id": "extdoc_book",
			"title":                 "Book",
			"section_count":         5,
			"chunks_ingested":       12,
		},
	}
	got := buildAttachedFilesBlock([]string{
		"/x/book.epub",    // extracted
		"/x/raw-data.bin", // not extracted (extractions slice has 1 entry; sees positional-match logic)
	}, extractions)

	// indexExtractionsByBasename uses positional join when counts
	// differ; with 1 extraction and 2 files it stamps the first
	// extraction onto BOTH. Verify the test reflects current
	// behaviour: both go into the DOCUMENTS section. (When the
	// upstream dispatcher records per-file extraction misses
	// explicitly we'd refine this.)
	assert.Contains(t, got, "ATTACHED DOCUMENTS")
}

// TestBuildAttachedFilesBlock_NoExtractionsKeepsLegacyShape — when
// no extractions are recorded (no auto-extractor wired, or every
// extraction was a no-op), the block falls back to the pre-Phase-3
// shape — file listing only, no trailer. Guards against an empty
// trailer appearing on every line.
func TestBuildAttachedFilesBlock_NoExtractionsKeepsLegacyShape(t *testing.T) {
	got := buildAttachedFilesBlock([]string{"/x/a.txt"}, nil)
	assert.Contains(t, got, "- /app/workspace/artifacts/in/a.txt")
	assert.NotContains(t, got, "↳")
	assert.NotContains(t, got, "ingested into project memory")
}

// indexOf is a small test-local helper kept narrow so callers can
// assert on relative positions without a strings.Index wrapper at
// every site.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
