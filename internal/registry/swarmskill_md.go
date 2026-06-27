package registry

// SWARM-SKILL.md authoring + portable-publish primitive.
//
// One file packages a single workflow plus the roles its steps
// reference, with frontmatter shaped after the agentskills.io
// SKILL.md spec. The canonical top-level fields (name,
// description, version, author, license, related_skills) are the
// shape consumed by every SKILL.md tool; vornik-specific
// structural payload (the Workflow struct + its roles) lives
// under metadata.vornik.* so a non-vornik reader sees a clean
// SKILL.md and ignores the namespace it doesn't understand.
//
// See https://docs.vornik.io for the
// full contract. Round-trip property the test suite enforces:
//
//	parse(marshal(b)) ≡ b
//	marshal(parse(s)) ≡ s   // for s produced by marshal
//
// Standard mode (MarshalSwarmSkillOpts.Standard = true) drops
// metadata.vornik.* entirely so the file is consumable by
// other agent ecosystems. The output is one-way — the workflow
// + roles can't be reconstructed from a standard-only file.

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SwarmSkillSchemaVersion is the only legal value for
// metadata.vornik.schema_version. Bumped when the on-disk shape
// of metadata.vornik.* changes incompatibly. v1 ships with the
// initial release; v2 will need a migrator.
const SwarmSkillSchemaVersion = 1

// swarmSkillKindLabel names this file format in error messages —
// "SWARM-SKILL.md foo.md: …" so operators can find the offending
// file in a daemon log without grepping for the parser.
const swarmSkillKindLabel = "SWARM-SKILL.md"

// Body section headings the marshaller emits and the parser
// consumes. Stable string constants so the validator can
// reference the same strings without duplicating literals.
const (
	swarmSkillPromptsSection     = "Prompts"
	swarmSkillRolePromptsSection = "Role prompts"
)

// SwarmSkill is the in-memory representation of a SWARM-SKILL.md
// file. The canonical fields mirror agentskills.io's SKILL.md
// spec; Workflow + Roles carry the vornik-specific payload that
// gets materialised into the registry on import.
type SwarmSkill struct {
	Name          string
	Description   string
	Version       string
	Author        string
	License       string
	RelatedSkills []string

	Workflow *Workflow
	Roles    []SwarmRole
}

// MarshalSwarmSkillOpts controls MarshalSwarmSkill output shape.
type MarshalSwarmSkillOpts struct {
	// Standard, when true, drops the metadata.vornik.* block so
	// the resulting file is a clean agentskills.io SKILL.md
	// consumable by non-vornik tools. The body (prompts +
	// role prompts) is preserved either way.
	Standard bool
}

// swarmSkillEnvelope is the intermediate frontmatter shape both
// marshaller and parser hit. Keeping it private decouples the
// on-disk YAML from the public SwarmSkill struct.
type swarmSkillEnvelope struct {
	Name          string                  `yaml:"name"`
	Description   string                  `yaml:"description"`
	Version       string                  `yaml:"version"`
	Author        string                  `yaml:"author,omitempty"`
	License       string                  `yaml:"license,omitempty"`
	RelatedSkills []string                `yaml:"related_skills,omitempty"`
	Metadata      *swarmSkillMetadataYAML `yaml:"metadata,omitempty"`
}

type swarmSkillMetadataYAML struct {
	Vornik *swarmSkillVornikYAML `yaml:"vornik,omitempty"`
}

type swarmSkillVornikYAML struct {
	SchemaVersion int         `yaml:"schema_version"`
	Workflow      *Workflow   `yaml:"workflow,omitempty"`
	Roles         []SwarmRole `yaml:"roles,omitempty"`
}

// MarshalSwarmSkill serialises a SwarmSkill into the on-disk
// SWARM-SKILL.md byte form. The output is:
//
//	---
//	<canonical SKILL.md fields>
//	[metadata.vornik.* (unless opts.Standard)]
//	---
//
//	# <displayName-or-name>
//
//	<description>
//
//	## Prompts
//	### <stepID> ...
//
//	## Role prompts
//	### <roleName> ...
//
// Step IDs render in alphabetical order; role prompts render in
// the order roles appear in skill.Roles. Both choices are
// deterministic so round-trip + golden-file tests stay stable.
//
// Empty zero-value scalars (`prompt: ""`, `count: 0`, etc.) are
// pruned from the frontmatter — pruning is safe because
// yaml.Unmarshal restores the same zero values when the field
// is absent. The pruning keeps the file readable; an operator
// scanning a published skill should see only the fields that
// carry signal.
func MarshalSwarmSkill(skill *SwarmSkill, opts MarshalSwarmSkillOpts) ([]byte, error) {
	if skill == nil {
		return nil, fmt.Errorf("MarshalSwarmSkill: skill is nil")
	}
	if strings.TrimSpace(skill.Name) == "" {
		return nil, fmt.Errorf("MarshalSwarmSkill: name is required")
	}
	if strings.TrimSpace(skill.Description) == "" {
		return nil, fmt.Errorf("MarshalSwarmSkill: description is required")
	}
	if strings.TrimSpace(skill.Version) == "" {
		return nil, fmt.Errorf("MarshalSwarmSkill: version is required")
	}
	if skill.Workflow == nil {
		return nil, fmt.Errorf("MarshalSwarmSkill: workflow is required")
	}

	envelope := swarmSkillEnvelope{
		Name:          skill.Name,
		Description:   skill.Description,
		Version:       skill.Version,
		Author:        skill.Author,
		License:       skill.License,
		RelatedSkills: skill.RelatedSkills,
	}

	if !opts.Standard {
		wfCopy := cloneWorkflowWithoutPrompts(skill.Workflow)
		rolesCopy := cloneRolesWithoutSystemPrompts(skill.Roles)
		envelope.Metadata = &swarmSkillMetadataYAML{
			Vornik: &swarmSkillVornikYAML{
				SchemaVersion: SwarmSkillSchemaVersion,
				Workflow:      wfCopy,
				Roles:         rolesCopy,
			},
		}
	}

	frontmatterBytes, err := marshalEnvelopePruned(&envelope)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(frontmatterBytes)
	buf.WriteString("---\n\n")
	writeSkillBody(&buf, skill)
	return buf.Bytes(), nil
}

// ParseSwarmSkill is the inverse of MarshalSwarmSkill. It rejects
// any file that:
//
//   - is missing the canonical agentskills.io fields
//     (name, description, version);
//   - declares an unsupported metadata.vornik.schema_version;
//   - has a `### <stepID>` body subsection that doesn't match a
//     step in metadata.vornik.workflow;
//   - has a `### <roleName>` body subsection that doesn't match
//     a role in metadata.vornik.roles;
//   - references a role from a workflow step that isn't declared
//     in metadata.vornik.roles.
//
// A file produced with MarshalSwarmSkillOpts.Standard = true has
// no metadata.vornik block, so its workflow/roles fields stay
// nil. Standard files are read-only for non-vornik tools; the
// importer rejects them because there's no structural payload
// to materialise.
// MaxSwarmSkillBytes bounds a SWARM-SKILL.md before parse — a DoS guard so a
// huge input can't exhaust memory in the YAML unmarshal. Mirrors the resolver's
// maxResolvedSkillSourceBytes; enforced centrally here so EVERY caller (install
// resolver, `skill import`) is covered, not just the resolver paths.
const MaxSwarmSkillBytes = 1 << 20

// guardYAMLComplexity rejects a frontmatter block whose YAML node graph is
// abnormally large or alias-heavy — a billion-laughs / alias-bomb guard. A
// 1 MiB input passes MaxSwarmSkillBytes yet, via anchors+aliases, can expand
// to multi-GB during Decode. We parse into a yaml.Node (which does NOT expand
// aliases) and bound both the literal node count and the alias count, so the
// subsequent typed Unmarshal can't blow up. Legitimate hand-authored skill
// frontmatter has zero aliases and far fewer nodes than these caps.
func guardYAMLComplexity(frontmatter []byte, filename string) error {
	var root yaml.Node
	if err := yaml.Unmarshal(frontmatter, &root); err != nil {
		return nil // a real parse error is surfaced by the typed Unmarshal that follows
	}
	const (
		maxNodes   = 50000
		maxAliases = 50
	)
	nodes, aliases := 0, 0
	var walk func(*yaml.Node) bool
	walk = func(n *yaml.Node) bool {
		if n == nil {
			return true
		}
		nodes++
		if n.Kind == yaml.AliasNode {
			aliases++
		}
		if nodes > maxNodes || aliases > maxAliases {
			return false
		}
		for _, c := range n.Content {
			if !walk(c) {
				return false
			}
		}
		return true
	}
	if !walk(&root) {
		return fmt.Errorf("%s %s: yaml frontmatter too complex (alias/node bomb guard)", swarmSkillKindLabel, filename)
	}
	return nil
}

func ParseSwarmSkill(content []byte, filename string) (*SwarmSkill, error) {
	if len(content) > MaxSwarmSkillBytes {
		return nil, fmt.Errorf("%s %s: input exceeds %d bytes", swarmSkillKindLabel, filename, MaxSwarmSkillBytes)
	}
	frontmatter, body, err := splitFrontmatter(content, swarmSkillKindLabel, filename)
	if err != nil {
		return nil, err
	}

	if err := guardYAMLComplexity(frontmatter, filename); err != nil {
		return nil, err
	}
	var env swarmSkillEnvelope
	if err := yaml.Unmarshal(frontmatter, &env); err != nil {
		return nil, fmt.Errorf("%s %s: yaml frontmatter parse: %w", swarmSkillKindLabel, filename, err)
	}

	if strings.TrimSpace(env.Name) == "" {
		return nil, fmt.Errorf("%s %s: required field 'name' is missing", swarmSkillKindLabel, filename)
	}
	if strings.TrimSpace(env.Description) == "" {
		return nil, fmt.Errorf("%s %s: required field 'description' is missing", swarmSkillKindLabel, filename)
	}
	if strings.TrimSpace(env.Version) == "" {
		return nil, fmt.Errorf("%s %s: required field 'version' is missing", swarmSkillKindLabel, filename)
	}

	skill := &SwarmSkill{
		Name:          env.Name,
		Description:   env.Description,
		Version:       env.Version,
		Author:        env.Author,
		License:       env.License,
		RelatedSkills: env.RelatedSkills,
	}

	if env.Metadata != nil && env.Metadata.Vornik != nil {
		s := env.Metadata.Vornik
		if s.SchemaVersion != 0 && s.SchemaVersion != SwarmSkillSchemaVersion {
			return nil, fmt.Errorf("%s %s: unsupported metadata.vornik.schema_version %d (supported: %d)", swarmSkillKindLabel, filename, s.SchemaVersion, SwarmSkillSchemaVersion)
		}
		skill.Workflow = s.Workflow
		skill.Roles = s.Roles
	}

	stepPrompts, err := extractSections(body, swarmSkillPromptsSection, swarmSkillKindLabel, filename, swarmSkillRolePromptsSection)
	if err != nil {
		return nil, err
	}
	rolePrompts, err := extractSections(body, swarmSkillRolePromptsSection, swarmSkillKindLabel, filename, swarmSkillPromptsSection)
	if err != nil {
		return nil, err
	}

	if skill.Workflow != nil {
		if err := applyWorkflowPromptsFromSkill(skill.Workflow, stepPrompts, filename); err != nil {
			return nil, err
		}
	} else if len(stepPrompts) > 0 {
		return nil, fmt.Errorf("%s %s: '## Prompts' body section is non-empty but metadata.vornik.workflow is missing (file may have been exported with --standard)", swarmSkillKindLabel, filename)
	}

	if len(skill.Roles) > 0 {
		if err := applyRolePromptsFromSkill(skill.Roles, rolePrompts, filename); err != nil {
			return nil, err
		}
	} else if len(rolePrompts) > 0 {
		return nil, fmt.Errorf("%s %s: '## Role prompts' body section is non-empty but metadata.vornik.roles is missing (file may have been exported with --standard)", swarmSkillKindLabel, filename)
	}

	if skill.Workflow != nil && len(skill.Roles) > 0 {
		if err := crossCheckSkillRoles(skill.Workflow, skill.Roles, filename); err != nil {
			return nil, err
		}
	}

	return skill, nil
}

// cloneWorkflowWithoutPrompts returns a deep-enough copy of wf
// with every Step.Prompt cleared. Prompts always live in the body
// on marshal so the frontmatter stays a clean structural view.
func cloneWorkflowWithoutPrompts(wf *Workflow) *Workflow {
	if wf == nil {
		return nil
	}
	cp := *wf
	if wf.Steps != nil {
		cp.Steps = make(map[string]WorkflowStep, len(wf.Steps))
		for k, v := range wf.Steps {
			v.Prompt = ""
			cp.Steps[k] = v
		}
	}
	return &cp
}

// cloneRolesWithoutSystemPrompts mirrors the workflow helper for
// the roles slice. Returns nil when the input is nil so YAML
// emits no key rather than an empty list.
//
// Also clears the SwarmRole fields that the loader DERIVES from
// OutputSchema during Validate (RequiredOutputKeys,
// PlausibilityRules). The in-memory Swarm carries both — the
// schema as the source of truth, and the legacy fields as a
// derived view — so a naïve re-marshal would emit both and the
// next load would refuse the file ("must be empty when
// outputSchema is set"). Idempotency requires us to suppress
// the derived view at the marshal seam.
func cloneRolesWithoutSystemPrompts(roles []SwarmRole) []SwarmRole {
	if roles == nil {
		return nil
	}
	out := make([]SwarmRole, len(roles))
	for i, r := range roles {
		r.SystemPrompt = ""
		if r.OutputSchema != nil {
			r.RequiredOutputKeys = nil
			r.PlausibilityRules = nil
		}
		out[i] = r
	}
	return out
}

// marshalEnvelopePruned yaml-marshals the envelope and walks the
// resulting yaml.Node tree to drop entries whose value is a
// zero-value scalar / empty mapping / empty sequence. The pruning
// keeps the file readable; absence and zero-value parse identically.
func marshalEnvelopePruned(env *swarmSkillEnvelope) ([]byte, error) {
	var node yaml.Node
	if err := node.Encode(env); err != nil {
		return nil, fmt.Errorf("%s: yaml encode: %w", swarmSkillKindLabel, err)
	}
	pruneZeroValueNodes(&node)
	out, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("%s: yaml marshal: %w", swarmSkillKindLabel, err)
	}
	return out, nil
}

// pruneZeroValueNodes walks a yaml.Node tree and drops mapping
// entries whose value is the zero value for its kind: "" for
// strings, 0 / 0.0 for numbers, false for bools, and any empty
// mapping or sequence. The required canonical fields (name,
// description, version, schema_version) are never zero by the
// time MarshalSwarmSkill calls this — Marshal validates them up
// front.
func pruneZeroValueNodes(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			pruneZeroValueNodes(c)
		}
	case yaml.SequenceNode:
		for _, c := range node.Content {
			pruneZeroValueNodes(c)
		}
	case yaml.MappingNode:
		kept := node.Content[:0]
		for i := 0; i < len(node.Content); i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			pruneZeroValueNodes(v)
			if isZeroValueNode(v) {
				continue
			}
			kept = append(kept, k, v)
		}
		node.Content = kept
	}
}

func isZeroValueNode(n *yaml.Node) bool {
	if n == nil {
		return true
	}
	switch n.Kind {
	case yaml.ScalarNode:
		// Tags: !!str → "" / !!int → 0 / !!float → 0/0.0 / !!bool → false.
		// Be conservative — only treat a known zero literal as pruneable.
		switch n.Tag {
		case "!!str", "":
			return n.Value == ""
		case "!!int":
			return n.Value == "0"
		case "!!float":
			return n.Value == "0" || n.Value == "0.0"
		case "!!bool":
			return n.Value == "false"
		case "!!null":
			return true
		}
		return false
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) == 0
	case yaml.AliasNode:
		return false
	}
	return false
}

// writeSkillBody emits the human-readable body for a SwarmSkill
// — heading + description + per-step prompt + per-role prompt.
// Deterministic: step IDs sorted alphabetically, roles in their
// declared slice order.
func writeSkillBody(buf *bytes.Buffer, skill *SwarmSkill) {
	heading := skill.Name
	if skill.Workflow != nil && strings.TrimSpace(skill.Workflow.DisplayName) != "" {
		heading = skill.Workflow.DisplayName
	}
	buf.WriteString("# ")
	buf.WriteString(heading)
	buf.WriteString("\n\n")

	if desc := strings.TrimSpace(skill.Description); desc != "" {
		buf.WriteString(desc)
		buf.WriteString("\n\n")
	}

	if skill.Workflow != nil && len(skill.Workflow.Steps) > 0 {
		stepIDs := make([]string, 0, len(skill.Workflow.Steps))
		for id := range skill.Workflow.Steps {
			stepIDs = append(stepIDs, id)
		}
		sort.Strings(stepIDs)
		anyPrompt := false
		for _, id := range stepIDs {
			if strings.TrimSpace(skill.Workflow.Steps[id].Prompt) != "" {
				anyPrompt = true
				break
			}
		}
		if anyPrompt {
			buf.WriteString("## ")
			buf.WriteString(swarmSkillPromptsSection)
			buf.WriteString("\n\n")
			for _, id := range stepIDs {
				step := skill.Workflow.Steps[id]
				body := strings.TrimSpace(step.Prompt)
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

	if len(skill.Roles) > 0 {
		anyPrompt := false
		for _, r := range skill.Roles {
			if strings.TrimSpace(r.SystemPrompt) != "" {
				anyPrompt = true
				break
			}
		}
		if anyPrompt {
			buf.WriteString("## ")
			buf.WriteString(swarmSkillRolePromptsSection)
			buf.WriteString("\n\n")
			for _, r := range skill.Roles {
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
	}
}

// applyWorkflowPromptsFromSkill reuses the same precedence rule
// as WORKFLOW.md (frontmatter-inline wins; body fills the gap)
// but raises a SWARM-SKILL.md-shaped error so log triage is
// unambiguous.
func applyWorkflowPromptsFromSkill(wf *Workflow, prompts map[string]string, filename string) error {
	for stepID := range prompts {
		if _, ok := wf.Steps[stepID]; !ok {
			return fmt.Errorf("%s %s: '## %s' has subsection '### %s' but no step '%s' is defined in metadata.vornik.workflow.steps", swarmSkillKindLabel, filename, swarmSkillPromptsSection, stepID, stepID)
		}
	}
	if wf.Steps == nil {
		return nil
	}
	updated := make(map[string]WorkflowStep, len(wf.Steps))
	for stepID, step := range wf.Steps {
		if step.Prompt == "" {
			if body, ok := prompts[stepID]; ok && body != "" {
				step.Prompt = body
			}
		}
		updated[stepID] = step
	}
	wf.Steps = updated
	return nil
}

// applyRolePromptsFromSkill mirrors swarm_md.go's apply rule —
// frontmatter wins, body fills the gap — with the SWARM-SKILL.md
// error prefix.
func applyRolePromptsFromSkill(roles []SwarmRole, prompts map[string]string, filename string) error {
	idx := make(map[string]int, len(roles))
	for i, r := range roles {
		idx[r.Name] = i
	}
	for roleName := range prompts {
		if _, ok := idx[roleName]; !ok {
			return fmt.Errorf("%s %s: '## %s' has subsection '### %s' but no role '%s' is defined in metadata.vornik.roles", swarmSkillKindLabel, filename, swarmSkillRolePromptsSection, roleName, roleName)
		}
	}
	for roleName, body := range prompts {
		if body == "" {
			continue
		}
		i := idx[roleName]
		if roles[i].SystemPrompt == "" {
			roles[i].SystemPrompt = body
		}
	}
	return nil
}

// crossCheckSkillRoles ensures every role referenced by a step
// is declared in skill.Roles. Unreferenced roles are NOT an error
// — operators may legitimately bundle a "spare" role for future
// extension; surfacing that as a warning is the validator's job.
func crossCheckSkillRoles(wf *Workflow, roles []SwarmRole, filename string) error {
	declared := make(map[string]bool, len(roles))
	for _, r := range roles {
		declared[r.Name] = true
	}
	for stepID, step := range wf.Steps {
		if step.Role == "" {
			continue
		}
		if !declared[step.Role] {
			return fmt.Errorf("%s %s: step '%s' references role '%s' which is not declared in metadata.vornik.roles", swarmSkillKindLabel, filename, stepID, step.Role)
		}
	}
	return nil
}
