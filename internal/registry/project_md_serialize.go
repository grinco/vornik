package registry

// PROJECT.md serializer — the inverse of ParseProjectMarkdown.
// Used by the web brief editor to write back form values as a
// canonical PROJECT.md file that the loader can re-parse on the
// next reload.
//
// Determinism contract (see web-authoring-ux-design.md):
//   1. Frontmatter: projectId, then optional displayName.
//   2. Preamble (Description) when non-empty.
//   3. Five named sections in a FIXED order:
//      Goal, Audience, Success criteria, Out of scope, Risk & cadence.
//   4. Extra sections in their source-captured order, at the tail.
//
// Sections with empty bodies are OMITTED for optional fields
// (OutOfScope, RiskCadence, Extra). Required sections (Goal,
// Audience, Success criteria) are always emitted — the parser
// rejects them empty downstream, surfacing operator error.
//
// One-time reordering note: if an operator hand-authored a
// PROJECT.md with `## References` between `## Goal` and `##
// Audience`, the first form save will move `## References` to
// the tail. That's an acceptable trade-off for canonical
// ordering; subsequent saves are idempotent.

import (
	"bytes"
	"fmt"
	"strings"
)

// briefNamedSectionOrder is the fixed sequence the serializer
// emits named sections in. Mirrors the headings the parser
// extracts from briefKnownSections, kept as a slice so the
// order is explicit and testable.
var briefNamedSectionOrder = []string{
	briefSectionGoal,
	briefSectionAudience,
	briefSectionSuccessCriteria,
	briefSectionOutOfScope,
	briefSectionRiskCadence,
}

// SerializeProjectBrief renders a *ProjectBrief into the
// canonical PROJECT.md byte form. The output is parseable by
// ParseProjectMarkdown — that round-trip is the load-bearing
// invariant the test suite enforces.
//
// Required fields:
//   - ProjectID must be non-empty (otherwise the parser would
//     reject the output and the loader would orphan-error).
//
// The function does NOT validate that required sections (Goal /
// Audience / Success criteria) are non-empty — that's the
// caller's responsibility (the form handler surfaces the
// per-field error in-line). Serializing an incomplete brief
// produces a file the parser will reject on the next load, which
// is the desired guardrail for "save as draft" flows the future
// might want.
func SerializeProjectBrief(brief *ProjectBrief) ([]byte, error) {
	if brief == nil {
		return nil, fmt.Errorf("SerializeProjectBrief: brief is nil")
	}
	if strings.TrimSpace(brief.ProjectID) == "" {
		return nil, fmt.Errorf("SerializeProjectBrief: projectId is required")
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString("projectId: ")
	buf.WriteString(yamlScalarValue(brief.ProjectID))
	buf.WriteByte('\n')
	if strings.TrimSpace(brief.DisplayName) != "" {
		buf.WriteString("displayName: ")
		buf.WriteString(yamlScalarValue(brief.DisplayName))
		buf.WriteByte('\n')
	}
	buf.WriteString("---\n\n")

	if pre := strings.TrimSpace(brief.Description); pre != "" {
		buf.WriteString(pre)
		buf.WriteString("\n\n")
	}

	sections := map[string]string{
		briefSectionGoal:            brief.Goal,
		briefSectionAudience:        brief.Audience,
		briefSectionSuccessCriteria: brief.SuccessCriteria,
		briefSectionOutOfScope:      brief.OutOfScope,
		briefSectionRiskCadence:     brief.RiskCadence,
	}
	required := map[string]bool{
		briefSectionGoal:            true,
		briefSectionAudience:        true,
		briefSectionSuccessCriteria: true,
	}
	for _, name := range briefNamedSectionOrder {
		body := strings.TrimSpace(sections[name])
		if body == "" && !required[name] {
			continue
		}
		writeBriefSection(&buf, name, body)
	}

	for _, extra := range brief.Extra {
		body := strings.TrimSpace(extra.Body)
		if body == "" {
			// Extra subsections with empty bodies are dropped
			// silently — they're hand-authored noise that
			// shouldn't survive a canonical save.
			continue
		}
		writeBriefSection(&buf, extra.Heading, body)
	}

	return buf.Bytes(), nil
}

// writeBriefSection writes a single `## Heading\n\nbody\n\n`
// block. Trailing blank line keeps the rendered file readable
// when stacked sections follow.
func writeBriefSection(buf *bytes.Buffer, heading, body string) {
	buf.WriteString("## ")
	buf.WriteString(heading)
	buf.WriteString("\n\n")
	if body != "" {
		buf.WriteString(body)
		buf.WriteString("\n\n")
	}
}

// yamlScalarValue renders a string as a YAML frontmatter scalar.
// We always double-quote so any embedded `:` / `#` / `[` / `{` /
// leading whitespace stays interpreted as a string rather than
// triggering YAML's auto-typing surprise. Inner quotes get
// escaped via YAML's `\"` convention by wrapping in double-quoted
// style. Multi-line strings aren't expected in frontmatter (the
// only scalars we emit are projectId + displayName); we don't
// support them here.
func yamlScalarValue(s string) string {
	// Empty case is filtered at the caller; defensive return
	// here so any future re-use doesn't emit a bare `""` that
	// the parser treats as "explicit empty".
	if s == "" {
		return `""`
	}
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
