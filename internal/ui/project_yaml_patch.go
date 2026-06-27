package ui

// Surgical yaml.Node patcher used by the form-driven project
// config editor (Phase 1B v1 of the web-authoring UX work). The
// existing YAML editor at /ui/projects/{id}/config keeps the raw
// textarea; the form editor at /ui/projects/{id}/config/form
// routes through this patcher so saving a form-edited field
// preserves all comments, the order of unrelated keys, and any
// commented-out documentation blocks the bundled YAMLs lean on.
//
// Why not unmarshal-into-struct-then-marshal? Because that path
// loses every comment and every commented-out scaffold the
// bundled assistant-project.yaml / dev-project.yaml rely on for
// operator legibility. Surgical edits keep those intact.

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/fieldguard"
	"vornik.io/vornik/internal/projectarchive"
)

// lifecyclePatchGuard is the field-allowlist for project-lifecycle
// writes (archive / unarchive / delete-now). Lifecycle operations must
// only ever touch the top-level `lifecycle:` key — never project config
// or identity (projectId, swarmId, autonomy, …). The guard is the
// chokepoint enforcing that on both the UI helper (applyLifecyclePatches)
// and the shared REST patcher (LifecyclePatcher), so a typo'd or
// accidentally-added patch can't ride the archive path into a protected
// field. See internal/fieldguard.
var lifecyclePatchGuard = fieldguard.Allowlist("lifecycle")

// gitPatchGuard is the field-allowlist for the git-over-HTTPS access toggle
// (ProjectGitToggle). The toggle may only ever touch the top-level `git:`
// key — never project config or identity — so a stray patch can't ride the
// toggle path into a protected field. See internal/fieldguard and
// https://docs.vornik.io
var gitPatchGuard = fieldguard.Allowlist("git")

// topLevelPatchKeys returns the first path segment of each patch (the
// top-level YAML key it targets), preserving order. Used to feed a
// fieldguard before applying a patch set.
func topLevelPatchKeys(patches []yamlPatch) []string {
	keys := make([]string, 0, len(patches))
	for _, p := range patches {
		if len(p.Path) > 0 {
			keys = append(keys, p.Path[0])
		}
	}
	return keys
}

// LifecycleServiceAdapter wraps a *projectarchive.LifecycleService
// so it satisfies the UI's ArchiveLifecycle interface (which takes
// the UI-package types rather than the projectarchive types to
// keep the UI's public surface independent of the lower-level
// package).
//
// Service container instantiates one and passes via
// WithArchiveLifecycle alongside the REST API's WithArchiveService.
type LifecycleServiceAdapter struct {
	Service *projectarchive.LifecycleService
}

// Archive forwards to the underlying service.
func (a *LifecycleServiceAdapter) Archive(ctx context.Context, projectID string, in ArchiveLifecycleInput) (ArchiveLifecycleSnapshot, error) {
	snap, err := a.Service.Archive(ctx, projectID, projectarchive.ArchiveInput{
		Grace: in.Grace, Reason: in.Reason, Principal: in.Principal,
	})
	if err != nil {
		return ArchiveLifecycleSnapshot{}, err
	}
	return ArchiveLifecycleSnapshot{
		Status:            snap.Status,
		ArchivedAt:        snap.ArchivedAt,
		ScheduledDeleteAt: snap.ScheduledDeleteAt,
		Reason:            snap.Reason,
		ArchivedBy:        snap.ArchivedBy,
	}, nil
}

// Unarchive forwards to the underlying service.
func (a *LifecycleServiceAdapter) Unarchive(ctx context.Context, projectID string) error {
	return a.Service.Unarchive(ctx, projectID)
}

// ScheduleDeleteNow forwards to the underlying service.
func (a *LifecycleServiceAdapter) ScheduleDeleteNow(ctx context.Context, projectID string, isArchived bool) error {
	return a.Service.ScheduleDeleteNow(ctx, projectID, isArchived)
}

// LifecyclePatcher exposes the UI package's YAML patcher as the
// shape projectarchive.LifecycleService expects. Used by the
// service container to wire one shared lifecycle service across
// UI handlers and the REST API without forcing the projectarchive
// package to depend on internal/ui.
func LifecyclePatcher() projectarchive.YAMLPatcher {
	return func(content []byte, patches []projectarchive.PatchOp) ([]byte, error) {
		uiPatches := make([]yamlPatch, 0, len(patches))
		for _, p := range patches {
			uiPatches = append(uiPatches, yamlPatch{
				Path:          p.Path,
				Value:         p.Value,
				RemoveIfEmpty: p.RemoveIfEmpty,
			})
		}
		if err := lifecyclePatchGuard.Check(topLevelPatchKeys(uiPatches)); err != nil {
			return nil, fmt.Errorf("lifecycle patch refused: %w", err)
		}
		return applyYAMLPatches(content, uiPatches)
	}
}

// yamlPatch is one surgical update to a project YAML.
//
// Path walks into nested mappings — e.g. ["autonomy", "goal"]
// targets `autonomy.goal:`. Missing intermediate mappings are
// created so a brand-new section can be introduced from the form.
//
// Value drives the leaf node:
//   - string: emitted as a string scalar; multi-line strings use
//     YAML literal style ('|') so newlines survive the round-trip.
//   - bool / int: emitted as the matching scalar.
//   - []string: emitted as a flow-or-block sequence; the patcher
//     uses block style to keep the file diff-readable.
//
// RemoveIfEmpty deletes the key entirely when the value is the
// zero value of its type. Used for "clear this optional field"
// without leaving `key: ""` litter in the YAML.
type yamlPatch struct {
	Path          []string
	Value         any
	RemoveIfEmpty bool
}

// applyYAMLPatches parses content as YAML, applies each patch in
// order, and returns the rendered YAML. Original comments,
// commented-out lines, and unmodified keys are preserved.
//
// Returns an error on YAML parse failures or when a patch
// targets a path through a non-mapping node (e.g.
// ["autonomy", "goal"] when `autonomy:` happens to be a string).
// Those cases are operator config breakage that the form path
// shouldn't paper over.
func applyYAMLPatches(content []byte, patches []yamlPatch) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("parse project yaml: %w", err)
	}
	// yaml.Unmarshal wraps the document in a DocumentNode; the
	// actual root mapping is its first child. An empty file
	// yields a zero-value Node with no children — synthesise a
	// document + mapping so patches can land.
	if root.Kind == 0 {
		root = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{
				{Kind: yaml.MappingNode},
			},
		}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("project yaml has unexpected top-level shape")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("project yaml top-level must be a mapping, got kind %d", doc.Kind)
	}

	for _, p := range patches {
		if len(p.Path) == 0 {
			return nil, fmt.Errorf("patch with empty path")
		}
		if err := applyOnePatch(doc, p); err != nil {
			return nil, err
		}
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("encode patched yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return out.Bytes(), nil
}

func applyOnePatch(rootMapping *yaml.Node, p yamlPatch) error {
	parent := rootMapping
	// Walk every Path segment except the last, creating mappings
	// along the way. The last segment is the leaf we set / remove.
	for i := 0; i < len(p.Path)-1; i++ {
		segment := p.Path[i]
		keyNode, valNode := findMappingChild(parent, segment)
		if keyNode == nil {
			// Intermediate key absent — only synthesise if the
			// patch actually has a non-empty value to write,
			// otherwise we'd be creating empty parent maps for
			// no reason.
			if patchIsEmpty(p) {
				return nil
			}
			newKey := &yaml.Node{Kind: yaml.ScalarNode, Value: segment}
			newMap := &yaml.Node{Kind: yaml.MappingNode}
			parent.Content = append(parent.Content, newKey, newMap)
			parent = newMap
			continue
		}
		if valNode.Kind != yaml.MappingNode {
			return fmt.Errorf("path segment %q is not a mapping (kind=%d); refusing to overwrite", segment, valNode.Kind)
		}
		parent = valNode
	}

	leafKey := p.Path[len(p.Path)-1]
	if p.RemoveIfEmpty && patchIsEmpty(p) {
		removeMappingChild(parent, leafKey)
		return nil
	}

	valueNode, err := scalarOrSequenceNode(p.Value)
	if err != nil {
		return fmt.Errorf("path %v: %w", p.Path, err)
	}

	keyNode, existingVal := findMappingChild(parent, leafKey)
	if keyNode == nil {
		parent.Content = append(parent.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: leafKey},
			valueNode,
		)
		return nil
	}
	// Replace the existing value in place. Copy the head/foot
	// comments off the prior value so an in-line `# foo` survives
	// the rewrite — operators who scroll the diff after save
	// shouldn't see comment loss they didn't ask for.
	valueNode.HeadComment = existingVal.HeadComment
	valueNode.LineComment = existingVal.LineComment
	valueNode.FootComment = existingVal.FootComment
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i] == keyNode {
			parent.Content[i+1] = valueNode
			return nil
		}
	}
	// Defensive: keyNode came from parent.Content; we shouldn't
	// reach here. If we do, the mapping is malformed.
	return fmt.Errorf("path %v: failed to locate key node during replace", p.Path)
}

// applyYAMLSequenceElementPatches targets a single element of a
// top-level sequence (e.g. swarm.roles) addressed by an id-key
// match (e.g. name=lead). It then applies the given patches to
// THAT ELEMENT — patch paths are rooted at the element, not at
// the document. Used by the swarm editor's per-role frontmatter
// editing path; the same pattern would extend to any other
// "sequence of mappings keyed by an id field" shape.
//
// Comments, sibling sequence elements, and unrelated top-level
// keys are preserved verbatim — same guarantee as
// applyYAMLPatches.
//
// Errors:
//   - sequenceKey absent or not a sequence: structural mismatch.
//   - no element with idKey == idValue: operator error (typo or
//     stale form pointing at a deleted element).
//   - any patch's own validation failure propagates.
func applyYAMLSequenceElementPatches(content []byte, sequenceKey, idKey, idValue string, patches []yamlPatch) ([]byte, error) {
	if len(patches) == 0 {
		return content, nil
	}

	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("parse project yaml: %w", err)
	}
	if root.Kind == 0 {
		return nil, fmt.Errorf("project yaml is empty — cannot patch sequence %q", sequenceKey)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("project yaml has unexpected top-level shape")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("project yaml top-level must be a mapping, got kind %d", doc.Kind)
	}

	_, seqNode := findMappingChild(doc, sequenceKey)
	if seqNode == nil {
		return nil, fmt.Errorf("sequence key %q not found at the top level", sequenceKey)
	}
	if seqNode.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("key %q is not a sequence (kind=%d)", sequenceKey, seqNode.Kind)
	}

	// Find the element whose idKey scalar equals idValue.
	var target *yaml.Node
	for _, elem := range seqNode.Content {
		if elem.Kind != yaml.MappingNode {
			continue
		}
		_, idValNode := findMappingChild(elem, idKey)
		if idValNode != nil && idValNode.Value == idValue {
			target = elem
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("no element in %q has %s=%q", sequenceKey, idKey, idValue)
	}

	for _, p := range patches {
		if err := applyOnePatch(target, p); err != nil {
			return nil, err
		}
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("encode patched yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return out.Bytes(), nil
}

// findMappingChild returns the key + value node pair for the
// named child of a mapping, or (nil, nil) when absent.
func findMappingChild(mapping *yaml.Node, name string) (*yaml.Node, *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k.Value == name {
			return k, mapping.Content[i+1]
		}
	}
	return nil, nil
}

// removeMappingChild deletes the key/value pair for name, no-op
// when absent.
func removeMappingChild(mapping *yaml.Node, name string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// patchIsEmpty reports whether the patch's value is the zero
// value of its type. Used by RemoveIfEmpty to decide between
// "set to zero" (write `key: 0`) and "absent" (delete key).
func patchIsEmpty(p yamlPatch) bool {
	switch v := p.Value.(type) {
	case string:
		return strings.TrimSpace(v) == ""
	case bool:
		return !v
	case int:
		return v == 0
	case int64:
		return v == 0
	case float64:
		return v == 0
	case []string:
		return len(v) == 0
	case []map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

// scalarOrSequenceNode builds a yaml.Node for the given Go value.
// Strings with newlines use literal block style ('|') so the
// rendered YAML stays readable for prose fields like
// autonomy.goal / description.
func scalarOrSequenceNode(value any) (*yaml.Node, error) {
	switch v := value.(type) {
	case string:
		n := &yaml.Node{Kind: yaml.ScalarNode, Value: v}
		if strings.Contains(v, "\n") {
			n.Style = yaml.LiteralStyle
		} else {
			n.Style = yaml.DoubleQuotedStyle
		}
		return n, nil
	case bool:
		n := &yaml.Node{Kind: yaml.ScalarNode}
		if v {
			n.Value = "true"
		} else {
			n.Value = "false"
		}
		return n, nil
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", v)}, nil
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", v)}, nil
	case float64:
		// Use %v so integral floats render as "20" — matches Go's
		// strconv.FormatFloat shortest-roundtrip behaviour. yaml.v3
		// parses "20" back as a float when the field is typed float,
		// so the round-trip is clean even when the integer
		// representation is shorter. Operators reading the YAML see
		// the same number Go would emit.
		return &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%v", v)}, nil
	case []string:
		// Style 0 is block style for sequences — yaml.v3's default
		// when no FlowStyle is requested. Keeps the rendered file
		// diff-readable (one item per line).
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range v {
			seq.Content = append(seq.Content, &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: item,
				Style: yaml.DoubleQuotedStyle,
			})
		}
		return seq, nil
	case []map[string]any:
		// Array-of-mappings — used by the MCP servers form. Each
		// map becomes one sequence element; map keys are emitted in
		// the order they appear in the slice's "keys" sibling (see
		// orderedMappingFromMap). Block style for diff-readability.
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range v {
			mapNode, err := orderedMappingFromMap(item)
			if err != nil {
				return nil, err
			}
			seq.Content = append(seq.Content, mapNode)
		}
		return seq, nil
	default:
		return nil, fmt.Errorf("unsupported patch value type %T", value)
	}
}

// orderedMappingFromMap builds a yaml.Node mapping from a Go map.
// The map iteration order is non-deterministic in Go, so to keep
// the rendered YAML stable across saves the mapping uses the
// special "_order" key (if present) to drive the emit order. Keys
// not in "_order" are appended in alphabetical order so the file
// stays diff-clean turn-after-turn.
//
// Empty string values are skipped — operators clearing an optional
// field shouldn't leave `url: ""` litter on the rendered server
// entry. Empty []string values are likewise dropped.
func orderedMappingFromMap(m map[string]any) (*yaml.Node, error) {
	mapNode := &yaml.Node{Kind: yaml.MappingNode}
	if m == nil {
		return mapNode, nil
	}
	var orderedKeys []string
	if rawOrder, ok := m["_order"]; ok {
		if oks, ok := rawOrder.([]string); ok {
			orderedKeys = oks
		}
	}
	seen := make(map[string]bool, len(m))
	emit := func(key string) error {
		if seen[key] || key == "_order" {
			return nil
		}
		val, present := m[key]
		if !present {
			return nil
		}
		// Skip "empty" optional fields so we don't write
		// `command: ""` / `args: []` on entries that don't use them.
		switch v := val.(type) {
		case string:
			if v == "" {
				seen[key] = true
				return nil
			}
		case []string:
			if len(v) == 0 {
				seen[key] = true
				return nil
			}
		}
		valNode, err := scalarOrSequenceNode(val)
		if err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
		mapNode.Content = append(mapNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			valNode,
		)
		seen[key] = true
		return nil
	}
	for _, k := range orderedKeys {
		if err := emit(k); err != nil {
			return nil, err
		}
	}
	// Alphabetical fallback for keys not pre-ordered. Deterministic
	// so successive saves diff cleanly.
	var rest []string
	for k := range m {
		if !seen[k] && k != "_order" {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	for _, k := range rest {
		if err := emit(k); err != nil {
			return nil, err
		}
	}
	return mapNode, nil
}

// sortStrings is a thin wrapper over sort.Strings, kept as a local
// helper so the test file can stub it if a sort regression ever
// surfaces. (Standard library sort is rock-solid; this is just a
// readability convenience to keep the patcher self-contained.)
func sortStrings(s []string) {
	// Insertion sort — keys-per-mapping count is tiny (≤10 for the
	// MCP server entries we emit), so the n² cost is invisible and
	// we avoid importing "sort" into this file just for this.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
