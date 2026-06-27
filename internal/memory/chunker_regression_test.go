package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestChunkText_MultibyteHardCut_StaysValidUTF8 is the regression for
// the 2026-06-04 bug sweep: when a paragraph had no ASCII whitespace in
// the chunk window (CJK prose, base64, minified JSON, a long URL), the
// splitter hard-cut at a raw byte offset that fell mid-rune, producing
// invalid UTF-8. Postgres rejects invalid UTF-8 in a TEXT column, so
// UpsertChunks errored and the entire artifact's chunks were lost.
func TestChunkText_MultibyteHardCut_StaysValidUTF8(t *testing.T) {
	// 3-byte runes, no spaces, well over the chunk size so the
	// sentence- and word-break branches both fail and the hard cut
	// fires repeatedly.
	src := strings.Repeat("你好世界漢字", 100) // 1800 bytes, all multibyte
	got := chunkText(src, 16, 4)         // 64-byte chunks, 16-byte overlap

	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	for i, c := range got {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk[%d] is not valid UTF-8: %q", i, c.Text)
		}
		if strings.ContainsRune(c.Text, '�') {
			t.Fatalf("chunk[%d] contains the replacement rune — a rune was sliced: %q", i, c.Text)
		}
	}
}

// TestChunkText_LargeParagraphOverlapIsApplied is the regression for
// the inverted overlap guard: splitLargeParagraph computed
// pos = breakAt - len(overlap) then `if pos < breakAt { pos = breakAt }`,
// which is always true for any non-empty overlap, so the overlap was
// silently discarded and consecutive chunks shared no context.
func TestChunkText_LargeParagraphOverlapIsApplied(t *testing.T) {
	// One paragraph, no sentence terminators, well over chunk size so
	// splitLargeParagraph drives the word-break + overlap path.
	src := strings.Repeat("alpha beta gamma delta epsilon ", 60)
	got := chunkText(src, 16, 8) // 64-byte chunk, 32-byte overlap

	if len(got) < 3 {
		t.Fatalf("expected several chunks, got %d", len(got))
	}

	maxShared := 0
	for i := 0; i+1 < len(got); i++ {
		if n := sharedBoundary(got[i].Text, got[i+1].Text); n > maxShared {
			maxShared = n
		}
	}
	if maxShared == 0 {
		t.Fatal("no consecutive chunks share boundary text — overlap was discarded")
	}
}

// TestMakeChunk_SanitisesInvalidUTF8 covers the defensive net added in
// makeChunk: even if an upstream slice produced invalid UTF-8, the
// emitted chunk text is always valid (so it can never fail the TEXT
// column on ingest).
func TestMakeChunk_SanitisesInvalidUTF8(t *testing.T) {
	// A valid rune truncated to its first byte — invalid UTF-8.
	bad := "ok-" + string([]byte{0xe4, 0xbd}) // first 2 bytes of a 3-byte rune
	c := makeChunk(0, bad)
	if !utf8.ValidString(c.Text) {
		t.Fatalf("makeChunk must emit valid UTF-8, got %q", c.Text)
	}
}

// sharedBoundary returns the length of the longest suffix of a that is
// also a prefix of b.
func sharedBoundary(a, b string) int {
	max := len(a)
	if len(b) < max {
		max = len(b)
	}
	for n := max; n > 0; n-- {
		if a[len(a)-n:] == b[:n] {
			return n
		}
	}
	return 0
}
