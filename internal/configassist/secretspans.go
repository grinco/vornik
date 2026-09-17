package configassist

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/secrethygiene"
)

// Secret redaction is driven by the YAML PARSER, not by a line regex.
//
// Two audits in a row found secrets reaching the model because the sanitiser
// matched TEXT: the first missed block scalars (`value: |` replaced the marker
// and left the body), and the fix for it still missed `value: &pem |` (an
// anchor between the key and the header) and `"value": |` (a quoted key that
// the key regex does not match at all). Each fix taught one more shape to a
// matcher that can never enumerate YAML's representation space — anchors,
// tags, quoting styles, folded vs literal, explicit indentation indicators,
// and combinations of all of them.
//
// So identification moved to the thing that already understands every one of
// those: the parser. `yaml.Node` reports the key's NAME already unquoted and
// untagged, which is the question the sanitiser actually needs answered, and
// its Line/Column say where the value sits in the original bytes.
//
// Redaction still edits TEXT, not a re-serialised tree, because
// re-materialisation must be byte-exact: a round-trip through yaml.Marshal
// would silently reformat an operator's file.

// secretSpan is one value to redact: the lines it occupies and where its text
// starts on the first of them.
type secretSpan struct {
	key       string
	startLine int // 1-based, the line carrying the key
	endLine   int // 1-based, inclusive; == startLine for a single-line value
	valueCol  int // 0-based byte offset on startLine where the VALUE text begins
}

// secretSpans locates every secret-bearing value in a YAML document.
// A document that does not parse yields (nil, false) and the caller falls
// back to the line-oriented pass, which still serves markdown frontmatter and
// non-YAML files.
func secretSpans(data []byte) ([]secretSpan, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) == 0 {
		return nil, false
	}
	lines := bytes.Split(data, []byte("\n"))
	var out []secretSpan
	var walk func(n *yaml.Node, inNamedSecrets bool)
	walk = func(n *yaml.Node, inNamedSecrets bool) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, inNamedSecrets)
			}
		case yaml.SequenceNode:
			for _, item := range n.Content {
				walk(item, inNamedSecrets)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				// k.Value is the DECODED key: quotes and tags are already
				// gone, which is the whole reason for parsing.
				if v.Kind == yaml.ScalarNode && isSecretBearing(inNamedSecrets, k.Value, v.Value) {
					if span, ok := spanFor(lines, k, v); ok {
						out = append(out, span)
						continue
					}
				}
				walk(v, inNamedSecrets || strings.EqualFold(k.Value, "named_secrets"))
			}
		case yaml.AliasNode:
			// An alias REFERS to a node redacted at its definition; expanding
			// it here would double-count. Its own text carries no secret.
		}
	}
	walk(&root, false)
	return out, true
}

// isSecretBearing is the single rule for "this field holds a secret",
// asked of a DECODED key and value.
func isSecretBearing(inNamedSecrets bool, key, value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || strings.HasPrefix(v, "${") || strings.HasPrefix(v, "$") {
		return false // env reference: a NAME, editable, never a value
	}
	if inNamedSecrets && strings.EqualFold(key, "value") {
		return true // design §10a: unconditional
	}
	if secrethygiene.IsSecretBearingName(key) {
		return true
	}
	return secrethygiene.LooksLikeRawSecret(v) && secrethygiene.IsSecretBearingName(strings.TrimSuffix(key, "_file"))
}

// spanFor computes the text extent of a value, given its key and value nodes.
//
// The extent runs from just after the key's `:` separator to the last line
// more indented than the key. That one rule covers a plain scalar, a quoted
// scalar, an anchored or tagged value, and every block-scalar variant,
// because all of them are "the key's line, plus any lines that belong to it
// by indentation".
func spanFor(lines [][]byte, k, v *yaml.Node) (secretSpan, bool) {
	start := k.Line - 1 // to 0-based
	if start < 0 || start >= len(lines) {
		return secretSpan{}, false
	}
	header := string(lines[start])
	sep := separatorAfterKey(header, k.Column-1)
	if sep < 0 {
		return secretSpan{}, false
	}
	// valueCol points at the first NON-SPACE after the separator, so the
	// header's original spacing stays in the prefix we keep and the span text
	// we store carries no leading blank. Storing the blank instead produced
	// `value:  |` on re-materialisation.
	valueCol := sep + 1
	for valueCol < len(header) && (header[valueCol] == ' ' || header[valueCol] == '\t') {
		valueCol++
	}
	keyIndent := k.Column - 1
	end := start
	for j := start + 1; j < len(lines); j++ {
		text := string(lines[j])
		if strings.TrimSpace(text) == "" {
			// A blank line belongs to the value only if the value continues
			// after it.
			continues := false
			for m := j + 1; m < len(lines); m++ {
				if strings.TrimSpace(string(lines[m])) == "" {
					continue
				}
				continues = indentWidth(string(lines[m])) > keyIndent
				break
			}
			if !continues {
				break
			}
			continue
		}
		if indentWidth(text) <= keyIndent {
			break
		}
		end = j
	}
	_ = v
	return secretSpan{key: k.Value, startLine: start + 1, endLine: end + 1, valueCol: valueCol}, true
}

// separatorAfterKey returns the byte index of the `:` that ends the key,
// starting the scan at the key's column and skipping any quoted key text.
func separatorAfterKey(line string, keyCol int) int {
	if keyCol < 0 || keyCol > len(line) {
		return -1
	}
	i := keyCol
	if i < len(line) && (line[i] == '"' || line[i] == '\'') {
		quote := line[i]
		i++
		for i < len(line) {
			if line[i] == '\\' && quote == '"' {
				i += 2
				continue
			}
			if line[i] == quote {
				i++
				break
			}
			i++
		}
	}
	for ; i < len(line); i++ {
		if line[i] == ':' {
			return i
		}
	}
	return -1
}

// residualSecret re-parses SANITISED bytes and reports the first
// secret-bearing value that is not a placeholder — the post-condition that
// makes this safe against representations nobody has thought of yet.
//
// A representation the span walker cannot locate is a representation whose
// secret would ship to a model. Refusing is the only honest outcome: a
// sanitiser that silently passes what it did not understand reports "clean"
// and means "not examined".
func residualSecret(sanitized []byte) (string, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal(sanitized, &root); err != nil || len(root.Content) == 0 {
		return "", false // unparseable output is handled by the caller's own gate
	}
	var found string
	var walk func(n *yaml.Node, inNamedSecrets bool)
	walk = func(n *yaml.Node, inNamedSecrets bool) {
		if n == nil || found != "" {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, inNamedSecrets)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if v.Kind == yaml.ScalarNode && isSecretBearing(inNamedSecrets, k.Value, v.Value) {
					if !strings.Contains(v.Value, placeholderPrefix) {
						found = k.Value
						return
					}
					continue
				}
				walk(v, inNamedSecrets || strings.EqualFold(k.Value, "named_secrets"))
			}
		}
	}
	walk(&root, false)
	return found, found != ""
}
