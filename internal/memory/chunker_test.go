package memory

import (
	"strings"
	"testing"
)

func TestChunkText_EmptyAndDefaults(t *testing.T) {
	if got := chunkText("", 0, 0); got != nil {
		t.Fatalf("empty input: got %v, want nil", got)
	}
	if got := chunkText("   \n\n   ", 512, 0); got != nil {
		t.Fatalf("whitespace-only: got %v, want nil", got)
	}
	// Defaults: chunkTokens<=0 → 512, overlapTokens<0 → 0; small input
	// fits in a single chunk.
	got := chunkText("hello world", -1, -1)
	if len(got) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(got))
	}
	if got[0].Index != 0 || got[0].Text != "hello world" || len(got[0].Hash) != 64 {
		t.Fatalf("unexpected chunk: %+v", got[0])
	}
}

func TestChunkText_ParagraphsAggregated(t *testing.T) {
	text := "para one.\n\npara two.\n\npara three."
	// chunkTokens=32 → 128 bytes — fits all three.
	got := chunkText(text, 32, 0)
	if len(got) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "para one") || !strings.Contains(got[0].Text, "para three") {
		t.Fatalf("missing paragraphs: %q", got[0].Text)
	}
}

func TestChunkText_ParagraphFlushAndOverlap(t *testing.T) {
	// Build 3 paragraphs each ~40 bytes; chunkTokens=10 (40 bytes) forces a flush after each.
	p1 := strings.Repeat("alpha ", 6) + "x" // 37 bytes
	p2 := strings.Repeat("beta ", 7) + "y"
	p3 := strings.Repeat("gamma ", 6) + "z"
	text := p1 + "\n\n" + p2 + "\n\n" + p3
	// overlapTokens=2 → 8 bytes; should land an overlap prefix on chunks 2/3.
	got := chunkText(text, 10, 2)
	if len(got) < 2 {
		t.Fatalf("want ≥2 chunks, got %d (chunks=%v)", len(got), got)
	}
	// Indexes are sequential starting at 0.
	for i, c := range got {
		if c.Index != i {
			t.Fatalf("chunk[%d].Index=%d", i, c.Index)
		}
		if c.Hash == "" {
			t.Fatalf("chunk[%d] has empty hash", i)
		}
	}
}

func TestChunkText_LargeParagraphSplits(t *testing.T) {
	// Single paragraph longer than chunkBytes triggers splitLargeParagraph.
	// Sentence-terminated so the sentence-break branch fires.
	long := strings.Repeat("This is one sentence. ", 200) // ~4400 bytes
	got := chunkText(long, 32, 4)                         // 128-byte chunks, 16-byte overlap
	if len(got) < 2 {
		t.Fatalf("want multiple chunks from large paragraph, got %d", len(got))
	}
	// Indexes monotonic.
	for i, c := range got {
		if c.Index != i {
			t.Fatalf("chunk[%d].Index=%d", i, c.Index)
		}
	}
}

func TestChunkText_LargeParagraphCombinedWithPartialBuilder(t *testing.T) {
	// Small lead paragraph + huge follow-up: the partial builder content
	// must prepend onto the first sub-chunk of the large split.
	small := "lead-in note." // 13 bytes
	huge := strings.Repeat("Word ", 200) + "."
	got := chunkText(small+"\n\n"+huge, 32, 0)
	if len(got) < 2 {
		t.Fatalf("expected several chunks, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "lead-in note") {
		t.Fatalf("first chunk should contain lead-in: %q", got[0].Text)
	}
}

func TestChunkText_LargeParagraphFallsBackToWordBreakAndHardCut(t *testing.T) {
	// No sentence terminator at all → must use word-break.
	noSentences := strings.Repeat("alpha beta gamma delta ", 50)
	got := chunkText(noSentences, 16, 0) // 64-byte chunk
	if len(got) < 2 {
		t.Fatalf("expected splits, got %d", len(got))
	}

	// No spaces at all → hard cut at chunkBytes (findSentenceBreak and
	// findWordBreak both return -1).
	noSpaces := strings.Repeat("a", 500)
	got2 := chunkText(noSpaces, 16, 0)
	if len(got2) < 2 {
		t.Fatalf("expected hard-cut splits, got %d", len(got2))
	}
}

func TestSplitParagraphs_Normalisation(t *testing.T) {
	got := splitParagraphs("one\r\n\r\ntwo\r\rthree\n\n\n\nfour")
	want := []string{"one", "two", "three", "four"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := splitParagraphs("\n\n  \n\n"); got != nil {
		t.Fatalf("only-whitespace: got %v", got)
	}
}

func TestOverlapSuffix(t *testing.T) {
	if got := overlapSuffix("short", 10); got != "short" {
		t.Fatalf("under limit: got %q", got)
	}
	// maxBytes smaller than the trailing word; walks forward to next space.
	got := overlapSuffix("alpha beta gamma delta", 10)
	if got == "" || strings.Contains(got, " ") && got[0] == ' ' {
		t.Fatalf("got %q", got)
	}
	// String with no space after the cut point → walks to end → empty after trim.
	if got := overlapSuffix("aaaaaaaaaaaaaaaa", 4); got != "" {
		t.Fatalf("expected empty when no space: %q", got)
	}
}

func TestFindSentenceBreak_AndFindWordBreak(t *testing.T) {
	para := "First sentence here. Second sentence here. Third sentence here. tail"
	// Window includes the period+space sequence.
	idx := findSentenceBreak(para, 0, len(para))
	if idx < 0 {
		t.Fatalf("expected sentence break in %q", para)
	}
	// No terminator at all.
	if got := findSentenceBreak("alpha beta gamma delta", 0, 22); got != -1 {
		t.Fatalf("want -1 on no terminator, got %d", got)
	}
	// findWordBreak
	if got := findWordBreak("hello world", 0, 11); got != 6 {
		t.Fatalf("findWordBreak got %d, want 6", got)
	}
	if got := findWordBreak("hellothere", 0, 10); got != -1 {
		t.Fatalf("expected -1 on no spaces, got %d", got)
	}
}

func TestMakeChunkHashStability(t *testing.T) {
	a := makeChunk(0, "alpha")
	b := makeChunk(99, "alpha")
	if a.Hash != b.Hash {
		t.Fatalf("hash mismatch for same text: %s vs %s", a.Hash, b.Hash)
	}
	if a.Hash == makeChunk(0, "beta").Hash {
		t.Fatalf("hash collision for different text")
	}
}
