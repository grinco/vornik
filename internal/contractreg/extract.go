package contractreg

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/agentloop"
	"vornik.io/vornik/internal/agenttools"
)

// AddAgentToolsGo records internal/agenttools.builtinTools BY IMPORT rather
// than by parsing the Go source — agenttools already exports Names(), so this
// surface is immune to reformatting.
func (t *Table) AddAgentToolsGo() {
	for _, n := range agenttools.Names() {
		t.Add(KindAgentToolGo, n, "internal/agenttools/agenttools.go")
	}
	// The Go side is two surfaces since 2026-09-05: the declaration and the
	// helper's dispatch table. Every consumer that asks "can this run?" needs
	// both, so they arrive together.
	t.AddAgentLoopHandlers(agentloop.HandlerNames())
}

const agentMCPPrefix = "mcp__"

// ToolRegistryFile is the generated registry's basename, beside the entrypoint.
const ToolRegistryFile = "tool_registry.generated.sh"

var (
	// BUILTIN_TOOL_NAMES_JSON='["a","b",...]' — a JSON array inside a
	// single-quoted shell assignment. Generated into the registry file; the
	// same regex applied to the entrypoint body detects an inline second copy.
	reAdvertised = regexp.MustCompile(`BUILTIN_TOOL_NAMES_JSON='(\[[^']*\])'`)
	// UNGATED_TOOL_NAMES_JSON / UNGATED_TOOL_PREFIXES_JSON — the shell's mirrors
	// of the declaration's exemptions, same single-quoted shape.
	reUngatedNames    = regexp.MustCompile(`UNGATED_TOOL_NAMES_JSON='(\[[^']*\])'`)
	reUngatedPrefixes = regexp.MustCompile(`UNGATED_TOOL_PREFIXES_JSON='(\[[^']*\])'`)
	// ADVERTISE_TOKENS_JSON='["always",...]' — the closed token set.
	reAdvertiseTokens = regexp.MustCompile(`ADVERTISE_TOKENS_JSON='(\[[^']*\])'`)
	reHelperNames     = regexp.MustCompile(`HELPER_TOOL_NAMES_JSON='(\[[^']*\])'`)
	// The registry array rides a quoted heredoc (definitions carry apostrophes).
	reToolRegistry = regexp.MustCompile(`(?s)read -r -d '' TOOL_REGISTRY_JSON <<'TOOL_REGISTRY_EOF' \|\| true\n(.*?)\nTOOL_REGISTRY_EOF\n`)
	// Any registry variable DECLARED (assigned) in a body — used on the
	// entrypoint to detect a second copy beside the sourced one.
	reRegistryDeclared = regexp.MustCompile(`(?m)^\s*(?:BUILTIN_TOOL_NAMES_JSON|UNGATED_TOOL_NAMES_JSON|UNGATED_TOOL_PREFIXES_JSON|ADVERTISE_TOKENS_JSON|HELPER_TOOL_NAMES_JSON)=|read -r -d '' TOOL_REGISTRY_JSON`)
	// The entrypoint sources the registry file.
	reRegistrySourced = regexp.MustCompile(`(?m)^\s*source\s+"?\$\{VORNIK_TOOL_REGISTRY:-.*tool_registry\.generated\.sh\}"?`)
	// tool_definitions()'s fail-closed filter: exempt, or declared AND allowed.
	// Matched loosely on purpose — what must hold is the three-way shape, not
	// the jq spelling.
	reFailClosedFilter = regexp.MustCompile(`\(\$exempt \| index\(\$name\) != null\) or \(\(\$builtin \| index\(\$name\) != null\) and \(\$allowed \| index\(\$name\) != null\)\)`)
	// tool_definition_for <name> — a definition appended by name.
	reDefinitionFor = regexp.MustCompile(`tool_definition_for\s+([a-z0-9_]+)`)
	// A shell case label: leading indent, one or more |-separated bare words,
	// a close paren, then either end-of-line or the body on the same line.
	//
	// Both forms occur in the real file and the parser must handle both:
	// exec_tool puts the body on following lines (`memory_search)`), while
	// tool_advertised_now packs it inline (`always) return 0 ;;`). Requiring
	// end-of-line once missed every gate entry and made the security check
	// vacuously green — caught only because AddEntrypointSurfaces errors on an
	// empty extraction.
	//
	// Still deliberately strict about the LABEL: bare lowercase identifiers
	// only, so shell globs in the same switches (`-*)`, `""|*[!0-9]*)`,
	// `"$WORKSPACE"/.tool_results/*)`) cannot be harvested as tool names.
	reCaseLabel = regexp.MustCompile(`^\s+([a-z0-9_]+(?:\|[a-z0-9_]+)*)\)(?:\s|$)`)
	// The refusing default arm of a case: `*) return 1 ;;`.
	reDefaultRefuses = regexp.MustCompile(`^\s+\*\)\s*return 1\s*;;`)
	// Function openers we care about, e.g. `is_builtin_tool() {`.
	reFuncOpen = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)
	// Inline exemptions on the execution gate:
	//   [ "$name" != "tool_search" ] && [ "$name" != "tool_result_read" ] && …
	// Matched per-occurrence rather than per-line so a gate carrying several
	// exemptions yields all of them. Anchored on the `$name !=` comparison so an
	// unrelated string test elsewhere in the file cannot be harvested as an
	// exemption.
	// Matches both polarities. The gate was `!=` while it failed open; a
	// fail-closed gate naturally invites the positive form, and a stray
	// exemption written either way is equally outside the registry.
	reInlineExempt = regexp.MustCompile(`"\$name"\s*(?:!=|=)\s*"([a-z0-9_]+)"`)
	// An inline tool definition: the one thing tool_definitions() must not carry.
	reInlineDefinition = regexp.MustCompile(`"name":\s*"|cat <<`)
)

// AddEntrypointSurfaces parses the agent-tool surfaces that live in
// images/vornik-agent/entrypoint.sh AND the generated registry beside it
// (tool_registry.generated.sh, sourced by the entrypoint since 2026-09-03):
//
//   - BUILTIN_TOOL_NAMES_JSON     → KindAgentToolAdvertised
//   - UNGATED_TOOL_*_JSON         → KindAgentToolInlineExempt / KindAgentToolUngatedPrefix
//   - ADVERTISE_TOKENS_JSON       → KindAgentToolAdvertiseToken
//   - TOOL_REGISTRY_JSON          → KindAgentToolRegistry (+ KindAgentToolNeverAdvertised)
//   - exec_tool() cases           → KindAgentToolDispatch   (what can actually run)
//   - tool_advertised_now() cases → KindAgentToolAdvertiseCase
//   - tool_definition_for <name>  → KindAgentToolAppendedByName
//   - structural markers          → KindAgentToolAdvertisementFilter
//
// The registry variables are read from BOTH files, so a fixture may declare
// them inline; on the real entrypoint an inline declaration is a second copy,
// recorded as the `registry-declared-inline` marker for CheckRegistryFileSourced.
//
// These are the unavoidable text parses. They are also the highest-value
// surfaces, because disagreement among them is a privilege bypass rather than a
// cosmetic drift (see CheckAgentToolAgreement). A parse that silently extracted
// nothing would make the check vacuously green, so an empty result for the
// advertised or dispatch set is an error, not an empty set; the newer kinds
// report their own emptiness as findings in their checks.
func (t *Table) AddEntrypointSurfaces(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read entrypoint: %w", err)
	}
	body := string(raw)

	registryPath := filepath.Join(filepath.Dir(path), ToolRegistryFile)
	registry := ""
	if rb, rerr := os.ReadFile(registryPath); rerr == nil {
		registry = string(rb)
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("read tool registry: %w", rerr)
	}
	both := registry + "\n" + body

	if m := reAdvertised.FindStringSubmatch(both); m != nil {
		var names []string
		if err := json.Unmarshal([]byte(m[1]), &names); err != nil {
			return fmt.Errorf("parse BUILTIN_TOOL_NAMES_JSON: %w", err)
		}
		for _, n := range names {
			t.Add(KindAgentToolAdvertised, n, registryPath+":BUILTIN_TOOL_NAMES_JSON")
		}
	}

	// Exemptions. The registry itself is UNGATED_TOOL_NAMES_JSON — a parsed JSON
	// array rather than a scrape of shell comparisons, which is why the gate was
	// rewritten to read it: the exemption set is now one declaration the shell
	// consumes and this parser reads, instead of literal string tests that had
	// to be matched by regex and could drift silently.
	if m := reUngatedNames.FindStringSubmatch(both); m != nil {
		var names []string
		if err := json.Unmarshal([]byte(m[1]), &names); err != nil {
			return fmt.Errorf("parse UNGATED_TOOL_NAMES_JSON: %w", err)
		}
		for _, n := range names {
			t.Add(KindAgentToolInlineExempt, n, registryPath+":UNGATED_TOOL_NAMES_JSON")
		}
	}
	if m := reUngatedPrefixes.FindStringSubmatch(both); m != nil {
		var prefixes []string
		if err := json.Unmarshal([]byte(m[1]), &prefixes); err != nil {
			return fmt.Errorf("parse UNGATED_TOOL_PREFIXES_JSON: %w", err)
		}
		for _, p := range prefixes {
			t.Add(KindAgentToolUngatedPrefix, p, registryPath+":UNGATED_TOOL_PREFIXES_JSON")
		}
	}
	if m := reHelperNames.FindStringSubmatch(both); m != nil {
		var names []string
		if err := json.Unmarshal([]byte(m[1]), &names); err != nil {
			return fmt.Errorf("parse HELPER_TOOL_NAMES_JSON: %w", err)
		}
		t.Add(KindAgentToolAdvertisementFilter, "helper-list-present", registryPath+":HELPER_TOOL_NAMES_JSON")
		for _, n := range names {
			t.Add(KindAgentToolHelperListed, n, registryPath+":HELPER_TOOL_NAMES_JSON")
		}
	}
	if m := reAdvertiseTokens.FindStringSubmatch(both); m != nil {
		var toks []string
		if err := json.Unmarshal([]byte(m[1]), &toks); err != nil {
			return fmt.Errorf("parse ADVERTISE_TOKENS_JSON: %w", err)
		}
		for _, tok := range toks {
			t.Add(KindAgentToolAdvertiseToken, tok, registryPath+":ADVERTISE_TOKENS_JSON")
		}
	}
	if m := reToolRegistry.FindStringSubmatch(both); m != nil {
		var entries []struct {
			Name      string `json:"name"`
			Advertise string `json:"advertise"`
		}
		if err := json.Unmarshal([]byte(m[1]), &entries); err != nil {
			return fmt.Errorf("parse TOOL_REGISTRY_JSON: %w", err)
		}
		for _, e := range entries {
			t.Add(KindAgentToolRegistry, e.Name, registryPath+":TOOL_REGISTRY_JSON")
			if e.Advertise == "never" {
				t.Add(KindAgentToolNeverAdvertised, e.Name, registryPath+":TOOL_REGISTRY_JSON")
			}
		}
	}

	// Structural markers on the entrypoint body.
	if reRegistrySourced.MatchString(body) {
		t.Add(KindAgentToolAdvertisementFilter, "registry-sourced", path+":source")
	}
	if reRegistryDeclared.MatchString(body) {
		t.Add(KindAgentToolAdvertisementFilter, "registry-declared-inline", path)
	}
	if reFailClosedFilter.MatchString(body) {
		t.Add(KindAgentToolAdvertisementFilter, "fail-closed-filter", path+":tool_definitions")
	}
	for _, m := range reDefinitionFor.FindAllStringSubmatchIndex(body, -1) {
		name := body[m[2]:m[3]]
		line := 1 + strings.Count(body[:m[0]], "\n")
		t.Add(KindAgentToolAppendedByName, name, fmt.Sprintf("%s:%d", path, line))
	}

	// STRAY inline exemptions. Nothing should compare "$name" against a literal
	// in the gate any more, but a future edit re-introducing one would be an
	// exemption outside the registry — invisible to the JSON parse above and to
	// review. Harvest any such comparison into the same kind so
	// CheckUngatedExemptionAgreement reports it as unregistered. Unlike the
	// registry parse, finding none here is the expected state.
	for _, m := range reInlineExempt.FindAllStringSubmatchIndex(body, -1) {
		name := body[m[2]:m[3]]
		line := 1 + strings.Count(body[:m[0]], "\n")
		t.Add(KindAgentToolInlineExempt, name, fmt.Sprintf("%s:%d", path, line))
	}

	// Walk line by line tracking which function we are inside, so case labels
	// are attributed to the right registry. Shell has no block structure we can
	// rely on beyond the closing `}` at column 0, which every target function
	// uses.
	var fn string
	var gateReadsRegistry, definitionsInline, definitionsReadRegistry, advertiseDefaultRefuses bool
	var helperMarkers helperBranchState
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
		switch fn {
		case "is_builtin_tool":
			if strings.Contains(text, "BUILTIN_TOOL_NAMES_JSON") {
				gateReadsRegistry = true
			}
		case "tool_definitions":
			if reInlineDefinition.MatchString(text) {
				definitionsInline = true
			}
			if strings.Contains(text, "TOOL_REGISTRY_JSON") {
				definitionsReadRegistry = true
			}
		case "tool_advertised_now":
			if reDefaultRefuses.MatchString(text) {
				advertiseDefaultRefuses = true
			}
			if m := reCaseLabel.FindStringSubmatch(text); m != nil {
				for _, name := range strings.Split(m[1], "|") {
					t.Add(KindAgentToolAdvertiseCase, name, fmt.Sprintf("%s:%d", path, line))
				}
			}
		case "exec_tool":
			if m := reCaseLabel.FindStringSubmatch(text); m != nil {
				for _, name := range strings.Split(m[1], "|") {
					t.Add(KindAgentToolDispatch, name, fmt.Sprintf("%s:%d", path, line))
				}
			}
			t.addHelperBranchMarkers(path, line, text, &helperMarkers)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan entrypoint: %w", err)
	}
	if gateReadsRegistry {
		t.Add(KindAgentToolAdvertisementFilter, "gate-reads-registry", path+":is_builtin_tool")
	}
	if definitionsReadRegistry && !definitionsInline {
		t.Add(KindAgentToolAdvertisementFilter, "definitions-registry-only", path+":tool_definitions")
	}
	if advertiseDefaultRefuses {
		t.Add(KindAgentToolAdvertisementFilter, "advertise-default-refuses", path+":tool_advertised_now")
	}

	for _, kind := range []Kind{KindAgentToolAdvertised, KindAgentToolDispatch} {
		if len(t.Names(kind)) == 0 {
			return fmt.Errorf("extracted zero names for %s from %s — the shell's shape "+
				"changed and this parser is now blind; fix the parser rather than the check", kind, path)
		}
	}
	return nil
}

// helperBranchState tracks, inside exec_tool, the lines CheckHelperBranchIsGated
// orders: the gate's `if ! tool_call_permitted`, its closing `fi`, every
// `tool_runs_in_helper` call, and the `case "$name" in` that opens dispatch.
type helperBranchState struct {
	gateSeen, gateClosed, caseSeen bool
	branches                       int
}

var (
	reHelperGateIf = regexp.MustCompile(`^\s*if ! tool_call_permitted "\$name"; then`)
	reHelperGateFi = regexp.MustCompile(`^    fi\s*$`)
	reHelperBranch = regexp.MustCompile(`\btool_runs_in_helper\b`)
	reHelperCase   = regexp.MustCompile(`^\s*case "\$name" in\s*$`)
)

func (t *Table) addHelperBranchMarkers(path string, line int, text string, st *helperBranchState) {
	src := fmt.Sprintf("%s:%d", path, line)
	at := fmt.Sprint(line)
	switch {
	case !st.gateSeen && reHelperGateIf.MatchString(text):
		st.gateSeen = true
		t.AddWithStatus(KindAgentToolHelperBranch, "gate", src, at)
	case st.gateSeen && !st.gateClosed && reHelperGateFi.MatchString(text):
		st.gateClosed = true
		t.AddWithStatus(KindAgentToolHelperBranch, "gate-end", src, at)
	case !st.caseSeen && reHelperCase.MatchString(text):
		st.caseSeen = true
		t.AddWithStatus(KindAgentToolHelperBranch, "case", src, at)
	}
	if reHelperBranch.MatchString(text) {
		st.branches++
		t.AddWithStatus(KindAgentToolHelperBranch, fmt.Sprintf("helper-branch#%d", st.branches), src, at)
	}
}

// AddAgentLoopHandlers records the tools internal/agentloop implements — the
// helper's dispatch cases, the third view CheckHelperDispatchAgreement holds
// level with the declaration and the bash cases.
func (t *Table) AddAgentLoopHandlers(names []string) {
	for _, n := range names {
		t.Add(KindAgentToolHelperDispatch, n, "internal/agentloop/agentloop.go")
	}
}
