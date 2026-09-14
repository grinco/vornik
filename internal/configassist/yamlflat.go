package configassist

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Flatten renders a YAML document as dotted key → scalar string, the shape
// the classifier compares before/after. Lists index by position, EXCEPT
// lists of mappings that carry a `slug` or `name` key, which index by that
// value so a feed keeps its identity when a neighbour is inserted (a
// positional index would make an insertion look like every later feed
// changed). Scalars render as their YAML text; nested mappings recurse.
// An unparseable document yields one key "<unparseable>" so the classifier
// treats it as E rather than as empty.
func Flatten(data []byte) map[string]string { return flatten(data, true) }

// FlattenPositional is Flatten with lists indexed by position only — the
// shape a duplicate-detection rule needs (identity indexing would collapse
// two feeds sharing a slug into one key).
func FlattenPositional(data []byte) map[string]string { return flatten(data, false) }

func flatten(data []byte, byIdentity bool) map[string]string {
	out := map[string]string{}
	if len(bytes.TrimSpace(data)) == 0 {
		return out
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		out["<unparseable>"] = err.Error()
		return out
	}
	if len(root.Content) == 0 {
		return out
	}
	flattenNode("", root.Content[0], out, byIdentity)
	return out
}

func flattenNode(prefix string, n *yaml.Node, out map[string]string, byIdentity bool) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenNode(key, n.Content[i+1], out, byIdentity)
		}
		if len(n.Content) == 0 && prefix != "" {
			out[prefix] = "{}"
		}
	case yaml.SequenceNode:
		if len(n.Content) == 0 && prefix != "" {
			out[prefix] = "[]"
			return
		}
		for i, item := range n.Content {
			idx := strconv.Itoa(i)
			if byIdentity && item.Kind == yaml.MappingNode {
				if id := mappingIdentity(item); id != "" {
					idx = id
				}
			}
			flattenNode(prefix+"."+idx, item, out, byIdentity)
		}
	case yaml.ScalarNode:
		out[prefix] = n.Value
	case yaml.AliasNode:
		if n.Alias != nil {
			flattenNode(prefix, n.Alias, out, byIdentity)
		}
	case yaml.DocumentNode:
		for _, c := range n.Content {
			flattenNode(prefix, c, out, byIdentity)
		}
	}
}

func mappingIdentity(n *yaml.Node) string {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if k == "slug" || k == "name" || k == "id" {
			if v := n.Content[i+1]; v.Kind == yaml.ScalarNode && v.Value != "" {
				return v.Value
			}
		}
	}
	return ""
}

// DiffFlat returns the key-level changes between two flattened documents,
// sorted by key. A key present only after has Before ""; only before has
// After "".
func DiffFlat(file string, before, after map[string]string) []Change {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var out []Change
	for k := range keys {
		b, a := before[k], after[k]
		if b == a {
			continue
		}
		out = append(out, Change{File: file, Key: k, Before: b, After: a})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// SplitFrontmatter splits a markdown document into its YAML frontmatter
// and body. A document without the leading `---` fence is all body.
func SplitFrontmatter(doc []byte) (frontmatter, body []byte, ok bool) {
	trimmed := bytes.TrimLeft(doc, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		return nil, doc, false
	}
	rest := trimmed[3:]
	rest = bytes.TrimLeft(rest, " \t\r")
	if !bytes.HasPrefix(rest, []byte("\n")) {
		return nil, doc, false
	}
	rest = rest[1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, doc, false
	}
	fm := rest[:end]
	after := rest[end+4:]
	if i := bytes.IndexByte(after, '\n'); i >= 0 {
		after = after[i+1:]
	} else {
		after = nil
	}
	return fm, after, true
}

// ChangesForFile derives the classifier's Change list for one op from the
// original and new bytes. YAML files flatten whole; markdown files flatten
// their frontmatter and treat a body change as one change on the key
// `<body>` (a workflow body is step prompts — B1 — and a project markdown
// body is descriptive prose — B2; the classifier reads the file path).
func ChangesForFile(rel string, before, after []byte) []Change {
	lower := strings.ToLower(rel)
	switch {
	case strings.HasSuffix(lower, ".yaml"), strings.HasSuffix(lower, ".yml"):
		return DiffFlat(rel, Flatten(before), Flatten(after))
	case strings.HasSuffix(lower, ".md"):
		bfm, bbody, _ := SplitFrontmatter(before)
		afm, abody, _ := SplitFrontmatter(after)
		changes := DiffFlat(rel, Flatten(bfm), Flatten(afm))
		if !bytes.Equal(bbody, abody) {
			key := "<body>"
			switch {
			case isProseDoc(lower):
				key = "PROJECT_CONTEXT.md" // classified B2 by name
			case strings.HasPrefix(lower, "workflows/"):
				key = "steps.<body>.prompt" // step prompts live in the body: B1
			case strings.HasPrefix(lower, "swarms/"):
				key = "roles.<body>.systemPrompt" // role prompts live in the body: B1
			}
			changes = append(changes, Change{File: rel, Key: key, Before: summarizeBody(bbody), After: summarizeBody(abody)})
		}
		return changes
	default:
		if bytes.Equal(before, after) {
			return nil
		}
		return []Change{{File: rel, Key: "<file>", Before: summarizeBody(before), After: summarizeBody(after)}}
	}
}

func isProseDoc(lower string) bool {
	base := lower
	if i := strings.LastIndex(lower, "/"); i >= 0 {
		base = lower[i+1:]
	}
	return strings.HasPrefix(lower, "projects/") && (base == "project_context.md" || base == "project.md" || base == "readme.md" || base == "backlog.md")
}

func summarizeBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + fmt.Sprintf("…(%d bytes)", len(s))
	}
	return s
}
