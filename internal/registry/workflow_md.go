package registry

// WORKFLOW.md authoring primitive — see
// https://docs.vornik.io
//
// A WORKFLOW.md file is YAML frontmatter (the same fields the
// existing YAML loader consumes) plus a Markdown body whose
// `## Prompts` section provides per-step prompt bodies. The
// parser produces the same *Workflow struct the YAML loader
// produces, so the executor / scheduler / hash machinery don't
// branch on source format.
//
// The frontmatter split + section walker are shared with
// swarm_md.go via mdfrontmatter.go; this file owns only the
// Workflow-specific apply step.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// workflowKindLabel identifies this file format in error
// messages, so operators see "WORKFLOW.md foo.md: …" rather
// than the swarm-side "swarm foo.md: …".
const workflowKindLabel = "WORKFLOW.md"

// promptsSectionHeading is the level-2 Markdown heading the
// parser scans for in the body.
const promptsSectionHeading = "Prompts"

// ParseWorkflowMarkdown decodes a WORKFLOW.md file into a
// *Workflow ready for the same validation + registry-load path
// the YAML loader uses. The filename is included in error
// messages so authors can find the offending file in the daemon
// log without grepping.
//
// Contract:
//   - The file MUST start with a `---` frontmatter marker
//     (leading whitespace / UTF-8 BOM are tolerated).
//   - The frontmatter MUST close with a matching `---` on its
//     own line.
//   - The frontmatter MUST yaml.Unmarshal cleanly into a
//     Workflow struct.
//   - Every step in the frontmatter MUST have a prompt — either
//     inline via `prompt:` in the frontmatter, or via a
//     `### <step-id>` subsection inside a `## Prompts` body
//     section.
//   - A `### <step-id>` subheading that doesn't match a known
//     step is rejected so typos surface at load rather than
//     dispatch.
//
// Frontmatter inline prompts win over body subsections (silent
// override). Authors who want body-canonical behaviour leave
// `prompt:` off the step entry.
func ParseWorkflowMarkdown(content []byte, filename string) (*Workflow, error) {
	frontmatter, body, err := splitFrontmatter(content, workflowKindLabel, filename)
	if err != nil {
		return nil, err
	}

	var wf Workflow
	if err := yaml.Unmarshal(frontmatter, &wf); err != nil {
		return nil, fmt.Errorf("%s %s: yaml frontmatter parse: %w", workflowKindLabel, filename, err)
	}

	prompts, err := extractSections(body, promptsSectionHeading, workflowKindLabel, filename)
	if err != nil {
		return nil, err
	}

	if err := applyWorkflowPrompts(&wf, prompts, filename); err != nil {
		return nil, err
	}

	return &wf, nil
}

// applyWorkflowPrompts fills each step's Prompt from the body
// subsections. Frontmatter-inline prompts win. Reports an error
// if a `### <step-id>` references a step not in the frontmatter,
// or if any agent step is left without a prompt.
func applyWorkflowPrompts(wf *Workflow, prompts map[string]string, filename string) error {
	for stepID := range prompts {
		if _, ok := wf.Steps[stepID]; !ok {
			return fmt.Errorf("%s %s: '## Prompts' has subsection '### %s' but no step '%s' is defined in the frontmatter", workflowKindLabel, filename, stepID, stepID)
		}
	}

	if wf.Steps == nil {
		// Validation will catch this — the YAML loader has the same
		// "at least one step is required" rule. Return early here so
		// the missing-prompt loop below doesn't fire on a zero map.
		return nil
	}

	updated := make(map[string]WorkflowStep, len(wf.Steps))
	for stepID, step := range wf.Steps {
		if step.Type != "agent" {
			// Non-agent steps (gate, approval) don't carry prompts;
			// leave them untouched.
			updated[stepID] = step
			continue
		}
		if step.Prompt == "" {
			body, ok := prompts[stepID]
			if !ok || body == "" {
				return fmt.Errorf("%s %s: step '%s' has no prompt (set frontmatter `prompt:` or add a '### %s' subsection under '## Prompts')", workflowKindLabel, filename, stepID, stepID)
			}
			step.Prompt = body
		}
		updated[stepID] = step
	}
	wf.Steps = updated
	return nil
}
