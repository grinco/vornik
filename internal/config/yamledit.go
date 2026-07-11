package config

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetYAMLKey updates a single scalar value identified by dottedKey (e.g.
// "instinct.consumers.application_feedback") inside content, preserving all
// comments, ordering, and surrounding structure.
//
// Missing keys are created: a missing leaf is appended to its parent
// mapping, and missing intermediate mappings are created along the path.
// It errors only if a path segment already exists but is not a mapping
// (cannot descend into a scalar). Appends go to the end of the parent
// mapping, so existing keys' comments and ordering are preserved.
//
// Supported val types: bool, string, int.
//
// The returned `created` bool reports whether the LEAF key was absent and
// had to be appended (true) versus updated in place (false). Callers that
// expect a key to already exist (a feature gate, an operator editing a
// known field) can use it to warn on a likely typo'd / unknown key, since
// a silent append produces a dead config entry that still parses cleanly.
func SetYAMLKey(content []byte, dottedKey string, val any) (out []byte, created bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, false, fmt.Errorf("yamledit: unmarshal: %w", err)
	}

	// Unmarshal wraps the real document in a DocumentNode.
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, fmt.Errorf("yamledit: unexpected document structure")
	}
	root := doc.Content[0]

	segments := strings.Split(dottedKey, ".")
	created, err = setInNode(root, segments, val)
	if err != nil {
		return nil, false, err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, fmt.Errorf("yamledit: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, false, fmt.Errorf("yamledit: marshal close: %w", err)
	}
	return buf.Bytes(), created, nil
}

// DeleteYAMLKey removes the node at dottedKey (e.g. "mcp_servers.homeassistant")
// and everything under it, preserving comments/ordering on the surrounding
// mapping. Returns removed=false (no error) when the key is absent. Errors only
// if a path segment exists but isn't a mapping. Comment-preserving counterpart
// to SetYAMLKey (control-plane hub MCP-remove path).
func DeleteYAMLKey(content []byte, dottedKey string) (out []byte, removed bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, false, fmt.Errorf("yamledit: unmarshal: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, fmt.Errorf("yamledit: unexpected document structure")
	}
	root := doc.Content[0]
	removed, err = deleteInNode(root, strings.Split(dottedKey, "."))
	if err != nil {
		return nil, false, err
	}
	if !removed {
		return content, false, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, fmt.Errorf("yamledit: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, false, fmt.Errorf("yamledit: marshal close: %w", err)
	}
	return buf.Bytes(), true, nil
}

// YAMLListField is one key/value of a list-item mapping, in emit order.
type YAMLListField struct {
	Key   string
	Value any // string | int | bool | []string (per setNodeValue)
}

// AppendYAMLListItem appends a mapping item built from fields (in order) to the
// YAML sequence at dottedKey (e.g. "mcp.servers"), preserving comments and
// ordering elsewhere. Missing intermediate mappings and a missing/empty/null
// sequence are created. Errors if a path segment exists but is not a mapping,
// or the target key exists but is a non-empty scalar (not a sequence).
//
// This is the list-shaped counterpart to SetYAMLKey: the daemon's MCP catalog
// lives at `mcp.servers` as a LIST of {name, transport, url, …} items, not a
// `mcp_servers.<name>` map — see the control-plane hub MCP add path.
func AppendYAMLListItem(content []byte, dottedKey string, fields []YAMLListField) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("yamledit: unmarshal: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("yamledit: unexpected document structure")
	}
	seq, err := ensureSequenceNode(doc.Content[0], strings.Split(dottedKey, "."))
	if err != nil {
		return nil, err
	}
	item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, f := range fields {
		kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.Key}
		vn := &yaml.Node{Kind: yaml.ScalarNode}
		if err := setNodeValue(vn, f.Value); err != nil {
			return nil, err
		}
		item.Content = append(item.Content, kn, vn)
	}
	seq.Content = append(seq.Content, item)
	return encodeYAMLDoc(&doc)
}

// RemoveYAMLListItemByField removes the first mapping item under the sequence at
// dottedKey whose `field` scalar equals value. removed=false (no error) when the
// sequence or a matching item is absent.
func RemoveYAMLListItemByField(content []byte, dottedKey, field, value string) (out []byte, removed bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, false, fmt.Errorf("yamledit: unmarshal: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, fmt.Errorf("yamledit: unexpected document structure")
	}
	seq := findSequenceNode(doc.Content[0], strings.Split(dottedKey, "."))
	if seq == nil {
		return content, false, nil
	}
	for i, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == field && item.Content[j+1].Value == value {
				seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
				b, encErr := encodeYAMLDoc(&doc)
				return b, encErr == nil, encErr
			}
		}
	}
	return content, false, nil
}

// UpsertYAMLListItemByField is the list-shaped upsert counterpart to
// SetYAMLKey: it replaces the first item in the sequence at dottedKey whose
// keyField scalar equals fields' own keyField entry, or appends a new item
// if no item matches. The one reusable add-or-replace primitive for
// list-shaped config edits — the control-plane hub's MCP-add path
// (internal/ui/admin_control_plane_mcp.go's mcpAddEdit) builds its
// ledger-proposal edit through it, so re-adding an existing server name
// replaces the entry instead of duplicating it.
//
// If fields has no entry for keyField, or that entry isn't a string, this
// falls back to a plain append (there is nothing to match against).
func UpsertYAMLListItemByField(content []byte, dottedKey, keyField string, fields []YAMLListField) ([]byte, error) {
	var keyValue string
	for _, f := range fields {
		if f.Key != keyField {
			continue
		}
		if s, ok := f.Value.(string); ok {
			keyValue = s
		}
		break
	}
	if keyValue != "" {
		out, _, err := RemoveYAMLListItemByField(content, dottedKey, keyField, keyValue)
		if err != nil {
			return nil, err
		}
		content = out
	}
	return AppendYAMLListItem(content, dottedKey, fields)
}

// SetYAMLListItemField updates ONE field on ONE list item — the first item in
// the sequence at dottedKey whose matchField scalar equals matchValue —
// preserving the item's other fields, comments, and the list order. The field
// is appended to the item when absent (same posture as SetYAMLKey's
// missing-leaf append). Unlike UpsertYAMLListItemByField (which replaces the
// whole item, dropping fields the caller didn't restate), this is the
// single-field surgical edit the control-plane actionizer needs
// (steps[].timeout, roles[].model, mcp.servers[].timeout_seconds — LLD
// 2026-07-11-control-plane-actionable-proposals §4.2).
//
// Errors when the sequence is absent (or a scalar), when no item matches, or
// on an unsupported value type — the actionizer treats every error as
// "degrade to informational", so absence must be loud, not a silent append.
func SetYAMLListItemField(content []byte, dottedKey, matchField, matchValue, field string, val any) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("yamledit: unmarshal: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("yamledit: unexpected document structure")
	}
	seq := findSequenceNode(doc.Content[0], strings.Split(dottedKey, "."))
	if seq == nil {
		return nil, fmt.Errorf("yamledit: %q is not an existing sequence", dottedKey)
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode || !mappingFieldEquals(item, matchField, matchValue) {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == field {
				if err := setNodeValue(item.Content[j+1], val); err != nil {
					return nil, err
				}
				return encodeYAMLDoc(&doc)
			}
		}
		// Field absent on the matched item — append it.
		kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}
		vn := &yaml.Node{Kind: yaml.ScalarNode}
		if err := setNodeValue(vn, val); err != nil {
			return nil, err
		}
		item.Content = append(item.Content, kn, vn)
		return encodeYAMLDoc(&doc)
	}
	return nil, fmt.Errorf("yamledit: no item in %q with %s=%q", dottedKey, matchField, matchValue)
}

// GetYAMLListItemField returns the scalar value of `field` on the first item
// in the sequence at dottedKey whose matchField equals matchValue. found=false
// when the sequence, the item, or the field is absent (or content is
// unparseable) — the read-side counterpart to SetYAMLListItemField.
func GetYAMLListItemField(content []byte, dottedKey, matchField, matchValue, field string) (value string, found bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return "", false
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", false
	}
	seq := findSequenceNode(doc.Content[0], strings.Split(dottedKey, "."))
	if seq == nil {
		return "", false
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode || !mappingFieldEquals(item, matchField, matchValue) {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == field && item.Content[j+1].Kind == yaml.ScalarNode {
				return item.Content[j+1].Value, true
			}
		}
		return "", false
	}
	return "", false
}

// mappingFieldEquals reports whether a mapping node has a scalar field with
// the given value.
func mappingFieldEquals(item *yaml.Node, field, value string) bool {
	for j := 0; j+1 < len(item.Content); j += 2 {
		if item.Content[j].Value == field && item.Content[j+1].Value == value {
			return true
		}
	}
	return false
}

// ensureSequenceNode walks segments[:len-1] as mappings (creating missing
// intermediates) then returns the sequence node at the final segment key,
// creating an empty sequence if the key is absent or holds an empty/null scalar.
func ensureSequenceNode(node *yaml.Node, segments []string) (*yaml.Node, error) {
	for i, seg := range segments {
		if node.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("yamledit: expected mapping at %q, got kind %d", seg, node.Kind)
		}
		last := i == len(segments)-1
		var val *yaml.Node
		for k := 0; k+1 < len(node.Content); k += 2 {
			if node.Content[k].Value == seg {
				val = node.Content[k+1]
				break
			}
		}
		if val == nil {
			kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: seg}
			if last {
				val = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			} else {
				val = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			node.Content = append(node.Content, kn, val)
		}
		if last {
			switch {
			case val.Kind == yaml.SequenceNode:
				return val, nil
			case val.Kind == yaml.ScalarNode && (val.Value == "" || val.Tag == "!!null"):
				val.Kind = yaml.SequenceNode
				val.Tag = "!!seq"
				val.Value = ""
				val.Content = nil
				return val, nil
			default:
				return nil, fmt.Errorf("yamledit: %q is not a sequence (kind %d)", seg, val.Kind)
			}
		}
		node = val
	}
	return nil, fmt.Errorf("yamledit: empty path")
}

// findSequenceNode returns the sequence node at dotted segments, or nil if any
// segment is absent or the resolved node is not a sequence.
func findSequenceNode(node *yaml.Node, segments []string) *yaml.Node {
	for _, seg := range segments {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var val *yaml.Node
		for k := 0; k+1 < len(node.Content); k += 2 {
			if node.Content[k].Value == seg {
				val = node.Content[k+1]
				break
			}
		}
		if val == nil {
			return nil
		}
		node = val
	}
	if node.Kind == yaml.SequenceNode {
		return node
	}
	return nil
}

// encodeYAMLDoc re-encodes a parsed document at 2-space indent.
func encodeYAMLDoc(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("yamledit: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yamledit: marshal close: %w", err)
	}
	return buf.Bytes(), nil
}

// deleteInNode walks mappings following segments and removes the leaf key/value
// pair from its parent mapping's Content. Returns removed=false when the key is
// absent at any level.
func deleteInNode(node *yaml.Node, segments []string) (bool, error) {
	if node.Kind != yaml.MappingNode {
		return false, fmt.Errorf("yamledit: expected mapping node, got kind %d", node.Kind)
	}
	key := segments[0]
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		if len(segments) == 1 {
			// Drop the [key, val] pair, preserving the rest of the block.
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return true, nil
		}
		return deleteInNode(node.Content[i+1], segments[1:])
	}
	return false, nil
}

// GetYAMLString returns the scalar string found at dottedKey (e.g.
// "database.host") inside content, or "" if the key is absent, the document
// is unparseable, or the resolved node is not a scalar. It is the read-side
// counterpart to SetYAMLKey and shares the same dotted-path convention.
//
// A scalar that the YAML decoder would interpret as a non-string (e.g. an
// int or bool) is returned as its plain textual form (the node's Value
// field), since callers in the migrate-ce path only feed the result into
// further string handling (placeholders, strconv.Atoi, defaults).
func GetYAMLString(content []byte, dottedKey string) string {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return ""
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return ""
	}
	node := doc.Content[0]
	for _, seg := range strings.Split(dottedKey, ".") {
		if node.Kind != yaml.MappingNode {
			return ""
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == seg {
				next = node.Content[i+1]
				break
			}
		}
		if next == nil {
			return ""
		}
		node = next
	}
	if node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

// setInNode recursively walks mapping nodes following segments and sets the
// leaf scalar's value. Returns created=true when the leaf key was absent and
// had to be appended (directly, or beneath a freshly-created intermediate
// mapping), and an error if any segment already exists but isn't a mapping.
func setInNode(node *yaml.Node, segments []string, val any) (created bool, err error) {
	if node.Kind != yaml.MappingNode {
		return false, fmt.Errorf("yamledit: expected mapping node, got kind %d", node.Kind)
	}

	key := segments[0]
	// MappingNode.Content is [key0, val0, key1, val1, ...]
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode.Value != key {
			continue
		}
		if len(segments) == 1 {
			// Leaf exists — update in place.
			return false, setNodeValue(valNode, val)
		}
		// Recurse into the next (existing) mapping.
		return setInNode(valNode, segments[1:], val)
	}

	// Key absent in this mapping — create it. Appending to Content keeps
	// existing keys (and their comments) untouched; the new key lands at
	// the end of the block.
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	if len(segments) == 1 {
		valNode := &yaml.Node{Kind: yaml.ScalarNode}
		if err := setNodeValue(valNode, val); err != nil {
			return false, err // unsupported type — created nothing
		}
		node.Content = append(node.Content, keyNode, valNode)
		return true, nil
	}
	// Missing intermediate — create an empty mapping and descend. The leaf
	// beneath a fresh mapping is necessarily new, so this path is created.
	childMap := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	node.Content = append(node.Content, keyNode, childMap)
	if _, err := setInNode(childMap, segments[1:], val); err != nil {
		return false, err
	}
	return true, nil
}

// setNodeValue updates node to hold val. Scalars (bool/string/int) become a
// ScalarNode with the matching tag; a []string becomes a SequenceNode of
// double-quoted string scalars. When val is a sequence, node is fully
// rewritten as a SequenceNode (its previous scalar Value/Tag are cleared) so
// an existing scalar leaf can be replaced by a list in place.
func setNodeValue(node *yaml.Node, val any) error {
	switch v := val.(type) {
	case bool:
		node.Kind = yaml.ScalarNode
		node.Tag = "!!bool"
		node.Content = nil
		if v {
			node.Value = "true"
		} else {
			node.Value = "false"
		}
	case string:
		node.Kind = yaml.ScalarNode
		node.Tag = "!!str"
		node.Content = nil
		node.Value = v
	case int:
		node.Kind = yaml.ScalarNode
		node.Tag = "!!int"
		node.Content = nil
		node.Value = strconv.Itoa(v)
	case []string:
		node.Kind = yaml.SequenceNode
		node.Tag = "!!seq"
		node.Value = ""
		node.Style = 0
		node.Content = make([]*yaml.Node, 0, len(v))
		for _, s := range v {
			node.Content = append(node.Content, &yaml.Node{
				Kind: yaml.ScalarNode,
				Tag:  "!!str",
				// Quote the value: api keys can contain '.' and other
				// glyphs that, while valid bare, read more safely quoted
				// and match the example's quoted-string convention.
				Style: yaml.DoubleQuotedStyle,
				Value: s,
			})
		}
	default:
		return fmt.Errorf("yamledit: unsupported value type %T", val)
	}
	return nil
}
