package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractProseFromResult_PullsKnownFields — the canonical
// path: result.json with a top-level "message" + "summary"
// returns both concatenated. This is what the executor's
// hallucination detector now scans, instead of the raw bytes.
func TestExtractProseFromResult_PullsKnownFields(t *testing.T) {
	in := []byte(`{
		"message": "Scraped 12 listings from https://example.com.",
		"summary": "All within the requested filter window.",
		"outputArtifacts": [{"name":"x.md","path":"out/x.md"}]
	}`)
	got := extractProseFromResult(in)
	assert.Contains(t, got, "Scraped 12 listings")
	assert.Contains(t, got, "All within the requested filter window")
	assert.NotContains(t, got, "outputArtifacts",
		"structured fields must be excluded — they're how v1 produced false positives")
	assert.NotContains(t, got, "out/x.md",
		"nested artifact paths must NOT leak into the prose blob")
}

// TestExtractProseFromResult_IgnoresEmbeddedToolHistory —
// production false-positive shape: the model dumped a
// JSON-escaped serialisation of past tool calls inside its
// own response text. The v1 detector saw nested URLs with
// trailing `\` characters and flagged every one. The fix is
// to NOT scan structured fields like nested objects or arrays
// of objects, and to only follow string-typed prose-bearing
// fields.
func TestExtractProseFromResult_IgnoresEmbeddedToolHistory(t *testing.T) {
	in := []byte(`{
		"message": "Done.",
		"toolCalls": [
			{"name":"web_fetch","input":"{\"url\":\"https://embedded.example/news\"}"}
		]
	}`)
	got := extractProseFromResult(in)
	assert.Contains(t, got, "Done.")
	assert.NotContains(t, got, "https://embedded.example/news",
		"URLs nested inside tool-history serialisations must not appear in the scan corpus")
}

// TestExtractProseFromResult_AcceptsArrayOfStrings — some
// roles emit `final_text: ["para 1", "para 2"]`. The
// extractor must concatenate each string entry; non-string
// elements are skipped.
func TestExtractProseFromResult_AcceptsArrayOfStrings(t *testing.T) {
	in := []byte(`{"final_text":["first paragraph.","second paragraph."]}`)
	got := extractProseFromResult(in)
	assert.Contains(t, got, "first paragraph")
	assert.Contains(t, got, "second paragraph")
}

// TestExtractProseFromResult_MalformedJSONReturnsEmpty —
// degrading to "no scan" on parse failure is the safe choice
// (no signals beats false-positive noise burst). A malformed
// result.json is itself a downstream failure surfaced
// elsewhere in the pipeline; the detector doesn't need to
// pile on.
func TestExtractProseFromResult_MalformedJSONReturnsEmpty(t *testing.T) {
	got := extractProseFromResult([]byte(`{not valid json`))
	assert.Equal(t, "", got)
}

// TestExtractProseFromResult_NoProseKeysReturnsEmpty — a
// well-formed result.json with no recognised prose field is
// also empty. Better to miss a future field-name addition than
// to scan unstructured noise (the v1 trap).
func TestExtractProseFromResult_NoProseKeysReturnsEmpty(t *testing.T) {
	got := extractProseFromResult([]byte(`{"status":"COMPLETED","outputArtifacts":[]}`))
	assert.Equal(t, "", got, "no prose-bearing field → empty corpus, not raw scan")
}

// TestExtractProseFromResult_StripsLeadingTrailingWhitespace —
// produced text has trailing newlines (one per appended
// field). Just ensure the basic concatenation doesn't drop
// content; trimming is the caller's concern (rules are
// whitespace-tolerant).
func TestExtractProseFromResult_StripsLeadingTrailingWhitespace(t *testing.T) {
	in := []byte(`{"message":"hello","summary":"world"}`)
	got := extractProseFromResult(in)
	got = strings.TrimSpace(got)
	require.NotEmpty(t, got)
	assert.Equal(t, "hello\nworld", got)
}
