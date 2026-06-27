package registry

// Shared helpers for the Markdown + YAML-frontmatter authoring
// primitives. Both workflow_md.go and swarm_md.go consume the
// same split + section-walker so the rules an operator learns
// for one format apply identically to the other.

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// frontmatterMarker is the line that opens and closes the YAML
// frontmatter block. Three hyphens, no trailing whitespace —
// matches the Jekyll / Hugo / SKILL.md convention.
const frontmatterMarker = "---"

// scannerInitial is the bufio.Scanner's initial buffer size, and
// scannerMax is its growth ceiling. The default 64 KiB cap is too
// small for prompt-heavy frontmatter blocks; 4 MiB is comfortably
// above any real-world file while still bounding a runaway scan.
const (
	scannerInitial = 1 << 20
	scannerMax     = 4 << 20
)

// utf8BOM is the byte sequence that some editors / copy-paste
// surfaces prepend to a file. Stripped before any other parsing.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// splitFrontmatter peels the leading `---`-delimited block off
// the file. Returns the YAML bytes (without the surrounding
// markers) and the body bytes (everything after the closing
// marker). Tolerates a UTF-8 BOM and leading whitespace before
// the opening marker.
//
// The kind argument names the file type in error messages
// ("WORKFLOW.md" / "swarm" / …) so operators can spot which
// loader rejected a file without grepping for the parser.
func splitFrontmatter(content []byte, kind, filename string) (frontmatter, body []byte, err error) {
	// Strip UTF-8 BOM if present, then leading whitespace. The BOM
	// is a fixed 3-byte prefix; trim by bytes to avoid Go's
	// "invalid BOM in the middle of the file" parser check that
	// kicks in when the rune literal is used in source.
	content = bytes.TrimPrefix(content, utf8BOM)
	trimmed := bytes.TrimLeft(content, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(frontmatterMarker)) {
		return nil, nil, fmt.Errorf("%s %s: missing opening frontmatter marker (file must start with '---')", kind, filename)
	}
	rest := bytes.TrimPrefix(trimmed, []byte(frontmatterMarker))
	if len(rest) == 0 || (rest[0] != '\n' && rest[0] != '\r') {
		return nil, nil, fmt.Errorf("%s %s: opening frontmatter marker must be on its own line", kind, filename)
	}

	var (
		fmBuf      bytes.Buffer
		bodyOffset = -1
		consumed   = 0 // bytes scanned through the rest buffer
	)
	for consumed < len(rest) {
		nextNL := bytes.IndexByte(rest[consumed:], '\n')
		lineLen := 0
		if nextNL >= 0 {
			lineLen = nextNL + 1
		} else {
			lineLen = len(rest) - consumed
		}
		line := rest[consumed : consumed+lineLen]
		lineNoNL := bytes.TrimSuffix(line, []byte{'\n'})
		lineNoNL = bytes.TrimSuffix(lineNoNL, []byte{'\r'})
		if len(lineNoNL) > scannerMax {
			return nil, nil, fmt.Errorf("%s %s: read frontmatter: line exceeds %d bytes", kind, filename, scannerMax)
		}
		consumed += lineLen
		if strings.TrimSpace(string(lineNoNL)) == frontmatterMarker {
			bodyOffset = consumed
			break
		}
		fmBuf.Write(lineNoNL)
		fmBuf.WriteByte('\n')
	}
	if bodyOffset < 0 {
		return nil, nil, fmt.Errorf("%s %s: missing closing frontmatter marker '---'", kind, filename)
	}

	body = nil
	if consumed < len(rest) {
		body = rest[consumed:]
	}
	return fmBuf.Bytes(), body, nil
}

// extractSections walks the Markdown body and returns a map of
// subsection-id → subsection body for every `### <id>` heading
// inside the named `## <sectionHeading>` section. Other level-2
// sections are ignored — they're documentation, not consumed by
// the parser.
//
// Used by both ParseWorkflowMarkdown (for `## Prompts`) and
// ParseSwarmMarkdown (for `## Role prompts`). Returns an empty
// (non-nil) map when the named section is absent so callers can
// range without nil-checking.
//
// siblingHeadings names OTHER level-2 sections the same document
// type defines (e.g. a SWARM-skill body carries both `## Prompts`
// and `## Role prompts`). A `## ` line whose heading matches one of
// these always closes the target section, even when more `### `
// subsections follow it — without this, the in-body sub-heading
// heuristic below would absorb a sibling section that legitimately
// owns its own subsections.
func extractSections(body []byte, sectionHeading, kind, filename string, siblingHeadings ...string) (map[string]string, error) {
	target := "## " + sectionHeading
	siblings := make(map[string]bool, len(siblingHeadings))
	for _, h := range siblingHeadings {
		siblings["## "+h] = true
	}
	sections := make(map[string]string)
	if len(body) == 0 {
		return sections, nil
	}

	// Structural headings are recognised only at column 0 (no leading
	// whitespace). Role/step prompt bodies legitimately contain
	// *indented* `## ` / `### ` lines as example/template content
	// (e.g. dev-swarm's analyst prompt embeds an indented
	// "  ### Subtask N: <short name>" template). Trimming before the
	// prefix check would mis-read those as headings; matching the raw
	// line start treats them as the body prose they are.
	isHeading2 := func(s string) bool {
		return strings.HasPrefix(s, "## ") && !strings.HasPrefix(s, "### ")
	}
	isHeading3 := func(s string) bool { return strings.HasPrefix(s, "### ") }

	// Pre-scan for the line index of the LAST column-0 `### `
	// subsection heading. A level-2 `## ` heading that appears
	// *before* this line while we are inside the target section is an
	// in-body sub-heading — operators legitimately use `## Foo` to
	// structure a long role/step prompt body (e.g. dev-swarm's coder
	// prompt has `## Workflow — TDD`). It must NOT be mistaken for a
	// sibling section that closes the prompts. Only a `## ` heading at
	// or after the last subsection genuinely terminates the section —
	// the documented trailing-section case (`## Notes`,
	// `## Error handling`) — or one whose text is a known sibling
	// section. This keeps the terminator contract every existing test
	// pins while no longer silently dropping every role prompt that
	// follows an in-body `## ` heading. (Limitation: a real trailing
	// `## ` section that itself contains a `### ` sub-heading and is
	// not a declared sibling would be absorbed; no config does this,
	// and the alternative — silently swallowing role prompts — is far
	// worse.)
	lastSubsection := -1
	{
		pre := bufio.NewScanner(bytes.NewReader(body))
		pre.Buffer(make([]byte, 0, scannerInitial), scannerMax)
		for idx := 0; pre.Scan(); idx++ {
			if isHeading3(pre.Text()) {
				lastSubsection = idx
			}
		}
		if err := pre.Err(); err != nil {
			return nil, fmt.Errorf("%s %s: read body: %w", kind, filename, err)
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, scannerInitial), scannerMax)

	var (
		inTarget    bool
		currentID   string
		currentBody bytes.Buffer
	)

	flush := func() {
		if currentID == "" {
			return
		}
		sections[currentID] = strings.TrimSpace(currentBody.String())
		currentID = ""
		currentBody.Reset()
	}

	lineIdx := -1
	for scanner.Scan() {
		lineIdx++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Level-2 heading toggles whether we're inside the target —
		// unless it's an in-body sub-heading (inside the target, with
		// more `### ` subsections still to come), in which case it's
		// kept as body content of the open subsection.
		if isHeading2(line) {
			if inTarget && lineIdx < lastSubsection && !siblings[trimmed] && trimmed != target {
				if currentID != "" {
					currentBody.WriteString(line)
					currentBody.WriteByte('\n')
				}
				continue
			}
			flush()
			inTarget = trimmed == target
			continue
		}
		if !inTarget {
			continue
		}
		// Level-3 heading inside the target opens a subsection.
		if isHeading3(line) {
			flush()
			currentID = strings.TrimSpace(strings.TrimPrefix(trimmed, "###"))
			continue
		}
		if currentID == "" {
			// Prose between `## <section>` and the first `###` is
			// section preamble — discard.
			continue
		}
		currentBody.WriteString(line)
		currentBody.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", kind, filename, err)
	}
	flush()

	return sections, nil
}
