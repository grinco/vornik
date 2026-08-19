package contractreg

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/agenttools"
)

// AddAgentToolsGo records internal/agenttools.builtinTools BY IMPORT rather
// than by parsing the Go source — agenttools already exports Names(), so this
// surface is immune to reformatting.
func (t *Table) AddAgentToolsGo() {
	for _, n := range agenttools.Names() {
		t.Add(KindAgentToolGo, n, "internal/agenttools/agenttools.go")
	}
}

const agentMCPPrefix = "mcp__"

var (
	// BUILTIN_TOOL_NAMES_JSON='["a","b",...]' — a JSON array inside a
	// single-quoted shell assignment.
	reAdvertised = regexp.MustCompile(`BUILTIN_TOOL_NAMES_JSON='(\[[^']*\])'`)
	// A shell case label: leading indent, one or more |-separated bare words,
	// a close paren, then either end-of-line or the body on the same line.
	//
	// Both forms occur in the real file and the parser must handle both:
	// exec_tool puts the body on following lines (`memory_search)`), while
	// is_builtin_tool packs it inline (`file_read|file_write) return 0 ;;`).
	// Requiring end-of-line missed every gate entry and made the security check
	// vacuously green — caught only because AddEntrypointSurfaces errors on an
	// empty extraction.
	//
	// Still deliberately strict about the LABEL: bare lowercase identifiers
	// only, so shell globs in the same switches (`-*)`, `""|*[!0-9]*)`,
	// `"$WORKSPACE"/.tool_results/*)`) cannot be harvested as tool names.
	reCaseLabel = regexp.MustCompile(`^\s+([a-z0-9_]+(?:\|[a-z0-9_]+)*)\)(?:\s|$)`)
	// Function openers we care about, e.g. `is_builtin_tool() {`.
	reFuncOpen = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)
	// Inline exemptions on the execution gate:
	//   [ "$name" != "tool_search" ] && [ "$name" != "tool_result_read" ] && …
	// Matched per-occurrence rather than per-line so a gate carrying several
	// exemptions yields all of them. Anchored on the `$name !=` comparison so an
	// unrelated string test elsewhere in the file cannot be harvested as an
	// exemption.
	reInlineExempt = regexp.MustCompile(`"\$name"\s*!=\s*"([a-z0-9_]+)"`)
)

// AddEntrypointSurfaces parses the three agent-tool registries that live in
// images/vornik-agent/entrypoint.sh:
//
//   - BUILTIN_TOOL_NAMES_JSON  → KindAgentToolAdvertised (advertisement filter)
//   - is_builtin_tool() cases  → KindAgentToolGate       (execution allowlist gate)
//   - exec_tool() cases        → KindAgentToolDispatch   (what can actually run)
//
// These are the unavoidable text parses. They are also the highest-value
// surfaces, because disagreement among them is a privilege bypass rather than a
// cosmetic drift (see CheckAgentToolAgreement). A parse that silently extracted
// nothing would make the check vacuously green, so an empty result for any of
// the three is an error, not an empty set.
func (t *Table) AddEntrypointSurfaces(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read entrypoint: %w", err)
	}
	body := string(raw)

	if m := reAdvertised.FindStringSubmatch(body); m != nil {
		var names []string
		if err := json.Unmarshal([]byte(m[1]), &names); err != nil {
			return fmt.Errorf("parse BUILTIN_TOOL_NAMES_JSON: %w", err)
		}
		for _, n := range names {
			t.Add(KindAgentToolAdvertised, n, path+":BUILTIN_TOOL_NAMES_JSON")
		}
	}

	// Inline exemptions on the execution gate. Scanned over the WHOLE body
	// rather than inside a function, because the gate is an `if` in the tool
	// loop rather than a named function. Anchored on `"$name" != "..."` so only
	// that comparison contributes.
	for _, m := range reInlineExempt.FindAllStringSubmatchIndex(body, -1) {
		name := body[m[2]:m[3]]
		line := 1 + strings.Count(body[:m[0]], "\n")
		t.Add(KindAgentToolInlineExempt, name, fmt.Sprintf("%s:%d", path, line))
	}

	// Walk line by line tracking which function we are inside, so case labels
	// are attributed to the right registry. Shell has no block structure we can
	// rely on beyond the closing `}` at column 0, which both target functions
	// use.
	var fn string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if m := reFuncOpen.FindStringSubmatch(text); m != nil {
			fn = m[1]
			continue
		}
		if text == "}" {
			fn = ""
			continue
		}
		var kind Kind
		switch fn {
		case "is_builtin_tool":
			kind = KindAgentToolGate
		case "exec_tool":
			kind = KindAgentToolDispatch
		default:
			continue
		}
		m := reCaseLabel.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		for _, name := range strings.Split(m[1], "|") {
			t.Add(kind, name, fmt.Sprintf("%s:%d", path, line))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan entrypoint: %w", err)
	}

	for _, kind := range []Kind{KindAgentToolAdvertised, KindAgentToolGate, KindAgentToolDispatch} {
		if len(t.Names(kind)) == 0 {
			return fmt.Errorf("extracted zero names for %s from %s — the shell's shape "+
				"changed and this parser is now blind; fix the parser rather than the check", kind, path)
		}
	}
	return nil
}
