package registry

// SWARM-SKILL.md validator — enforces the agentskills.io SKILL.md
// shape on top of the structural parse in swarmskill_md.go.
//
// Like the WORKFLOW.md validator, this never returns a Go error
// for a content problem — every issue becomes a finding so the
// CLI prints them all in one pass. A Go error only surfaces if
// the validator itself can't run (and bytes-in / bytes-out has
// no I/O so that path is reserved for catastrophic library bugs).
//
// Severity, finding, and report types are aliased to the
// WORKFLOW.md validator's types — the on-disk shapes share the
// same finding vocabulary, and the CLI table renderer wants one
// type to work with. Aliasing keeps the API surface light without
// inventing parallel hierarchies.

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// swarmSkillFrontmatterShape is the permissive view the validator
// uses to run field-shape checks before delegating to the full
// parser. Mirrors workflowMDFrontmatterShape's role — anything
// unrecognised stays in Metadata as a raw yaml.Node so the
// canonical-field rules can fire even when the structural payload
// is malformed.
type swarmSkillFrontmatterShape struct {
	Name          string    `yaml:"name"`
	Description   string    `yaml:"description"`
	Version       string    `yaml:"version"`
	Author        string    `yaml:"author"`
	License       string    `yaml:"license"`
	RelatedSkills []string  `yaml:"related_skills"`
	Metadata      yaml.Node `yaml:"metadata"`
}

// SwarmSkillFinding mirrors WorkflowMDFinding so the CLI can
// route SwarmSkill validation through the same pretty-printer.
type SwarmSkillFinding = WorkflowMDFinding

// SwarmSkillValidationReport mirrors WorkflowMDValidationReport.
// The HasErrors / HasWarnings helpers carry over verbatim.
type SwarmSkillValidationReport = WorkflowMDValidationReport

// Skill-shape size limits. Same constants as the WORKFLOW.md
// validator on purpose — both formats target the same agentskills.io
// 100k / 15k recommendations and operators learn one budget.
const (
	swarmSkillHardSizeLimit = workflowMDHardSizeLimit
	swarmSkillSoftSizeLimit = workflowMDSoftSizeLimit
	swarmSkillNameMaxLen    = workflowMDNameMaxLen
	swarmSkillDescMaxLen    = workflowMDDescMaxLen
)

// ValidateSwarmSkillMarkdown runs the SKILL.md-shape rules + the
// vornik-payload consistency rules against a SWARM-SKILL.md
// file's raw bytes and returns the full report.
//
// filename is the basename used in finding messages.
//
// Findings the validator emits, in order they'd be encountered:
//
//   - file_size_{hard,soft}: file too large (hard) or above soft target.
//   - frontmatter_{split,empty,parse}: structural problems before any
//     shape check can run.
//   - name_{missing,too_long,shape}: agentskills.io canonical name field.
//   - description_{missing,too_long}: ditto for description.
//   - version_{missing,shape}: ditto for version.
//   - author_missing / license_missing: WARNING-level recommendations.
//   - schema_version_unsupported: metadata.vornik.schema_version not v1.
//   - workflow_id_mismatch: metadata.vornik.workflow.workflowId differs
//     from the canonical name (WARNING; coexistence is allowed).
//   - step_prompt_missing: an agent step has no prompt anywhere.
//   - step_role_unknown: a step references a role not in
//     metadata.vornik.roles.
//   - role_system_prompt_missing: a role has no systemPrompt
//     (WARNING — BuiltinRolePrelude still applies at runtime).
//   - role_unreferenced: a role declared in roles is never referenced
//     by any step (WARNING — operators may bundle spare roles).
func ValidateSwarmSkillMarkdown(content []byte, filename string) *SwarmSkillValidationReport {
	report := &SwarmSkillValidationReport{Filename: filename}

	// Hard byte cap (DoS guard): refuse to parse an oversized input at all —
	// the char-based limits below are advisory and don't stop the parse.
	if len(content) > MaxSwarmSkillBytes {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "file_size_bytes",
			Message:  fmt.Sprintf("input is %d bytes; hard max is %d", len(content), MaxSwarmSkillBytes),
		})
		return report
	}

	size := len(content)
	if size > swarmSkillHardSizeLimit {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "file_size_hard",
			Message:  fmt.Sprintf("file is %d chars; max is %d. Split into smaller skills or move docs out of the file.", size, swarmSkillHardSizeLimit),
		})
	} else if size > swarmSkillSoftSizeLimit {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "file_size_soft",
			Message:  fmt.Sprintf("file is %d chars; target ≤%d. Consider moving long prose to a separate doc.", size, swarmSkillSoftSizeLimit),
		})
	}

	// Split + permissive shape-parse FIRST so we can emit one
	// finding per canonical-field rule. Delegating to
	// ParseSwarmSkill is only safe after the required fields pass,
	// otherwise its all-or-nothing required-field check folds many
	// findings into a single "missing field X" error.
	frontmatter, _, err := splitFrontmatter(content, swarmSkillKindLabel, filename)
	if err != nil {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_split",
			Message:  err.Error(),
		})
		return report
	}
	if len(bytes.TrimSpace(frontmatter)) == 0 {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_empty",
			Message:  "frontmatter block is empty",
		})
		return report
	}

	if err := guardYAMLComplexity(frontmatter, filename); err != nil {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_complexity",
			Message:  err.Error(),
		})
		return report
	}
	var shape swarmSkillFrontmatterShape
	if err := yaml.Unmarshal(frontmatter, &shape); err != nil {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "frontmatter_parse",
			Message:  err.Error(),
		})
		return report
	}

	checkSkillCanonicalShape(&shape, report)

	// Parse the full structural payload via the production parser
	// only if the canonical fields are present — the parser's own
	// required-field guard would otherwise short-circuit here.
	if shape.Name == "" || shape.Description == "" || shape.Version == "" {
		return report
	}

	skill, err := ParseSwarmSkill(content, filename)
	if err != nil {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "parse",
			Message:  err.Error(),
		})
		return report
	}

	checkSkillVornikPayload(skill, report)
	checkSkillWorkflowSymmetry(skill, report)
	checkSkillRoleCoverage(skill, report)

	return report
}

func checkSkillCanonicalShape(shape *swarmSkillFrontmatterShape, report *SwarmSkillValidationReport) {
	// name — required, regex, length cap.
	switch {
	case strings.TrimSpace(shape.Name) == "":
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "name_missing",
			Field:    "name",
			Message:  "required",
			Hint:     "name: <lowercase-hyphenated-id>",
		})
	case len(shape.Name) > swarmSkillNameMaxLen:
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "name_too_long",
			Field:    "name",
			Message:  fmt.Sprintf("name is %d chars; max %d", len(shape.Name), swarmSkillNameMaxLen),
		})
	case !nameShapeRe.MatchString(shape.Name):
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "name_shape",
			Field:    "name",
			Message:  "must be lowercase letters/digits with single hyphens between segments",
			Hint:     fmt.Sprintf("name: %s", sluggifyName(shape.Name)),
		})
	}

	// description — required, length cap.
	switch {
	case strings.TrimSpace(shape.Description) == "":
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "description_missing",
			Field:    "description",
			Message:  "required",
		})
	case len(shape.Description) > swarmSkillDescMaxLen:
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "description_too_long",
			Field:    "description",
			Message:  fmt.Sprintf("description is %d chars; max %d. Move detail to the body.", len(shape.Description), swarmSkillDescMaxLen),
		})
	}

	// version — required, semver-ish shape.
	switch {
	case strings.TrimSpace(shape.Version) == "":
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "version_missing",
			Field:    "version",
			Message:  "required",
			Hint:     "version: 1.0.0",
		})
	case !versionShapeRe.MatchString(shape.Version):
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityError,
			Code:     "version_shape",
			Field:    "version",
			Message:  fmt.Sprintf("%q is not a valid version (expect MAJOR.MINOR[.PATCH][-pre][+meta])", shape.Version),
		})
	}

	// author / license — recommended; warn if missing.
	if strings.TrimSpace(shape.Author) == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "author_missing",
			Field:    "author",
			Message:  "recommended; set so consumers can attribute the skill",
		})
	}
	if strings.TrimSpace(shape.License) == "" {
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "license_missing",
			Field:    "license",
			Message:  "recommended; SPDX identifier (e.g. MIT, Apache-2.0) so consumers know reuse terms",
		})
	}
}

func checkSkillVornikPayload(skill *SwarmSkill, report *SwarmSkillValidationReport) {
	if skill.Workflow == nil && len(skill.Roles) == 0 {
		// Likely a --standard export (no metadata.vornik.* block).
		// Surface as a WARNING so operators inspecting a file
		// they thought was full-fidelity see it; the importer
		// will refuse the file outright.
		report.Findings = append(report.Findings, WorkflowMDFinding{
			Severity: SeverityWarning,
			Code:     "vornik_payload_missing",
			Field:    "metadata.vornik",
			Message:  "no vornik-specific payload; file is read-only for non-vornik tools (export --standard mode)",
		})
		return
	}

	if skill.Workflow != nil && strings.TrimSpace(skill.Workflow.ID) != "" {
		expected := sluggifyName(skill.Name)
		if skill.Workflow.ID != skill.Name && skill.Workflow.ID != expected {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityWarning,
				Code:     "workflow_id_mismatch",
				Field:    "metadata.vornik.workflow.workflowId",
				Message:  fmt.Sprintf("workflowId %q does not match canonical name %q (or its slug %q); consumers may surface mismatched IDs", skill.Workflow.ID, skill.Name, expected),
			})
		}
	}
}

func checkSkillWorkflowSymmetry(skill *SwarmSkill, report *SwarmSkillValidationReport) {
	if skill.Workflow == nil {
		return
	}
	for stepID, step := range skill.Workflow.Steps {
		if step.Type == "agent" && strings.TrimSpace(step.Prompt) == "" {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityError,
				Code:     "step_prompt_missing",
				Field:    fmt.Sprintf("metadata.vornik.workflow.steps.%s.prompt", stepID),
				Message:  fmt.Sprintf("agent step %q has no prompt (inline or under '## %s'/'### %s')", stepID, swarmSkillPromptsSection, stepID),
				Hint:     fmt.Sprintf("## %s\n\n### %s\n\n<step body>", swarmSkillPromptsSection, stepID),
			})
		}
		if step.Role != "" && !skillHasRole(skill.Roles, step.Role) {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityError,
				Code:     "step_role_unknown",
				Field:    fmt.Sprintf("metadata.vornik.workflow.steps.%s.role", stepID),
				Message:  fmt.Sprintf("step %q references role %q which is not declared in metadata.vornik.roles", stepID, step.Role),
			})
		}
	}
}

func checkSkillRoleCoverage(skill *SwarmSkill, report *SwarmSkillValidationReport) {
	if skill.Workflow == nil {
		return
	}
	used := make(map[string]bool, len(skill.Roles))
	for _, step := range skill.Workflow.Steps {
		if step.Role != "" {
			used[step.Role] = true
		}
	}
	for _, r := range skill.Roles {
		if strings.TrimSpace(r.SystemPrompt) == "" {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityWarning,
				Code:     "role_system_prompt_missing",
				Field:    fmt.Sprintf("metadata.vornik.roles[%s].systemPrompt", r.Name),
				Message:  fmt.Sprintf("role %q has no systemPrompt; BuiltinRolePrelude still applies, but a role prompt is recommended", r.Name),
				Hint:     fmt.Sprintf("## %s\n\n### %s\n\n<role body>", swarmSkillRolePromptsSection, r.Name),
			})
		}
		if !used[r.Name] {
			report.Findings = append(report.Findings, WorkflowMDFinding{
				Severity: SeverityWarning,
				Code:     "role_unreferenced",
				Field:    fmt.Sprintf("metadata.vornik.roles[%s]", r.Name),
				Message:  fmt.Sprintf("role %q is declared but never referenced by any workflow step", r.Name),
			})
		}
	}
}

// skillHasRole tests membership without a per-call map build —
// the role list is bounded (< ~20 in practice) so linear scan is
// cheaper than allocating.
func skillHasRole(roles []SwarmRole, name string) bool {
	for _, r := range roles {
		if r.Name == name {
			return true
		}
	}
	return false
}

// SluggifySkillName lower-cases and replaces non-conforming runes
// with hyphens, collapsing runs. Used by the validator's name_shape
// hint, the workflowId mismatch comparison, and the export CLI when
// deriving a SKILL.md-shaped `name` from a workflow's `workflowId`.
func SluggifySkillName(s string) string { return sluggifyName(s) }

// sluggifyName is the internal implementation. The exported wrapper
// keeps the API surface narrow; rename freely without a Go API churn.
func sluggifyName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasHyphen := true // suppress leading hyphens
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastWasHyphen = false
		case r == '-' || r == ' ' || r == '_':
			if !lastWasHyphen {
				b.WriteByte('-')
				lastWasHyphen = true
			}
		}
	}
	out := b.String()
	return strings.TrimRight(out, "-")
}
