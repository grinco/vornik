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

// Expansion bounds (audit 2026-09-15 CA-02). A YAML document is a GRAPH, not
// a tree: `&loop [*loop]` is a finite document whose expansion never ends,
// and the "billion laughs" shape expands exponentially with no cycle at all.
// Unbounded recursion here was a fatal stack overflow — which Go does not let
// a recover() handler contain, and which the class ceiling (applied AFTER
// flattening) never got the chance to refuse. Both a cycle check and a size
// bound are needed; neither alone is sufficient.
const (
	maxFlattenKeys  = 5000
	maxFlattenDepth = 100
)

// Sentinel keys. Every one begins with "<", which classifyOne refuses as E —
// a document the flattener could not expand must be DENIED, never silently
// read as an empty (and therefore changeless) document.
const (
	keyUnparseable   = "<unparseable>"
	keyCyclicAlias   = "<cyclic-alias>"
	keyTooManyKeys   = "<expansion-too-large>"
	keyTooDeeplyNest = "<nesting-too-deep>"
)

func flatten(data []byte, byIdentity bool) map[string]string {
	out := map[string]string{}
	if len(bytes.TrimSpace(data)) == 0 {
		return out
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		out[keyUnparseable] = err.Error()
		return out
	}
	if len(root.Content) == 0 {
		return out
	}
	f := &flattener{out: out, byIdentity: byIdentity, onPath: map[*yaml.Node]bool{}}
	f.node("", root.Content[0], 0)
	return out
}

// flattener carries the traversal state the bounds need. onPath holds the
// alias targets currently being expanded on THIS branch (not every node ever
// visited): the same anchor referenced twice as siblings is legitimate reuse,
// while an anchor that contains a reference to itself is the cycle.
type flattener struct {
	out        map[string]string
	byIdentity bool
	onPath     map[*yaml.Node]bool
	stopped    bool
}

// budget reports whether traversal may continue, recording the reason it may
// not. Once stopped, the sentinel is already in out and every caller unwinds.
func (f *flattener) budget(depth int) bool {
	if f.stopped {
		return false
	}
	if depth > maxFlattenDepth {
		f.out[keyTooDeeplyNest] = fmt.Sprintf("document nests deeper than %d levels", maxFlattenDepth)
		f.stopped = true
		return false
	}
	if len(f.out) > maxFlattenKeys {
		f.out[keyTooManyKeys] = fmt.Sprintf("document expands past %d keys", maxFlattenKeys)
		f.stopped = true
		return false
	}
	return true
}

func (f *flattener) node(prefix string, n *yaml.Node, depth int) {
	if n == nil || !f.budget(depth) {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			f.node(key, n.Content[i+1], depth+1)
		}
		if len(n.Content) == 0 && prefix != "" {
			f.out[prefix] = "{}"
		}
	case yaml.SequenceNode:
		if len(n.Content) == 0 && prefix != "" {
			f.out[prefix] = "[]"
			return
		}
		for i, item := range n.Content {
			idx := strconv.Itoa(i)
			if f.byIdentity && item != nil && item.Kind == yaml.MappingNode {
				if id := mappingIdentity(item); id != "" {
					idx = id
				}
			}
			f.node(prefix+"."+idx, item, depth+1)
		}
	case yaml.ScalarNode:
		f.out[prefix] = n.Value
	case yaml.AliasNode:
		if n.Alias == nil {
			return
		}
		if f.onPath[n.Alias] {
			f.out[keyCyclicAlias] = "alias expands into itself at " + prefix
			f.stopped = true
			return
		}
		f.onPath[n.Alias] = true
		f.node(prefix, n.Alias, depth+1)
		delete(f.onPath, n.Alias)
	case yaml.DocumentNode:
		for _, c := range n.Content {
			f.node(prefix, c, depth+1)
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
