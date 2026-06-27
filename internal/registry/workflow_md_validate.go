package registry

// WORKFLOW.md "phase 2" validator — enforces the agentskills.io /
// SKILL.md frontmatter shape on top of the workflow-specific schema
// validation in workflow.go.
//
// Background. Phase 1 (commits 161bf35 / 0a2afeb / 39f8510) moved
// workflows from YAML-plus-prompt-files to a single Markdown file
// with YAML frontmatter; YAML support was dropped at the end of
// phase 1. Phase 2 layers a content-quality check on top so the
// shared catalog can cross-pollinate with the agentskills.io
// ecosystem without operators rediscovering field-by-field what
// the convention requires.
//
// The phase-1 parser (workflow_md.go) is intentionally minimal:
// fail fast on syntactic problems, defer everything else to
// Workflow.Validate(). This file is the "everything else" for the
// SKILL.md shape — required + recommended fields, length caps,
// step→prompt symmetry, name+version regexes. It runs after
// parsing so callers see a coherent (severity, code, field,
// message) tuple per finding rather than a single bail-on-first
// error string.
//
// The validator does NOT mutate the file. The `--fix` surface on
// the CLI prints suggestions; writing them back is explicit
// operator work, not validator concern. That keeps this package
// reusable from the daemon's doctor check (where mutation would
// be alarming) and the CLI alike.

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Severity classifies a validator finding. ERROR fails the
// command + the doctor check; WARNING is reported but does not
// fail. The naming mirrors the existing DoctorCheck.Status
// vocabulary so the doctor adapter doesn't have to translate.
type Severity string

const (
	// SeverityError marks rules that fail the validator's overall
	// run — required fields, malformed shape, oversized file.
	SeverityError Severity = "ERROR"
	// SeverityWarning marks recommended-but-optional rules —
	// missing `author` / `license`, file approaching the 15 k char
	// "target" ceiling. The run still passes overall.
	SeverityWarning Severity = "WARNING"
)

// Hard + soft file-size limits.
//
//   - HARD: 100 000 chars. Past this, a workflow is almost
//     certainly conflating multiple skills into one file; reject
//     so the operator splits before merging.
//   - SOFT: 15 000 chars. agentskills.io's recommended ceiling —
//     warn but don't fail.
const (
	workflowMDHardSizeLimit = 100_000
	workflowMDSoftSizeLimit = 15_000
	workflowMDNameMaxLen    = 64
	workflowMDDescMaxLen    = 1024
)

// nameShapeRe matches the SKILL.md `name` shape: lowercase
// letters, digits, single hyphens between segments. Anchored;
// rejects leading/trailing hyphen, consecutive hyphens, and
// non-ASCII. The same regex validates `metadata.related_skills`
// entries (each is a name pointing at another skill).
var nameShapeRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// versionShapeRe accepts both two-segment (`1.0`) and full
// semver (`1.0.0[-pre][+meta]`) forms. agentskills.io's spec
// says "semver"; the shipped workflows in this repo predate the
// spec and use the looser two-segment form. Accept both so the
// validator's "MUST pass on every shipped file" property holds
// without a mass-rename — but reject obviously-not-a-version
// strings like a Go duration (`25m`).
//
// Loosely follows SemVer 2.0.0 with an optional missing patch
// segment; we don't load a semver library because the regex is
// readable enough on its own and we don't have version-compare
// semantics here.
var versionShapeRe = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?(-[0-9A-Za-z][0-9A-Za-z.\-]*)?(\+[0-9A-Za-z][0-9A-Za-z.\-]*)?$`)

// WorkflowMDFinding is a single validator output.
type WorkflowMDFinding struct {
	// Severity is ERROR or WARNING; ERROR fails the run.
	Severity Severity
	// Code is a short machine-stable identifier for the rule,
	// useful in tests and `--fix` flag dispatch. Format
	// `<area>_<reason>` (e.g. `name_shape`, `description_missing`).
	Code string
	// Field is the dotted-path of the offending frontmatter key
	// ("name", "metadata.related_skills[0]"). Empty for findings
	// that aren't tied to a specific field (e.g. file_size).
	Field string
	// Message is the human-readable explanation.
	Message string
	// Hint, when set, is the suggested fix the `--fix` flag will
	// print. Empty when no mechanical fix is meaningful (e.g.
	// "write a description").
	Hint string
}

// String returns a single-line representation suitable for CLI
// output: `[SEVERITY] code: field — message`.
func (f WorkflowMDFinding) String() string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(string(f.Severity))
	b.WriteString("] ")
	b.WriteString(f.Code)
	if f.Field != "" {
		b.WriteString(": ")
		b.WriteString(f.Field)
	}
	b.WriteString(" — ")
	b.WriteString(f.Message)
	return b.String()
}

// WorkflowMDValidationReport is the validator's full output.
type WorkflowMDValidationReport struct {
	// Filename is the file the report applies to (basename, not a
	// full path) so the doctor adapter can stable-sort.
	Filename string
	// Findings is the full list of findings, in rule-encounter
	// order. Empty == clean run.
	Findings []WorkflowMDFinding
}

// HasErrors reports whether any finding is at ERROR severity.
// The CLI uses this to set its exit code; the doctor check uses
// it to pick Status.
func (r *WorkflowMDValidationReport) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// HasWarnings reports whether any finding is at WARNING. Used by
// the doctor check to distinguish "clean" from "passed but
// noisy" — the latter becomes WARNING in the report.
func (r *WorkflowMDValidationReport) HasWarnings() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

// workflowMDFrontmatterShape is the subset of frontmatter fields
// the SKILL.md validator inspects. Kept separate from the full
// Workflow struct because:
//
//   - We need a permissive yaml.Node-typed view of `metadata` so
//     unknown subfields don't blow up the parse.
//   - The `name` field maps to the existing `workflowId` key
//     (we accept either, preferring `name` if both are present
//     — agentskills.io uses `name`, vornik's pre-phase-2 files
//     use `workflowId`). The validator handles the alias inline
//     rather than letting yaml's strict mode reject the file.
type workflowMDFrontmatterShape struct {
	Name        string    `yaml:"name"`
	WorkflowID  string    `yaml:"workflowId"`
	Description string    `yaml:"description"`
	Version     string    `yaml:"version"`
	Author      string    `yaml:"author"`
	License     string    `yaml:"license"`
	Metadata    yaml.Node `yaml:"metadata"`
}

// relatedSkills extracts the optional metadata.related_skills
// list. Returns nil + true when the field is absent (so the
// validator skips the per-entry shape check); the second return
// is false when the field is present but malformed (not a list
// of strings) so the validator can emit a single shape error
// rather than spurious per-entry findings.
func (s *workflowMDFrontmatterShape) relatedSkills() ([]string, bool) {
	if s.Metadata.Kind == 0 {
		// Field absent — no findings.
		return nil, true
	}
	if s.Metadata.Kind != yaml.MappingNode {
		return nil, false
	}
	// Iterate key/value pairs to find `related_skills`. yaml.v3
	// preserves order so this is deterministic.
	for i := 0; i+1 < len(s.Metadata.Content); i += 2 {
		k := s.Metadata.Content[i]
		v := s.Metadata.Content[i+1]
		if k.Value != "related_skills" {
			continue
		}
		if v.Kind == yaml.ScalarNode && (v.Value == "" || v.Value == "null" || v.Tag == "!!null") {
			return nil, true
		}
		if v.Kind != yaml.SequenceNode {
			return nil, false
		}
		out := make([]string, 0, len(v.Content))
		for _, item := range v.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, false
			}
			out = append(out, item.Value)
		}
		return out, true
	}
	return nil, true
}

// ValidateWorkflowMarkdown runs every SKILL.md-shape rule
// against a WORKFLOW.md file's raw bytes and returns the full
// report. Never returns a Go error for a content problem — all
// findings live in the report, so the caller's UX is consistent.
// A Go error indicates the validator itself couldn't run (I/O
// failure on the bytes is the caller's responsibility).
//
// Filename is the basename (e.g. "adaptive.md") used in
// findings so operators can spot which file in a batch run.
//
// The validator does NOT call Workflow.Validate(). That's the
// registry's job at load time; the SKILL.md shape and the
// workflow schema are orthogonal concerns and conflating them
// would make a clear violation of one rule look like a generic
// "validation failed" string from the other.
func ValidateWorkflowMarkdown(content []byte, filename string) *WorkflowMDValidationReport {
	report := &WorkflowMDValidationReport{Filename: filename}

	// Size cap: applied to the RAW bytes, not the post-strip
	// content, so a 200KB file padded with trailing whitespace
	// still trips the cap. The 100 000-char limit is generous
	// (the agentskills.io spec quotes the same figure); the
	// 15 000-char soft target follows the same source.
	size := len(content)
	if size > workflowMDHardSizeLimit {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "file_size_hard",
			Message:  fmt.Sprintf("file is %d chars; max is %d. Split into smaller skills or move docs out of the workflow file.", size, workflowMDHardSizeLimit),
		})
		// Keep going — operators want to see every problem in
		// one pass, not "fix size, then run again, fix next".
	} else if size > workflowMDSoftSizeLimit {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "file_size_soft",
			Message:  fmt.Sprintf("file is %d chars; target ≤%d. Consider moving long prose to a separate doc.", size, workflowMDSoftSizeLimit),
		})
	}

	// Frontmatter split: reuse the phase-1 helper so the same
	// "marker on its own line, BOM-tolerant" semantics apply.
	frontmatter, body, err := splitFrontmatter(content, workflowKindLabel, filename)
	if err != nil {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_split",
			Message:  err.Error(),
		})
		return report
	}

	// Empty frontmatter is a degenerate case — yaml.Unmarshal
	// will accept it silently, producing an all-zero shape, and
	// every required-field rule would then fire individually.
	// Surface the root cause loudly.
	if len(bytes.TrimSpace(frontmatter)) == 0 {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_empty",
			Message:  "frontmatter block is empty",
		})
		return report
	}

	var shape workflowMDFrontmatterShape
	if err := yaml.Unmarshal(frontmatter, &shape); err != nil {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_yaml",
			Message:  fmt.Sprintf("frontmatter is not valid YAML: %v", err),
		})
		return report
	}

	// --- Required fields ---------------------------------------------------

	// `name` (alias `workflowId`): required. The validator
	// accepts EITHER key — agentskills.io ships `name`, vornik's
	// pre-phase-2 files ship `workflowId`. Preferring `name`
	// when both are set keeps a migrating author's overlap
	// period sane.
	name := shape.Name
	nameField := "name"
	if name == "" {
		name = shape.WorkflowID
		nameField = "workflowId"
	}
	if name == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "name_missing",
			Field:    "name",
			Message:  "`name` (or legacy `workflowId`) is required",
			Hint:     "name: my-workflow",
		})
	} else {
		if len(name) > workflowMDNameMaxLen {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityError,
				Code:     "name_too_long",
				Field:    nameField,
				Message:  fmt.Sprintf("%s is %d chars; max %d", nameField, len(name), workflowMDNameMaxLen),
			})
		}
		if !nameShapeRe.MatchString(name) {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityError,
				Code:     "name_shape",
				Field:    nameField,
				Message:  fmt.Sprintf("%q is not lowercase-hyphens (a-z, 0-9, single `-` between segments, no leading/trailing hyphen)", name),
				Hint:     suggestNameShape(name),
			})
		}
	}

	// `description`: required. The agentskills.io spec
	// emphasises this — discoverability in a shared catalog
	// depends on it. vornik's pre-phase-2 files don't have one,
	// so phase 2 ships matching `description:` lines to the
	// shipped workflows.
	if strings.TrimSpace(shape.Description) == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "description_missing",
			Field:    "description",
			Message:  "`description` is required",
			Hint:     "description: \"<one-paragraph summary of what this workflow does>\"",
		})
	} else if len(shape.Description) > workflowMDDescMaxLen {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "description_too_long",
			Field:    "description",
			Message:  fmt.Sprintf("description is %d chars; max %d. Move detail into the body.", len(shape.Description), workflowMDDescMaxLen),
		})
	}

	// `version`: required, semver-ish. We accept the looser
	// two-segment form to match the pre-phase-2 corpus; see
	// versionShapeRe for the rationale.
	if shape.Version == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "version_missing",
			Field:    "version",
			Message:  "`version` is required",
			Hint:     "version: \"1.0.0\"",
		})
	} else if !versionShapeRe.MatchString(shape.Version) {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "version_shape",
			Field:    "version",
			Message:  fmt.Sprintf("%q is not semver (expected `MAJOR.MINOR[.PATCH][-prerelease][+meta]`)", shape.Version),
			Hint:     "version: \"1.0.0\"",
		})
	}

	// --- Recommended fields ------------------------------------------------

	// `author` and `license` are recommended. They're WARNING,
	// not ERROR, so an internal-only workflow doesn't have to
	// invent an author to pass validation.
	if shape.Author == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "author_missing",
			Field:    "author",
			Message:  "`author` recommended for shared-catalog discovery",
			Hint:     "author: \"<your name or team>\"",
		})
	}
	if shape.License == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "license_missing",
			Field:    "license",
			Message:  "`license` recommended for shared-catalog discovery",
			Hint:     "license: \"Apache-2.0\"",
		})
	}

	// --- Optional metadata.related_skills ----------------------------------

	if related, ok := shape.relatedSkills(); !ok {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "related_skills_shape",
			Field:    "metadata.related_skills",
			Message:  "`metadata.related_skills` must be a list of strings",
		})
	} else {
		for i, rel := range related {
			if rel == "" {
				report.Findings = append(report.Findings, WorkflowMDFinding{
					Severity: SeverityError,
					Code:     "related_skills_empty",
					Field:    fmt.Sprintf("metadata.related_skills[%d]", i),
					Message:  "empty entry",
				})
				continue
			}
			if len(rel) > workflowMDNameMaxLen {
				report.Findings = append(report.Findings, WorkflowMDFinding{
					Severity: SeverityError,
					Code:     "related_skills_too_long",
					Field:    fmt.Sprintf("metadata.related_skills[%d]", i),
					Message:  fmt.Sprintf("%q is %d chars; max %d", rel, len(rel), workflowMDNameMaxLen),
				})
				continue
			}
			if !nameShapeRe.MatchString(rel) {
				report.Findings = append(report.Findings, WorkflowMDFinding{
					Severity: SeverityError,
					Code:     "related_skills_shape_entry",
					Field:    fmt.Sprintf("metadata.related_skills[%d]", i),
					Message:  fmt.Sprintf("%q is not lowercase-hyphens", rel),
					Hint:     suggestNameShape(rel),
				})
			}
		}
	}

	// --- Body: `## Prompts` symmetry ---------------------------------------
	//
	// The frontmatter declares steps. For every step that's an
	// "agent" with a non-empty `role` and no inline `prompt:`
	// field, the body MUST carry a `### <step-id>` subsection
	// under `## Prompts`. This mirrors the phase-1 parser's
	// applyWorkflowPrompts() but at validator-time so the
	// reviewer can see the gap before the registry rejects the
	// file at load.
	missingPromptSection, missingPromptSteps := analyseWorkflowBody(frontmatter, body)
	if missingPromptSection {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "prompts_section_missing",
			Field:    "body",
			Message:  "frontmatter declares agent steps without inline `prompt:` fields, but the body has no `## Prompts` section",
			Hint:     "## Prompts\n\n### <step-id>\n\n<prompt body>",
		})
	}
	for _, stepID := range missingPromptSteps {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "prompt_step_missing",
			Field:    fmt.Sprintf("steps.%s.prompt", stepID),
			Message:  fmt.Sprintf("agent step %q has no prompt (no inline `prompt:` and no `### %s` subsection under `## Prompts`)", stepID, stepID),
			Hint:     fmt.Sprintf("### %s\n\n<prompt body>", stepID),
		})
	}

	return report
}

// analyseWorkflowBody returns (bodyMissingPromptsSection, stepsMissingPrompts).
//
// A step "needs a body prompt" when:
//   - its type is "agent",
//   - its role is non-empty (gate/approval steps have no role),
//   - and the frontmatter doesn't already inline a `prompt:` for it.
//
// `stepsMissingPrompts` is sorted by step id for stable test output.
func analyseWorkflowBody(frontmatter, body []byte) (bool, []string) {
	// Minimal step shape — only the fields the prompt-symmetry
	// check cares about.
	var stepsShape struct {
		Steps map[string]struct {
			Type   string `yaml:"type"`
			Role   string `yaml:"role"`
			Prompt string `yaml:"prompt"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(frontmatter, &stepsShape); err != nil {
		// Frontmatter YAML errors are surfaced separately; don't
		// double-report here.
		return false, nil
	}
	// Build the list of steps that need a body prompt.
	needPrompt := map[string]struct{}{}
	for id, step := range stepsShape.Steps {
		if step.Type != "agent" {
			continue
		}
		if step.Role == "" {
			continue
		}
		if strings.TrimSpace(step.Prompt) != "" {
			continue
		}
		needPrompt[id] = struct{}{}
	}
	if len(needPrompt) == 0 {
		return false, nil
	}
	bodyPrompts, sectionPresent := walkPromptsSection(body)
	if !sectionPresent {
		// Every needy step is unsatisfied; we report the
		// section-missing finding ONCE plus a per-step finding for
		// each so the operator's `--fix` output covers the whole
		// gap rather than fixing the section header and leaving
		// the subsections empty.
		ids := sortedKeys(needPrompt)
		return true, ids
	}
	var missing []string
	for id := range needPrompt {
		if body, ok := bodyPrompts[id]; !ok || strings.TrimSpace(body) == "" {
			missing = append(missing, id)
		}
	}
	stringSort(missing)
	return false, missing
}

// walkPromptsSection scans the body for `## Prompts` and returns
// the step-id → body map from each `### <step-id>` subsection.
// The second return value is true iff a `## Prompts` heading
// was seen at all (so callers can distinguish "section missing"
// from "section present but a subsection is empty").
//
// Implementation note: deliberately not delegating to
// extractSections() (which always returns a non-nil map) so the
// "section present" boolean is observable without sentinel
// values. Cheaper than reshaping extractSections's return.
func walkPromptsSection(body []byte) (map[string]string, bool) {
	out := map[string]string{}
	if len(body) == 0 {
		return out, false
	}
	const heading = "## " + promptsSectionHeading
	var (
		inTarget     bool
		sectionFound bool
		curID        string
		curBody      []byte
	)
	flush := func() {
		if curID == "" {
			return
		}
		out[curID] = strings.TrimSpace(string(curBody))
		curID = ""
		curBody = curBody[:0]
	}
	for _, raw := range splitLines(body) {
		trim := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trim, "## ") && !strings.HasPrefix(trim, "### ") {
			flush()
			inTarget = trim == heading
			if inTarget {
				sectionFound = true
			}
			continue
		}
		if !inTarget {
			continue
		}
		if strings.HasPrefix(trim, "### ") {
			flush()
			curID = strings.TrimSpace(strings.TrimPrefix(trim, "###"))
			continue
		}
		if curID == "" {
			continue
		}
		curBody = append(curBody, raw...)
		curBody = append(curBody, '\n')
	}
	flush()
	return out, sectionFound
}

// splitLines splits a byte slice into trimmed-of-trailing-CR
// lines without copying the underlying data. Returning raw
// []byte slices keeps walkPromptsSection allocation-light on
// large bodies.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		end := i
		if end > start && b[end-1] == '\r' {
			end--
		}
		out = append(out, b[start:end])
		start = i + 1
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// sortedKeys returns the keys of a set, sorted. Pulled out so
// the test fixtures (which compare full slice values) don't
// flake on Go's randomised map iteration.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	stringSort(out)
	return out
}

// stringSort is a tiny insertion sort. Pulled out of the
// standard library "sort" call so this file has zero deps
// beyond yaml.v3, regexp, and the stdlib basics — keeps the
// validator portable enough to be re-vendored if the agentskills
// catalog grows its own copy.
func stringSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// suggestNameShape rewrites a candidate name into the
// lowercase-hyphens shape: lowercase ASCII, collapse runs of
// non-[a-z0-9] to a single hyphen, trim leading/trailing
// hyphens. Used as the `--fix` hint for name and
// related_skills entries.
func suggestNameShape(raw string) string {
	if raw == "" {
		return raw
	}
	var b strings.Builder
	prevHyphen := true // suppresses a leading hyphen
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevHyphen = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := b.String()
	for strings.HasSuffix(out, "-") {
		out = out[:len(out)-1]
	}
	return out
}
