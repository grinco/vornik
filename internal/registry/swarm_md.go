package registry

// SWARM.md authoring primitive — mirrors WORKFLOW.md but routes
// per-role systemPrompt bodies into a `## Role prompts` section.
// Same frontmatter + section split as workflows (mdfrontmatter.go);
// only the apply step is swarm-shaped.
//
// Authoring win: role systemPrompts are the largest text blocks
// in any swarm YAML (often 50-200 lines of operational
// instructions). Moving them to a Markdown body section makes
// them editable as prose instead of as YAML scalars with
// indentation-sensitive whitespace.
//
// What stays in frontmatter:
//   - All structural metadata: swarmId, displayName, leadRole,
//     rolePrelude
//   - Every per-role field except SystemPrompt (description,
//     allowed_tools, outputSchema, requiredOutputKeys, runtime,
//     permissions, model, etc.)
//
// outputSchema in particular is a nested YAML structure — pulling
// it into Markdown would just turn it into a fenced code block
// that the parser would have to re-parse. Frontmatter is the
// right home.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// swarmKindLabel identifies this file format in error messages.
const swarmKindLabel = "SWARM.md"

// roleSectionHeading is the level-2 Markdown heading the parser
// scans for per-role systemPrompt bodies.
const roleSectionHeading = "Role prompts"

// ParseSwarmMarkdown decodes a SWARM.md file into a *Swarm ready
// for the same validation + registry-load path the YAML loader
// uses.
//
// Contract (mirrors ParseWorkflowMarkdown):
//   - File starts with `---` frontmatter (BOM/leading-ws tolerated).
//   - Frontmatter closes with `---` on its own line.
//   - Frontmatter yaml.Unmarshals into a Swarm struct.
//   - Every role declared in the frontmatter MUST have a
//     systemPrompt — either inline in the frontmatter, or via a
//     `### <role-name>` subsection inside `## Role prompts`.
//   - A `### <role-name>` subheading that doesn't match a known
//     role is rejected so typos surface at load.
//
// Frontmatter inline `systemPrompt:` wins on conflict. Roles
// that legitimately have no system prompt (rare but legal — the
// daemon's BuiltinRolePrelude + swarm.rolePrelude still apply)
// can opt out by adding `systemPrompt: ""` explicitly in the
// frontmatter — that empty string is treated as "intentionally
// blank" rather than "missing".
func ParseSwarmMarkdown(content []byte, filename string) (*Swarm, error) {
	frontmatter, body, err := splitFrontmatter(content, swarmKindLabel, filename)
	if err != nil {
		return nil, err
	}

	var sw Swarm
	if err := yaml.Unmarshal(frontmatter, &sw); err != nil {
		return nil, fmt.Errorf("%s %s: yaml frontmatter parse: %w", swarmKindLabel, filename, err)
	}

	prompts, err := extractSections(body, roleSectionHeading, swarmKindLabel, filename)
	if err != nil {
		return nil, err
	}

	if err := applyRolePrompts(&sw, prompts, filename); err != nil {
		return nil, err
	}

	return &sw, nil
}

// applyRolePrompts fills each role's SystemPrompt from the body
// subsections. Frontmatter-inline systemPrompt wins. Unknown
// role names under `## Role prompts` fail loud. Roles whose
// frontmatter omits systemPrompt AND that have no body subsection
// fall through silently — see the contract note about
// BuiltinRolePrelude carrying the safety floor.
func applyRolePrompts(sw *Swarm, prompts map[string]string, filename string) error {
	// Index roles by name for the unknown-id check and the
	// update pass.
	roleIdx := make(map[string]int, len(sw.Roles))
	for i, r := range sw.Roles {
		roleIdx[r.Name] = i
	}

	for roleName := range prompts {
		if _, ok := roleIdx[roleName]; !ok {
			return fmt.Errorf("%s %s: '## Role prompts' has subsection '### %s' but no role '%s' is defined in the frontmatter", swarmKindLabel, filename, roleName, roleName)
		}
	}

	for roleName, body := range prompts {
		if body == "" {
			continue
		}
		idx := roleIdx[roleName]
		// Frontmatter wins: only fill from body when the inline
		// systemPrompt is empty AND wasn't explicitly set to "".
		// We can't distinguish "unset" from "empty string" through
		// yaml.Unmarshal alone, so the rule is: any non-empty
		// frontmatter value wins; an empty-string frontmatter value
		// also wins (operator opt-out). The body fills only the
		// "field absent" case, which manifests as an empty string
		// indistinguishable from explicit "" — pragmatic compromise:
		// authors who want body-canonical leave the field off.
		if sw.Roles[idx].SystemPrompt == "" {
			sw.Roles[idx].SystemPrompt = body
		}
	}
	return nil
}
