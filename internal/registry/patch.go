package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetProjectAutonomyEnabled updates the autonomy.enabled field for a project
// in memory and on disk. If the config directory is unset (e.g. in tests),
// only the in-memory update is performed. Disk write errors are returned but
// the in-memory update is always applied.
func (r *Registry) SetProjectAutonomyEnabled(projectID string, enabled bool) error {
	r.mu.Lock()
	project, exists := r.projects[projectID]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("project %q not found", projectID)
	}
	// Copy-on-write rather than mutating in place. GetProject /
	// ListProjects hand out these *Project pointers, and the autonomy
	// manager reads p.Autonomy.Enabled on them without holding r.mu
	// (manager.go:280,478). Writing the field in place was therefore a
	// data race against those lock-free reads (bug sweep 2026-06-04).
	// Replacing the map entry with a fresh struct keeps every reader on
	// an immutable snapshot; the swap itself is under the write lock.
	updated := *project
	updated.Autonomy.Enabled = enabled
	r.projects[projectID] = &updated
	configDir := r.configDir
	r.mu.Unlock()

	if configDir == "" {
		return nil
	}
	return patchProjectAutonomyEnabled(configDir, projectID, enabled)
}

// patchProjectAutonomyEnabled finds the YAML file for projectID inside
// configDir/projects/ and updates the autonomy.enabled field in place.
func patchProjectAutonomyEnabled(configDir, projectID string, enabled bool) error {
	projectsDir := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return fmt.Errorf("cannot read projects directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		path := filepath.Join(projectsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		// Quick pre-check before full parse.
		if !bytes.Contains(data, []byte(projectID)) {
			continue
		}

		// Verify this is the right project.
		var check struct {
			ProjectID string `yaml:"projectId"`
		}
		if err := yaml.Unmarshal(data, &check); err != nil || check.ProjectID != projectID {
			continue
		}

		patched, err := patchYAMLAutonomyEnabled(data, enabled)
		if err != nil {
			return fmt.Errorf("failed to patch %s: %w", path, err)
		}
		// Verify the output parses cleanly before overwriting the file.
		var roundtrip struct {
			ProjectID string `yaml:"projectId"`
		}
		if err := yaml.Unmarshal(patched, &roundtrip); err != nil {
			return fmt.Errorf("patched YAML for %s failed round-trip validation (not written): %w", path, err)
		}
		// 0o600 — project YAML can carry LLM/MCP/webhook secrets;
		// only the daemon needs read access.
		return os.WriteFile(path, patched, 0o600)
	}

	return fmt.Errorf("config file for project %q not found in %s", projectID, projectsDir)
}

// preserveBlockScalars walks a yaml.Node tree and explicitly marks multi-line
// ScalarNodes to use LiteralStyle. This prevents yaml.v3's encoder from
// choosing a different style (e.g. double-quoted) for strings that contain
// characters like `:` which are valid inside `|` blocks but not in plain style.
func preserveBlockScalars(node *yaml.Node) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "\n") {
		if node.Style == yaml.LiteralStyle || node.Style == yaml.FoldedStyle {
			// already explicit — leave it
		} else {
			node.Style = yaml.LiteralStyle
		}
	}
	for _, child := range node.Content {
		preserveBlockScalars(child)
	}
}

// PatchSwarmAddDispatcherRole appends a "dispatcher" role to the
// frontmatter of a SWARM.md file. Idempotent: re-running on a
// swarm that already has the role is a no-op (returns nil). Used
// by the doctor's --fix path when an operator has set
// telegram.dispatcher_project_id but the chosen project's swarm
// has no dispatcher role.
//
// The added role is a minimal stub:
//
//   - name: dispatcher
//     model: <model>          # the daemon's chat model, for spend attribution
//     runtime:
//     image: "noop:dispatcher"  # dispatcher isn't a containerised agent
//
// runtime.image is required by the registry loader, so we put a
// recognisable noop string. The dispatcher never actually launches
// a container — the role exists so the dashboard's role+model
// aggregation rows align with the swarm catalogue.
//
// As of 2026-05-17 the patcher only accepts `.md` swarm paths.
// The frontmatter is patched in place; the body section is
// preserved byte-for-byte.
func PatchSwarmAddDispatcherRole(swarmPath, model string) error {
	if swarmPath == "" {
		return fmt.Errorf("swarmPath is empty")
	}
	if !strings.HasSuffix(swarmPath, ".md") {
		return fmt.Errorf("patcher accepts only `.md` swarm files (got %s); YAML format was removed 2026-05-17", swarmPath)
	}
	data, err := os.ReadFile(swarmPath)
	if err != nil {
		return fmt.Errorf("read swarm file: %w", err)
	}
	frontmatter, body, err := splitFrontmatter(data, swarmKindLabel, filepath.Base(swarmPath))
	if err != nil {
		return err
	}
	patchedFM, alreadyPresent, err := patchYAMLAddDispatcherRole(frontmatter, model)
	if err != nil {
		return fmt.Errorf("patch swarm frontmatter: %w", err)
	}
	if alreadyPresent {
		return nil
	}
	// Round-trip the patched frontmatter through the SWARM.md
	// parser by reassembling the file. Catches structural damage
	// (broken yaml.Node manipulation, missing required fields)
	// before we overwrite the source.
	reassembled := assembleMarkdown(patchedFM, body)
	if _, err := ParseSwarmMarkdown(reassembled, filepath.Base(swarmPath)); err != nil {
		return fmt.Errorf("patched SWARM.md failed round-trip parse (not written): %w", err)
	}
	// 0o600 — SWARM.md frontmatter can reference role-model API
	// gateway tokens; the daemon owns the file.
	return os.WriteFile(swarmPath, reassembled, 0o600)
}

// assembleMarkdown rebuilds a SWARM.md / WORKFLOW.md file from a
// patched frontmatter byte buffer + the original body. Caller
// supplies the frontmatter without the surrounding `---` markers;
// this helper adds them back and stitches the body verbatim.
func assembleMarkdown(frontmatter, body []byte) []byte {
	var out []byte
	out = append(out, []byte("---\n")...)
	out = append(out, frontmatter...)
	// Ensure exactly one newline between the YAML and the closing
	// marker — yaml.Marshal may or may not emit a trailing newline.
	if len(frontmatter) > 0 && frontmatter[len(frontmatter)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, []byte("---\n")...)
	if len(body) > 0 {
		out = append(out, body...)
	}
	return out
}

// patchYAMLAddDispatcherRole appends a dispatcher role to the
// roles list in the swarm YAML. Returns alreadyPresent=true when
// a role named "dispatcher" already exists — caller treats that
// as a successful no-op.
func patchYAMLAddDispatcherRole(data []byte, model string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse YAML: %w", err)
	}
	preserveBlockScalars(&doc)
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("expected mapping at YAML root")
	}

	rolesNode := yamlMappingValue(root, "roles")
	if rolesNode == nil {
		// No roles list yet — create one. Unusual for a real
		// swarm (the registry validator requires at least one
		// role), but defensively handled.
		rolesNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "roles", Tag: "!!str"},
			rolesNode,
		)
	}
	if rolesNode.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("roles is not a YAML sequence")
	}

	// Idempotency: scan existing roles for one named "dispatcher".
	for _, item := range rolesNode.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if name := yamlMappingValue(item, "name"); name != nil && name.Value == "dispatcher" {
			return data, true, nil
		}
	}

	// Build the new role node. yaml.v3 will choose appropriate
	// styles for the scalars; we explicitly tag the runtime image
	// so a future linter doesn't rewrite it.
	runtimeNode := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "image", Tag: "!!str"},
			{Kind: yaml.ScalarNode, Value: "noop:dispatcher", Tag: "!!str"},
		},
	}
	roleContent := []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "name", Tag: "!!str"},
		{Kind: yaml.ScalarNode, Value: "dispatcher", Tag: "!!str"},
	}
	if model != "" {
		roleContent = append(roleContent,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "model", Tag: "!!str"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: model, Tag: "!!str"},
		)
	}
	roleContent = append(roleContent,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "runtime", Tag: "!!str"},
		runtimeNode,
	)
	dispatcherRole := &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: roleContent,
	}
	// HeadComment surfaces in the YAML output so an operator
	// reading the file later understands why this role exists.
	dispatcherRole.HeadComment = "added by `vornikctl doctor --fix`: dispatcher cost attribution stub (telegram bot doesn't run as a container)"

	rolesNode.Content = append(rolesNode.Content, dispatcherRole)
	out, err := marshalYAML(&doc, detectYAMLIndent(data))
	if err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// patchYAMLAutonomyEnabled updates autonomy.enabled in raw YAML bytes using
// the yaml.v3 Node API so that all comments and other fields are preserved.
func patchYAMLAutonomyEnabled(data []byte, enabled bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	// Preserve block scalar styles before re-encoding to avoid yaml.v3
	// choosing plain or quoted style for multi-line strings that contain `:`
	// or other characters valid only inside `|` block scalars.
	preserveBlockScalars(&doc)
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("unexpected YAML structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping at YAML root")
	}

	val := "false"
	if enabled {
		val = "true"
	}

	// Find or create the autonomy: mapping node.
	autonomyNode := yamlMappingValue(root, "autonomy")
	if autonomyNode == nil {
		autonomyNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "autonomy", Tag: "!!str"},
			autonomyNode,
		)
	}

	// Find or create the enabled: scalar within autonomy.
	for i := 0; i+1 < len(autonomyNode.Content); i += 2 {
		if autonomyNode.Content[i].Value == "enabled" {
			autonomyNode.Content[i+1].Value = val
			autonomyNode.Content[i+1].Tag = "!!bool"
			autonomyNode.Content[i+1].Kind = yaml.ScalarNode
			return marshalYAML(&doc, detectYAMLIndent(data))
		}
	}

	// enabled key absent — prepend it inside autonomy.
	autonomyNode.Content = append(
		[]*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "enabled", Tag: "!!str"},
			{Kind: yaml.ScalarNode, Value: val, Tag: "!!bool"},
		},
		autonomyNode.Content...,
	)
	return marshalYAML(&doc, detectYAMLIndent(data))
}

// yamlMappingValue returns the value node for key in a yaml.MappingNode.
func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// detectYAMLIndent returns the indentation width used in data by finding the
// first indented non-empty line. Defaults to 2 if none is found.
func detectYAMLIndent(data []byte) int {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		spaces := 0
		for _, b := range line {
			if b == ' ' {
				spaces++
			} else {
				break
			}
		}
		if spaces > 0 && spaces <= 8 {
			return spaces
		}
	}
	return 2
}

// marshalYAML encodes a yaml.Node document back to bytes using the given indent.
func marshalYAML(doc *yaml.Node, indent int) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
