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
