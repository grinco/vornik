package registry

// MarshalSwarmMarkdown — the inverse of ParseSwarmMarkdown.
// Used by the `vornikctl skill import` path to write back the
// target SWARM.md after merging in the imported skill's roles.
//
// Output shape mirrors the canonical SWARM.md:
//
//	---
//	<swarm frontmatter with empty fields pruned; each role's
//	 systemPrompt is cleared and rendered in the body>
//	---
//
//	# <displayName-or-id>
//
//	## Role prompts
//
//	### <role-name>
//
//	<system prompt body>
//
// Roles render in their slice order (preserving operator-authored
// sequence). Round-trip property: parsing the output via
// ParseSwarmMarkdown returns a *Swarm equal to the input.

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarshalSwarmMarkdown serialises sw into SWARM.md byte form.
// Each role's SystemPrompt is moved to the `## Role prompts`
// body section so the YAML stays a clean structural view.
func MarshalSwarmMarkdown(sw *Swarm) ([]byte, error) {
	if sw == nil {
		return nil, fmt.Errorf("MarshalSwarmMarkdown: swarm is nil")
	}
	if strings.TrimSpace(sw.ID) == "" {
		return nil, fmt.Errorf("MarshalSwarmMarkdown: swarmId is required")
	}

	cp := *sw
	if len(sw.Roles) > 0 {
		// cloneRolesWithoutSystemPrompts also clears the derived
		// fields (RequiredOutputKeys, PlausibilityRules) when a
		// role declares OutputSchema, so the output is loader-
		// idempotent — see that helper's comment.
		cp.Roles = cloneRolesWithoutSystemPrompts(sw.Roles)
	}

	var node yaml.Node
	if err := node.Encode(&cp); err != nil {
		return nil, fmt.Errorf("%s: encode frontmatter: %w", swarmKindLabel, err)
	}
	pruneZeroValueNodes(&node)
	frontmatterBytes, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal frontmatter: %w", swarmKindLabel, err)
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(frontmatterBytes)
	buf.WriteString("---\n\n")

	heading := sw.DisplayName
	if strings.TrimSpace(heading) == "" {
		heading = sw.ID
	}
	buf.WriteString("# ")
	buf.WriteString(heading)
	buf.WriteString("\n\n")

	anyPrompt := false
	for _, r := range sw.Roles {
		if strings.TrimSpace(r.SystemPrompt) != "" {
			anyPrompt = true
			break
		}
	}
	if anyPrompt {
		buf.WriteString("## ")
		buf.WriteString(roleSectionHeading)
		buf.WriteString("\n\n")
		for _, r := range sw.Roles {
			body := strings.TrimSpace(r.SystemPrompt)
			if body == "" {
				continue
			}
			buf.WriteString("### ")
			buf.WriteString(r.Name)
			buf.WriteString("\n\n")
			buf.WriteString(body)
			buf.WriteString("\n\n")
		}
	}

	return buf.Bytes(), nil
}
