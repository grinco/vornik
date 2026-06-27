package registry

// Surgical edits on SWARM.md body content. Used by the web
// swarm editor (Phase 1B v1) to update per-role systemPrompt
// subsections without touching the frontmatter or any
// non-prompts body section. Preserves comments / spacing in the
// non-edited spans so operators don't lose hand-authored
// documentation on a form save.
//
// Pairs with applyYAMLPatches (in internal/ui) which handles the
// frontmatter half. The UI handler splits SWARM.md once,
// applies each half's edits independently, then joins.

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// SplitSwarmContent peels the YAML frontmatter and Markdown
// body apart so a caller can apply structure-aware edits to
// each half independently. Same shape contract as
// splitFrontmatter (the package-private helper used by the
// parser); exported here because the web layer needs it.
//
// Returns (frontmatter, body, error). The frontmatter slice
// excludes the surrounding `---` markers. The body slice
// includes everything after the closing marker, including the
// leading blank line that conventionally separates body from
// frontmatter.
func SplitSwarmContent(content []byte, filename string) (frontmatter, body []byte, err error) {
	return splitFrontmatter(content, swarmKindLabel, filename)
}

// SplitWorkflowContent is the WORKFLOW.md sibling of
// SplitSwarmContent — same shape, different kind label for
// error messages.
func SplitWorkflowContent(content []byte, filename string) (frontmatter, body []byte, err error) {
	return splitFrontmatter(content, workflowKindLabel, filename)
}

// JoinWorkflowContent reassembles a WORKFLOW.md file from its
// frontmatter + body halves. Logically identical to
// JoinSwarmContent; kept as a separate exported name so the
// caller's intent is documented at the call site.
func JoinWorkflowContent(frontmatter, body []byte) []byte {
	return joinFrontmatterBody(frontmatter, body)
}

// JoinSwarmContent reassembles a swarm file from its
// frontmatter + body halves. Always emits the canonical
// `---\n<frontmatter>\n---\n<body>` shape. A trailing newline
// is added to the frontmatter slice if missing so the closing
// `---` lands on its own line.
func JoinSwarmContent(frontmatter, body []byte) []byte {
	return joinFrontmatterBody(frontmatter, body)
}

// joinFrontmatterBody is the shared implementation used by
// both JoinSwarmContent and JoinWorkflowContent — the on-disk
// shape is identical for both formats so the join logic is too.
func joinFrontmatterBody(frontmatter, body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(frontmatter)
	if len(frontmatter) == 0 || frontmatter[len(frontmatter)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString("---\n")
	buf.Write(body)
	return buf.Bytes()
}

// ReplaceSwarmRolePrompts is a thin SWARM.md wrapper around
// replaceMarkdownSubsections targeting the `## Role prompts`
// section.
func ReplaceSwarmRolePrompts(body []byte, updates map[string]string) ([]byte, error) {
	return replaceMarkdownSubsections(body, roleSectionHeading, updates, nil)
}

// ReplaceSwarmRolePromptsKeeping is ReplaceSwarmRolePrompts with pruning:
// any `### <id>` subsection whose id is NOT in keep is dropped. The
// schema-driven swarm editor passes keep = the current role names so a
// removed role's prompt body goes with it — otherwise the parser rejects
// the orphan subsection ("no role X is defined in the frontmatter"). A
// nil keep keeps every subsection (the un-pruned behaviour).
func ReplaceSwarmRolePromptsKeeping(body []byte, updates map[string]string, keep map[string]bool) ([]byte, error) {
	return replaceMarkdownSubsections(body, roleSectionHeading, updates, keep)
}

// ReplaceWorkflowStepPrompts is the WORKFLOW.md wrapper
// targeting the `## Prompts` section. Behaviourally identical
// to ReplaceSwarmRolePrompts — both are bodies of identical
// shape (a single level-2 wrapper containing level-3
// subsections keyed by id).
func ReplaceWorkflowStepPrompts(body []byte, updates map[string]string) ([]byte, error) {
	return replaceMarkdownSubsections(body, promptsSectionHeading, updates, nil)
}

// ReplaceWorkflowStepPromptsKeeping is ReplaceWorkflowStepPrompts with
// pruning: any `### <id>` subsection whose id is NOT in keep is dropped.
// The schema-driven workflow editor passes keep = the current step ids so
// a removed step's prompt body goes with it (otherwise the parser rejects
// the orphan). Nil keep keeps every subsection.
func ReplaceWorkflowStepPromptsKeeping(body []byte, updates map[string]string, keep map[string]bool) ([]byte, error) {
	return replaceMarkdownSubsections(body, promptsSectionHeading, updates, keep)
}

// replaceMarkdownSubsections walks body looking for the named
// level-2 section, then within it replaces each `### <id>`
// subsection's body with the matching value from updates.
// Subsections for ids not in the map are kept verbatim. Ids
// in the map without a matching subsection are appended at
// the end of the named section so a new id can grow its body
// from a form save.
//
// If the body has no `## <sectionHeading>` section at all, one
// is synthesised at the end (after any existing prose / other
// level-2 sections). Empty updates produce verbatim output —
// no reformatting happens.
//
// Body shape rules followed:
//   - subsection body is everything between `### <id>` and
//     the next `###` heading (or the next level-1/level-2
//     heading, or end of section).
//   - replacement body trims leading / trailing whitespace and
//     ends with a single blank line so the next subsection /
//     section starts cleanly.
//   - prose between `## <sectionHeading>` and the first
//     `### <id>` (the section preamble) is preserved.
func replaceMarkdownSubsections(body []byte, sectionHeading string, updates map[string]string, keep map[string]bool) ([]byte, error) {
	if len(updates) == 0 && keep == nil {
		return body, nil
	}

	target := "## " + sectionHeading

	// Mirror extractSections' disambiguation: a level-2 `## ` heading
	// that appears before the LAST `### ` subsection (while inside the
	// target section) is an in-body sub-heading, not a section
	// terminator. Keeping this in lockstep with the parser ensures a
	// round-trip (parse → edit → re-parse) preserves role prompts that
	// use `## ` sub-headings instead of silently splitting them off.
	lastSubsection := -1
	{
		pre := bufio.NewScanner(bytes.NewReader(body))
		pre.Buffer(make([]byte, 0, scannerInitial), scannerMax)
		for idx := 0; pre.Scan(); idx++ {
			if strings.HasPrefix(pre.Text(), "### ") {
				lastSubsection = idx
			}
		}
		if err := pre.Err(); err != nil {
			return nil, fmt.Errorf("%s read body: %w", swarmKindLabel, err)
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, scannerInitial), scannerMax)

	// Pass 1: tokenise into a sequence of typed chunks the
	// rebuilder can walk linearly. Each chunk is either:
	//   - a non-promp-section span (header text or level-2
	//     section other than `## Role prompts`): kept verbatim.
	//   - the `## Role prompts` opening heading.
	//   - the section preamble (lines after `## Role prompts`
	//     up to the first `### <role>`).
	//   - a `### <role>` subsection (heading + body).
	type chunkKind int
	const (
		chunkVerbatim chunkKind = iota
		chunkSectionHeading
		chunkSectionPreamble
		chunkSubsection
	)
	type chunk struct {
		kind chunkKind
		// For verbatim, sectionPreamble, sectionHeading: raw
		// lines as captured.
		lines []string
		// For subsection: the id (heading) and the body lines
		// (excluding the `### id` line itself).
		id        string
		bodyLines []string
	}

	var (
		chunks       []chunk
		current      chunk
		insidePrompt bool
		inSubsection bool
	)

	flushCurrent := func() {
		if current.kind == chunkSubsection {
			chunks = append(chunks, current)
		} else if len(current.lines) > 0 || current.kind == chunkSectionHeading {
			chunks = append(chunks, current)
		}
		current = chunk{}
	}

	openVerbatim := func() {
		if current.kind != chunkVerbatim {
			flushCurrent()
			current = chunk{kind: chunkVerbatim}
		}
	}

	lineIdx := -1
	for scanner.Scan() {
		lineIdx++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Recognise structural headings only at column 0 — indented
		// `## `/`### ` lines are in-body example/template content, not
		// headings. Mirrors extractSections so parse and edit agree.
		isLevel2 := strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "### ")
		isLevel3 := strings.HasPrefix(line, "### ")

		// In-body `## ` sub-heading: inside the prompt section with
		// more `### ` subsections still to come. Keep it as body
		// content rather than closing the section. Must precede the
		// generic `isLevel2 && insidePrompt` close case below.
		if isLevel2 && insidePrompt && inSubsection && lineIdx < lastSubsection {
			current.bodyLines = append(current.bodyLines, line)
			continue
		}

		switch {
		case isLevel2 && trimmed == target:
			flushCurrent()
			insidePrompt = true
			inSubsection = false
			chunks = append(chunks, chunk{kind: chunkSectionHeading, lines: []string{line}})
		case isLevel2 && insidePrompt:
			// Closing the target section: a new level-2 section
			// starts. The remainder is verbatim.
			flushCurrent()
			insidePrompt = false
			inSubsection = false
			openVerbatim()
			current.lines = append(current.lines, line)
		case isLevel3 && insidePrompt:
			flushCurrent()
			id := strings.TrimSpace(strings.TrimPrefix(trimmed, "###"))
			current = chunk{kind: chunkSubsection, id: id}
			inSubsection = true
		case insidePrompt && !inSubsection:
			// Section preamble (lines after `## <section>` and
			// before the first `### <id>`).
			if current.kind != chunkSectionPreamble {
				flushCurrent()
				current = chunk{kind: chunkSectionPreamble}
			}
			current.lines = append(current.lines, line)
		case insidePrompt && inSubsection:
			current.bodyLines = append(current.bodyLines, line)
		default:
			openVerbatim()
			current.lines = append(current.lines, line)
		}
	}
	flushCurrent()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s read body: %w", swarmKindLabel, err)
	}

	// Pass 2: rebuild output. Replace bodies for ids in
	// updates; remember which updates we've applied so we can
	// append the rest.
	applied := map[string]bool{}
	var out bytes.Buffer
	for _, c := range chunks {
		switch c.kind {
		case chunkVerbatim, chunkSectionPreamble, chunkSectionHeading:
			for _, l := range c.lines {
				out.WriteString(l)
				out.WriteByte('\n')
			}
		case chunkSubsection:
			// Prune: drop subsections whose id isn't in keep (nil keep
			// keeps all). Marked applied so it isn't re-appended below.
			if keep != nil && !keep[c.id] {
				applied[c.id] = true
				continue
			}
			out.WriteString("### ")
			out.WriteString(c.id)
			out.WriteByte('\n')
			if newBody, ok := updates[c.id]; ok {
				writeRoleBody(&out, newBody)
				applied[c.id] = true
			} else {
				// Preserve the existing body verbatim.
				for _, l := range c.bodyLines {
					out.WriteString(l)
					out.WriteByte('\n')
				}
			}
		}
	}

	// Append ids in `updates` that didn't appear as a body
	// subsection. Synthesise `## <sectionHeading>` if the
	// original body had no such section at all.
	hasSection := false
	for _, c := range chunks {
		if c.kind == chunkSectionHeading {
			hasSection = true
			break
		}
	}

	needsAppend := []string{}
	for id := range updates {
		if !applied[id] {
			needsAppend = append(needsAppend, id)
		}
	}
	if len(needsAppend) > 0 {
		if !hasSection {
			// Make sure there's a blank line before our new
			// section if the body's tail isn't already blank.
			rendered := out.Bytes()
			if len(rendered) > 0 && !bytes.HasSuffix(rendered, []byte("\n\n")) {
				if !bytes.HasSuffix(rendered, []byte("\n")) {
					out.WriteByte('\n')
				}
				out.WriteByte('\n')
			}
			out.WriteString("## ")
			out.WriteString(sectionHeading)
			out.WriteString("\n\n")
		}
		// Stable-ish order so the appended set is deterministic
		// regardless of map iteration order.
		ordered := sortStrings(needsAppend)
		for _, id := range ordered {
			out.WriteString("### ")
			out.WriteString(id)
			out.WriteByte('\n')
			writeRoleBody(&out, updates[id])
		}
	}

	return out.Bytes(), nil
}

// writeRoleBody emits a role subsection body: leading blank
// line, trimmed body, trailing blank line. Keeps the rendered
// Markdown readable when stacked with sibling subsections.
func writeRoleBody(buf *bytes.Buffer, body string) {
	body = strings.TrimSpace(body)
	buf.WriteByte('\n')
	if body != "" {
		buf.WriteString(body)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
}

// sortStrings is a tiny in-place sort wrapper so the append-
// ordering test stays deterministic without dragging in
// "sort" at the call site every time.
func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
