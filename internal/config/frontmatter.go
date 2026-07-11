package config

import (
	"bytes"
	"errors"
	"fmt"
)

// EditFrontmatter applies a YAML edit to a markdown document's leading
// `---`-fenced frontmatter block, leaving everything outside the fences
// byte-identical (LLD 2026-07-11-control-plane-actionable-proposals §4.2).
// Fence semantics mirror the registry's splitFrontmatter so any workflow /
// swarm file the daemon loads is editable here: a UTF-8 BOM and leading
// whitespace before the opening `---` are tolerated (and preserved on
// output); the closing fence is the next line whose trimmed content is
// exactly `---`.
//
// The edit callback receives the frontmatter YAML without the fences and
// returns the replacement; its error propagates unchanged. A document with
// no well-formed fence returns ErrNoFrontmatter — callers (the actionizer)
// degrade to an informational proposal rather than guessing at structure.

// ErrNoFrontmatter means the document has no leading `---`-fenced block.
var ErrNoFrontmatter = errors.New("config: document has no frontmatter fence")

var fmMarker = []byte("---")

// EditFrontmatter splits doc, applies edit to the frontmatter YAML, and
// reassembles the document.
func EditFrontmatter(doc []byte, edit func(frontmatter []byte) ([]byte, error)) ([]byte, error) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	rest := bytes.TrimPrefix(doc, bom)
	rest = bytes.TrimLeft(rest, " \t\r\n")
	prefix := doc[:len(doc)-len(rest)] // BOM + leading whitespace, preserved verbatim

	if !bytes.HasPrefix(rest, fmMarker) {
		return nil, ErrNoFrontmatter
	}
	afterOpen := rest[len(fmMarker):]
	if len(afterOpen) == 0 || (afterOpen[0] != '\n' && afterOpen[0] != '\r') {
		return nil, ErrNoFrontmatter // opening marker not on its own line
	}
	// Consume the opening marker's line ending.
	nl := bytes.IndexByte(afterOpen, '\n')
	if nl < 0 {
		return nil, ErrNoFrontmatter
	}
	afterOpen = afterOpen[nl+1:]

	// Find the closing fence: the next line whose trimmed content is `---`.
	offset := 0
	for offset < len(afterOpen) {
		lineEnd := bytes.IndexByte(afterOpen[offset:], '\n')
		var line []byte
		next := len(afterOpen)
		if lineEnd >= 0 {
			line = afterOpen[offset : offset+lineEnd]
			next = offset + lineEnd + 1
		} else {
			line = afterOpen[offset:]
		}
		if bytes.Equal(bytes.TrimSpace(line), fmMarker) {
			frontmatter := afterOpen[:offset]
			body := afterOpen[next:]
			edited, err := edit(frontmatter)
			if err != nil {
				return nil, fmt.Errorf("config: frontmatter edit: %w", err)
			}
			if len(edited) > 0 && edited[len(edited)-1] != '\n' {
				edited = append(edited, '\n')
			}
			var out bytes.Buffer
			out.Grow(len(prefix) + len(edited) + len(body) + 8)
			out.Write(prefix)
			out.WriteString("---\n")
			out.Write(edited)
			out.WriteString("---\n")
			out.Write(body)
			return out.Bytes(), nil
		}
		offset = next
	}
	return nil, ErrNoFrontmatter // no closing fence
}
