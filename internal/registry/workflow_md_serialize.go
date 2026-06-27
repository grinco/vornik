package registry

// MarshalWorkflowMarkdown — the inverse of ParseWorkflowMarkdown.
// Used by the `vornikctl skill import` path to materialise a
// Workflow into a WORKFLOW.md file in the deployed config tree.
//
// The output shape is the canonical WORKFLOW.md form:
//
//	---
//	<workflow frontmatter with empty fields pruned>
//	---
//
//	# <displayName-or-id>
//
//	## Prompts
//
//	### <stepID>
//
//	<prompt body>
//
// Step IDs render in alphabetical order; empty zero-value scalars
// are pruned from the frontmatter the same way MarshalSwarmSkill
// does it. Round-trip property: parsing the output via
// ParseWorkflowMarkdown returns a *Workflow equal to the input.

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarshalWorkflowMarkdown serialises wf into WORKFLOW.md byte form.
// Steps with non-empty Prompt have their Prompt cleared from the
// frontmatter and re-emitted under `## Prompts` / `### <id>` so the
// body is canonical prose, not a YAML scalar.
func MarshalWorkflowMarkdown(wf *Workflow) ([]byte, error) {
	if wf == nil {
		return nil, fmt.Errorf("MarshalWorkflowMarkdown: workflow is nil")
	}
	if strings.TrimSpace(wf.ID) == "" {
		return nil, fmt.Errorf("MarshalWorkflowMarkdown: workflowId is required")
	}

	stripped := cloneWorkflowWithoutPrompts(wf)

	var node yaml.Node
	if err := node.Encode(stripped); err != nil {
		return nil, fmt.Errorf("%s: encode frontmatter: %w", workflowKindLabel, err)
	}
	pruneZeroValueNodes(&node)
	frontmatterBytes, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal frontmatter: %w", workflowKindLabel, err)
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(frontmatterBytes)
	buf.WriteString("---\n\n")

	heading := wf.DisplayName
	if strings.TrimSpace(heading) == "" {
		heading = wf.ID
	}
	buf.WriteString("# ")
	buf.WriteString(heading)
	buf.WriteString("\n\n")

	if desc := strings.TrimSpace(wf.Description); desc != "" {
		buf.WriteString(desc)
		buf.WriteString("\n\n")
	}

	if len(wf.Steps) > 0 {
		stepIDs := make([]string, 0, len(wf.Steps))
		for id := range wf.Steps {
			stepIDs = append(stepIDs, id)
		}
		sort.Strings(stepIDs)
		anyPrompt := false
		for _, id := range stepIDs {
			if strings.TrimSpace(wf.Steps[id].Prompt) != "" {
				anyPrompt = true
				break
			}
		}
		if anyPrompt {
			buf.WriteString("## ")
			buf.WriteString(promptsSectionHeading)
			buf.WriteString("\n\n")
			for _, id := range stepIDs {
				body := strings.TrimSpace(wf.Steps[id].Prompt)
				if body == "" {
					continue
				}
				buf.WriteString("### ")
				buf.WriteString(id)
				buf.WriteString("\n\n")
				buf.WriteString(body)
				buf.WriteString("\n\n")
			}
		}
	}

	return buf.Bytes(), nil
}
