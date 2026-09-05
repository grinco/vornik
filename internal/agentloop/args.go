package agentloop

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// args is the decoded tool arguments with the same reading the entrypoint's
// `jq -r '.key // default'` gave them: null, false and a missing key fall to
// the default; a string is itself; a number or true is its jq -r rendering.
type args map[string]any

func decodeArgs(raw json.RawMessage) args {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return args{}
	}
	return m
}

// str is `jq -r '.key // "<def>"'`: the "// empty" form is def == "".
func (a args) str(key, def string) string {
	v, ok := a[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if !t {
			return def
		}
		return "true"
	case float64:
		return jqNumber(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// boolFlag is the entrypoint's `[ "$(jq -r '.k // false')" = "true" ]` (bash)
// or `os.environ[...].lower() == "true"` (python) test: only a JSON true or
// the string "true" (any case) counts.
func (a args) boolFlag(key string) bool {
	return strings.EqualFold(a.str(key, "false"), "true")
}

// intOr is `int(jq -r '.k // <def>')` with python's fallback on ValueError.
func (a args) intOr(key string, def int) int {
	s := a.str(key, strconv.Itoa(def))
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// strList is `jq -c '.k // []'` read back as a list of strings. A non-array
// value is treated as absent.
func (a args) strList(key string) []string {
	v, ok := a[key]
	if !ok || v == nil {
		return nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		switch t := it.(type) {
		case string:
			out = append(out, t)
		default:
			b, _ := json.Marshal(t)
			out = append(out, string(b))
		}
	}
	return out
}

// jqNumber renders a JSON number the way jq -r prints it: integers without a
// fraction, others in shortest form.
func jqNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e17 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ---------------------------------------------------------------- python json

// pyJSON renders a value as python's json.dumps(indent=2) would: ", " and
// ": " separators, two-space indentation, ensure_ascii escaping of every
// non-ASCII character, and dict keys in insertion order (given as ordered
// pairs). The shell tools printed their JSON this way and the golden pins it.
type pyObject []pyField

type pyField struct {
	Key   string
	Value any
}

func pyJSON(v any) string {
	var b strings.Builder
	writePyJSON(&b, v, 0)
	return b.String()
}

func writePyJSON(b *strings.Builder, v any, depth int) {
	indent := func(d int) { b.WriteString(strings.Repeat("  ", d)) }
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case string:
		writePyString(b, t)
	case pyObject:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, f := range t {
			indent(depth + 1)
			writePyString(b, f.Key)
			b.WriteString(": ")
			writePyJSON(b, f.Value, depth+1)
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		indent(depth)
		b.WriteString("}")
	case []pyObject:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, o := range t {
			indent(depth + 1)
			writePyJSON(b, o, depth+1)
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		indent(depth)
		b.WriteString("]")
	default:
		fmt.Fprint(b, t)
	}
}

// writePyString escapes as json.dumps does with ensure_ascii=True.
func writePyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r >= 0x7f && r <= 0xffff):
				if r < 0x20 || r >= 0x7f {
					fmt.Fprintf(b, `\u%04x`, r)
				} else {
					b.WriteRune(r)
				}
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ------------------------------------------------------------------ strings

// runeSlice is python str slicing on a Go string: the first n characters.
func runeSlice(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// byteSliceOnRune cuts s at n bytes, backing up to a rune boundary — the
// design's D4: bash's %.Ns counted characters, the port counts bytes.
func byteSliceOnRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// relTo is os.path.relpath(path, root) for a path known to be under root.
func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
