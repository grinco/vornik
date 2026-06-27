package memory

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Chunk is a text fragment produced by the chunker.
type Chunk struct {
	// Index is the zero-based position of this chunk within the source text.
	Index int

	// Text is the chunk content.
	Text string

	// Hash is the SHA-256 hex digest of Text, used for deduplication.
	Hash string
}

// chunkText splits text into overlapping chunks respecting paragraph boundaries
// where possible. chunkTokens and overlapTokens are expressed in approximate
// token counts (1 token ≈ 4 chars).
func chunkText(text string, chunkTokens, overlapTokens int) []Chunk {
	if chunkTokens <= 0 {
		chunkTokens = 512
	}
	if overlapTokens < 0 {
		overlapTokens = 0
	}
	// Convert from tokens to approximate byte counts.
	chunkBytes := chunkTokens * 4
	overlapBytes := overlapTokens * 4

	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Split the source text into paragraphs (double newline boundaries).
	paragraphs := splitParagraphs(text)

	var chunks []Chunk
	var current strings.Builder
	index := 0

	flush := func() {
		s := strings.TrimSpace(current.String())
		if s == "" {
			return
		}
		chunks = append(chunks, makeChunk(index, s))
		index++
		current.Reset()
	}

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		// If adding this paragraph would exceed the chunk size and the current
		// builder already has content, flush first.
		projected := current.Len() + len(para) + 2 // +2 for "\n\n"
		if projected > chunkBytes && current.Len() > 0 {
			flush()

			// Begin the next chunk with the overlap from the end of the
			// previous text so context carries across chunk boundaries.
			if overlapBytes > 0 && len(chunks) > 0 {
				prev := chunks[len(chunks)-1].Text
				overlap := overlapSuffix(prev, overlapBytes)
				if overlap != "" {
					current.WriteString(overlap)
					current.WriteString("\n\n")
				}
			}
		}

		// If a single paragraph is larger than chunkBytes, split it by sentence/word.
		if len(para) > chunkBytes {
			subChunks := splitLargeParagraph(para, chunkBytes, overlapBytes, index)
			// Any partial content in current is prepended to the first sub-chunk.
			if current.Len() > 0 && len(subChunks) > 0 {
				subChunks[0] = makeChunk(subChunks[0].Index, strings.TrimSpace(current.String())+"\n\n"+subChunks[0].Text)
				current.Reset()
			}
			for _, sc := range subChunks {
				sc.Index = index
				index++
				chunks = append(chunks, sc)
			}
			continue
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	flush()
	return chunks
}

// splitParagraphs splits text at double-newline boundaries.
func splitParagraphs(text string) []string {
	// Normalise line endings.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	// Split on two or more consecutive newlines.
	var parts []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// overlapSuffix returns up to maxBytes from the end of s, breaking at a word
// boundary where possible.
func overlapSuffix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	// Walk forward to the next space to avoid cutting in the middle of a word.
	for start < len(s) && s[start] != ' ' && s[start] != '\n' {
		start++
	}
	return strings.TrimSpace(s[start:])
}

// splitLargeParagraph splits a paragraph that is bigger than chunkBytes.
// It tries to break at sentence endings first, then at spaces.
func splitLargeParagraph(para string, chunkBytes, overlapBytes, startIndex int) []Chunk {
	var chunks []Chunk
	idx := startIndex
	pos := 0
	for pos < len(para) {
		end := pos + chunkBytes
		if end >= len(para) {
			// Last chunk in the range — no idx increment needed
			// because we break out of the loop immediately.
			s := strings.TrimSpace(para[pos:])
			if s != "" {
				chunks = append(chunks, makeChunk(idx, s))
			}
			break
		}

		// Try to find a sentence boundary (. ! ?) within the last 20% of the chunk.
		breakAt := findSentenceBreak(para, pos, end)
		if breakAt <= pos {
			// Fall back to word boundary.
			breakAt = findWordBreak(para, pos, end)
		}
		if breakAt <= pos {
			breakAt = end
			// bug sweep 2026-06-04: with no whitespace in the window
			// (CJK prose, base64, minified JSON, a long URL) breakAt is
			// a raw byte offset that can fall mid-rune. Slicing there
			// yields invalid UTF-8, which Postgres rejects — aborting
			// the whole artifact's ingest. Snap back to a rune boundary.
			for breakAt > pos+1 && !utf8.RuneStart(para[breakAt]) {
				breakAt--
			}
		}

		s := strings.TrimSpace(para[pos:breakAt])
		if s != "" {
			chunks = append(chunks, makeChunk(idx, s))
			idx++
		}

		// Advance with overlap so context carries across the boundary.
		// bug sweep 2026-06-04: the old guard compared the stepped-back
		// position against breakAt, so for any non-empty overlap
		// (always <= breakAt) it reset pos = breakAt and discarded the
		// overlap entirely. Guard against non-advancement past the
		// *current* chunk start instead, and snap to a rune boundary so
		// the next chunk never begins mid-rune.
		if overlapBytes > 0 && len(chunks) > 0 {
			prev := chunks[len(chunks)-1].Text
			overlap := overlapSuffix(prev, overlapBytes)
			newPos := breakAt - len(overlap)
			if newPos <= pos {
				newPos = breakAt
			}
			for newPos < breakAt && !utf8.RuneStart(para[newPos]) {
				newPos++
			}
			pos = newPos
		} else {
			pos = breakAt
		}
	}
	return chunks
}

// findSentenceBreak looks backwards from end in para[pos:end] for a sentence
// terminator followed by a space. Returns the position after the terminator.
func findSentenceBreak(para string, pos, end int) int {
	window := para[pos:end]
	searchFrom := len(window) * 4 / 5 // look in the last 20%
	if searchFrom < 0 {
		searchFrom = 0
	}
	best := -1
	for i := len(window) - 1; i >= searchFrom; i-- {
		c := window[i]
		if c == '.' || c == '!' || c == '?' {
			if i+1 < len(window) && (window[i+1] == ' ' || window[i+1] == '\n') {
				if best < 0 || i > best {
					best = i + 2 // include the space
				}
			}
		}
	}
	if best < 0 {
		return -1
	}
	return pos + best
}

// findWordBreak looks backwards from end for the last space in para.
func findWordBreak(para string, pos, end int) int {
	for i := end; i > pos; i-- {
		if para[i-1] == ' ' || para[i-1] == '\n' {
			return i
		}
	}
	return -1
}

// makeChunk creates a Chunk with the given index and text, computing its hash.
func makeChunk(index int, text string) Chunk {
	// Final guard: never emit a chunk with invalid UTF-8. A mid-rune
	// byte slice upstream would otherwise reach the TEXT column and
	// fail the whole ingest (bug sweep 2026-06-04). ToValidUTF8 is a
	// no-op for already-valid input, so this costs nothing on the
	// common path.
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	h := sha256.Sum256([]byte(text))
	return Chunk{
		Index: index,
		Text:  text,
		Hash:  fmt.Sprintf("%x", h),
	}
}
