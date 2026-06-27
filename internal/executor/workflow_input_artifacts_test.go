// Tests for the 2026-05-16 multi-step-attachment fix.
//
// Pre-fix: task.Payload.context.inputFiles was extracted ONCE into
// stepArtifacts, then on every step transition stepArtifacts was
// REPLACED with the previous step's output artifacts. The user's
// uploaded PDF reached the researcher's container but vanished
// from the writer's because the executor never re-staged it.
//
// Post-fix: the extraction is a pure helper called once before the
// loop, and stepArtifacts is rebuilt as (task inputs ⊕ step output)
// on every transition. These tests pin the helper's contract so the
// re-merge code in executeWorkflowAttempt stays honest under refactor.

package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExtractTaskInputArtifacts_HappyPath is the canonical case:
// a Telegram-uploaded file lands in task.Payload after the
// dispatcher snapshots it into the durable artifact store. The
// extractor turns the path list into the {name, sourcePath} shape
// container.go's staging code consumes.
func TestExtractTaskInputArtifacts_HappyPath(t *testing.T) {
	payload := []byte(`{"context":{"inputFiles":["/opt/vornik/artifacts/assistant/inputs/artifact_xyz/cv.pdf"]}}`)
	got := extractTaskInputArtifacts(payload)
	assert.Equal(t, []map[string]string{
		{"name": "cv.pdf", "sourcePath": "/opt/vornik/artifacts/assistant/inputs/artifact_xyz/cv.pdf"},
	}, got)
}

// TestExtractTaskInputArtifacts_MultipleFiles — two attachments
// preserve order so the operator's mental model ("attach in this
// order") matches what reaches the agent's input dir.
func TestExtractTaskInputArtifacts_MultipleFiles(t *testing.T) {
	payload := []byte(`{"context":{"inputFiles":["/a/first.png","/b/second.jpg"]}}`)
	got := extractTaskInputArtifacts(payload)
	assert.Len(t, got, 2)
	assert.Equal(t, "first.png", got[0]["name"])
	assert.Equal(t, "second.jpg", got[1]["name"])
}

// TestExtractTaskInputArtifacts_NilPayloadReturnsNil — defensive:
// older API callers may construct tasks without a payload at all.
// Must not crash; nil result lets the caller's append act as a
// no-op.
func TestExtractTaskInputArtifacts_NilPayloadReturnsNil(t *testing.T) {
	assert.Nil(t, extractTaskInputArtifacts(nil))
	assert.Nil(t, extractTaskInputArtifacts([]byte{}))
}

// TestExtractTaskInputArtifacts_MalformedJSONReturnsNil — best-
// effort: a malformed payload (e.g. truncated by a buggy caller)
// must not stop the workflow. The agent runs anyway and just
// doesn't see the inputs.
func TestExtractTaskInputArtifacts_MalformedJSONReturnsNil(t *testing.T) {
	assert.Nil(t, extractTaskInputArtifacts([]byte(`{not json`)))
}

// TestExtractTaskInputArtifacts_NoInputFilesField — when the
// payload exists but has no inputFiles key (the common shape for
// tasks created without attachments), the extractor returns nil
// so the caller's append is a no-op.
func TestExtractTaskInputArtifacts_NoInputFilesField(t *testing.T) {
	assert.Nil(t, extractTaskInputArtifacts([]byte(`{"context":{"prompt":"do thing"}}`)))
}

// TestExtractTaskInputArtifacts_EmptyStringsFiltered — defensive:
// upstream code may pass through an empty path (race / partial
// failure during snapshotting). Skip it rather than emit a
// {"name": ".", "sourcePath": ""} entry that container.go
// would silently drop with a "src empty" log.
func TestExtractTaskInputArtifacts_EmptyStringsFiltered(t *testing.T) {
	payload := []byte(`{"context":{"inputFiles":["","/real.txt",""]}}`)
	got := extractTaskInputArtifacts(payload)
	assert.Len(t, got, 1)
	assert.Equal(t, "real.txt", got[0]["name"])
}

// TestExtractTaskInputArtifacts_EmptyListReturnsNil — explicit
// inputFiles:[] is the same shape as no inputFiles field. Return
// nil so the caller's append doesn't reserve capacity for nothing.
func TestExtractTaskInputArtifacts_EmptyListReturnsNil(t *testing.T) {
	assert.Nil(t, extractTaskInputArtifacts([]byte(`{"context":{"inputFiles":[]}}`)))
}
