package registry

// PROJECT.md authoring primitive — see
// https://docs.vornik.io
//
// A PROJECT.md file is a small YAML frontmatter (projectId +
// optional displayName) followed by a Markdown body containing
// the project brief: a free-form preamble that becomes the
// homepage description, plus five named level-2 sections (Goal,
// Audience, Success criteria, Out of scope, Risk & cadence).
//
// Unlike SWARM.md / WORKFLOW.md, the brief does not own
// operational configuration — project.yaml remains authoritative
// for autonomy, permissions, budgets, and so on. PROJECT.md
// captures the *intent* that grounds the prompt-writing
// assistant and the project-tuning wizard. The loader joins
// brief → project by projectId; an orphaned brief is a hard
// error so a typo in projectId surfaces at boot.
//
// Frontmatter / section split is shared with workflow_md.go and
// swarm_md.go via mdfrontmatter.go (splitFrontmatter), but the
// section walker is bespoke: PROJECT.md captures level-2
// sections directly, not level-3 subsections under a single
// level-2 wrapper.

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// projectKindLabel identifies this file format in error messages.
const projectKindLabel = "PROJECT.md"

// Brief section headings. The first three are required; the
// remaining two are optional. Anything else is preserved as an
// Extra section so the editor round-trip is lossless.
const (
	briefSectionGoal            = "Goal"
	briefSectionAudience        = "Audience"
	briefSectionSuccessCriteria = "Success criteria"
	briefSectionOutOfScope      = "Out of scope"
	briefSectionRiskCadence     = "Risk & cadence"
)

// briefRequiredSections is the smallest set of brief sections
// that gives the prompt assistant enough grounding. Anything
// less and "draft this" suggestions devolve into generic prose.
var briefRequiredSections = []string{
	briefSectionGoal,
	briefSectionAudience,
	briefSectionSuccessCriteria,
}

// briefKnownSections is the full list of sections the parser
// surfaces as typed fields on ProjectBrief. Sections outside
// this list land in Extra (preserving order) so future schema
// additions don't drop operator content.
var briefKnownSections = map[string]struct{}{
	briefSectionGoal:            {},
	briefSectionAudience:        {},
	briefSectionSuccessCriteria: {},
	briefSectionOutOfScope:      {},
	briefSectionRiskCadence:     {},
}

// ProjectBrief is the parsed PROJECT.md content. Attached to a
// *Project by the loader; nil when no PROJECT.md companion
// exists. Empty optional sections are stored as "" (not absent)
// so callers don't have to nil-check every field.
type ProjectBrief struct {
	// ProjectID joins the brief to project.yaml of the same ID.
	ProjectID string
	// DisplayName overrides project.yaml.DisplayName when set.
	// Empty means "inherit from project.yaml" — the conflict-
	// resolution rule in the LLD.
	DisplayName string
	// Description is the body preamble: everything between an
	// optional `# Title` line and the first `## ` heading. Empty
	// when the body opens directly with a level-2 section.
	Description string
	// Goal is the high-level objective the brief commits to.
	// Required. Feeds Project.Autonomy.Goal when YAML omits it
	// and is always exposed to the prompt assistant and wizard.
	Goal string
	// Audience identifies who the project serves. Required.
	// Drives audience-specific phrasing in assistant suggestions.
	Audience string
	// SuccessCriteria lists what success looks like. Required.
	SuccessCriteria string
	// OutOfScope is the operator's stated exclusions. Optional;
	// strongly recommended because exclusions sharpen prompt
	// generation more than inclusions do.
	OutOfScope string
	// RiskCadence captures risk tolerance and operating cadence.
	// Optional; feeds autonomy-mode wizard recommendations.
	RiskCadence string
	// Extra holds level-2 sections whose heading isn't in
	// briefKnownSections, preserved in source order so the
	// editor round-trip writes them back verbatim.
	Extra []ProjectBriefSection
}

// ProjectBriefSection is a single level-2 section the parser
// captured but doesn't map to a typed field. Used for Extra.
type ProjectBriefSection struct {
	Heading string
	Body    string
}

// projectBriefFrontmatter is the minimal struct PROJECT.md
// frontmatter unmarshals into. Kept private so callers consume
// ProjectBrief rather than the raw frontmatter shape.
type projectBriefFrontmatter struct {
	ProjectID   string `yaml:"projectId"`
	DisplayName string `yaml:"displayName"`
}

// ParseProjectMarkdown decodes a PROJECT.md file into a
// *ProjectBrief ready for the loader to attach to the matching
// *Project.
//
// Contract:
//   - File MUST start with a `---` frontmatter marker (BOM /
//     leading whitespace tolerated, same as the other primitives).
//   - Frontmatter MUST close with `---` on its own line.
//   - Frontmatter MUST yaml.Unmarshal cleanly and produce a
//     non-empty `projectId`.
//   - Body MUST include non-empty `## Goal`, `## Audience`, and
//     `## Success criteria` sections. Missing or empty required
//     sections fail loud so a half-written brief doesn't reach
//     the autonomy loop or the assistant.
//   - Unknown level-2 sections are kept in Extra in source order.
//   - Section headings are matched case-sensitively (mirrors the
//     SWARM.md / WORKFLOW.md rule). `## goal` does not satisfy
//     the requirement for `## Goal`.
func ParseProjectMarkdown(content []byte, filename string) (*ProjectBrief, error) {
	frontmatter, body, err := splitFrontmatter(content, projectKindLabel, filename)
	if err != nil {
		return nil, err
	}

	var fm projectBriefFrontmatter
	if err := yaml.Unmarshal(frontmatter, &fm); err != nil {
		return nil, fmt.Errorf("%s %s: yaml frontmatter parse: %w", projectKindLabel, filename, err)
	}
	if strings.TrimSpace(fm.ProjectID) == "" {
		return nil, fmt.Errorf("%s %s: frontmatter is missing required field 'projectId'", projectKindLabel, filename)
	}

	preamble, sections, order, err := extractBriefSections(body, filename)
	if err != nil {
		return nil, err
	}

	brief := &ProjectBrief{
		ProjectID:       strings.TrimSpace(fm.ProjectID),
		DisplayName:     strings.TrimSpace(fm.DisplayName),
		Description:     preamble,
		Goal:            sections[briefSectionGoal],
		Audience:        sections[briefSectionAudience],
		SuccessCriteria: sections[briefSectionSuccessCriteria],
		OutOfScope:      sections[briefSectionOutOfScope],
		RiskCadence:     sections[briefSectionRiskCadence],
	}

	for _, heading := range order {
		if _, known := briefKnownSections[heading]; known {
			continue
		}
		brief.Extra = append(brief.Extra, ProjectBriefSection{
			Heading: heading,
			Body:    sections[heading],
		})
	}

	if err := validateBriefRequired(brief, filename); err != nil {
		return nil, err
	}

	return brief, nil
}

// validateBriefRequired enforces that every required section is
// present and non-empty. Reported as a single error per section
// (not aggregated) so the operator sees the first thing to fix
// rather than a wall of complaints.
func validateBriefRequired(brief *ProjectBrief, filename string) error {
	values := map[string]string{
		briefSectionGoal:            brief.Goal,
		briefSectionAudience:        brief.Audience,
		briefSectionSuccessCriteria: brief.SuccessCriteria,
	}
	for _, name := range briefRequiredSections {
		if strings.TrimSpace(values[name]) == "" {
			return fmt.Errorf("%s %s: required brief section '## %s' is missing or empty", projectKindLabel, filename, name)
		}
	}
	return nil
}

// extractBriefSections walks the Markdown body and returns:
//   - preamble: everything before the first `## ` heading, with
//     an optional leading `# Title` line stripped and the rest
//     trimmed.
//   - sections: heading → body for every level-2 section.
//   - order: section headings in source order, so callers can
//     preserve Extra ordering on round-trip.
//
// Level-1 headings (`# `) other than the first occurrence are
// kept as part of whatever section is currently open — they're
// rare in briefs and treating them as section-bodies-with-an-h1
// avoids surprising operators who paste rich Markdown.
func extractBriefSections(body []byte, filename string) (preamble string, sections map[string]string, order []string, err error) {
	sections = make(map[string]string)
	if len(body) == 0 {
		return "", sections, nil, nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, scannerInitial), scannerMax)

	var (
		preambleBuf  bytes.Buffer
		currentHead  string
		currentBody  bytes.Buffer
		sawTitleH1   bool
		seenHeadings = map[string]bool{}
	)

	flush := func() {
		if currentHead == "" {
			return
		}
		sections[currentHead] = strings.TrimSpace(currentBody.String())
		currentBody.Reset()
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### "):
			flush()
			currentHead = strings.TrimSpace(strings.TrimPrefix(trimmed, "##"))
			if seenHeadings[currentHead] {
				return "", nil, nil, fmt.Errorf("%s %s: duplicate level-2 section '## %s'", projectKindLabel, filename, currentHead)
			}
			seenHeadings[currentHead] = true
			order = append(order, currentHead)
		case currentHead == "":
			// Preamble territory. Strip an optional first `# Title`
			// line; keep the rest verbatim.
			if !sawTitleH1 && strings.HasPrefix(trimmed, "# ") {
				sawTitleH1 = true
				continue
			}
			preambleBuf.WriteString(line)
			preambleBuf.WriteByte('\n')
		default:
			currentBody.WriteString(line)
			currentBody.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, nil, fmt.Errorf("%s %s: read body: %w", projectKindLabel, filename, err)
	}
	flush()

	return strings.TrimSpace(preambleBuf.String()), sections, order, nil
}
