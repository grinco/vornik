package contractreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The third advertisement path, 2026-08-22.
//
// tool_definitions() concatenated $extras_ungated unconditionally, so a
// definition appended there reached every role's model regardless of
// allowedTools AND regardless of every registry. It is a different failure from
// the ones CheckAgentToolAgreement and CheckUngatedExemptionAgreement catch:
// those compare vocabularies and find disagreement, and this path consulted no
// vocabulary to disagree with.

func TestCheckToolDefinitionsReadRegistryOnly_FiresOnAnInlineDefinition(t *testing.T) {
	body := `
tool_definitions() {
    local extra
    extra=$(cat <<'X_EOF'
{"type":"function","function":{"name":"sneaky","parameters":{}}}
X_EOF
)
    printf '%s' "$TOOL_REGISTRY_JSON" | jq --argjson e "$extra" '[.[] | .definition] + [$e] | [.[] | select(.function.name as $name | ($exempt | index($name) != null) or (($builtin | index($name) != null) and ($allowed | index($name) != null)))]'
}
`
	findings := CheckToolDefinitionsReadRegistryOnly(tableFromShell(t, body))
	if len(findings) != 1 {
		t.Fatalf("an inline definition inside tool_definitions must be reported once, got %+v", findings)
	}
	if !strings.Contains(findings[0].Detail, "2026-08-22") {
		t.Errorf("the finding must name the bypass class: %s", findings[0].Detail)
	}
}

func TestCheckToolDefinitionsReadRegistryOnly_FiresWhenTheFilterIsGone(t *testing.T) {
	body := `
tool_definitions() {
    printf '%s' "$TOOL_REGISTRY_JSON" | jq '[.[] | .definition]'
}
`
	findings := CheckToolDefinitionsReadRegistryOnly(tableFromShell(t, body))
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, "fail-closed") {
		t.Fatalf("a tool_definitions without the fail-closed filter must be reported, got %+v", findings)
	}
}

func TestCheckToolDefinitionsReadRegistryOnly_SilentInTheFixedShape(t *testing.T) {
	if f := CheckToolDefinitionsReadRegistryOnly(tableFromShell(t, fixedToolDefinitions)); len(f) != 0 {
		t.Fatalf("the fixed shape must be silent, got %+v", f)
	}
}

// An extraction that broke must not read as "the markers are present". They
// are recorded only on a positive match, so an empty table is two findings.
func TestCheckToolDefinitionsReadRegistryOnly_EmptyTableIsAFinding(t *testing.T) {
	if f := CheckToolDefinitionsReadRegistryOnly(New()); len(f) != 2 {
		t.Fatalf("a broken extraction must fail the build, not pass silently; got %+v", f)
	}
}

// The shipped entrypoint must be in the fixed state. This is the assertion that
// would have caught the original defect.
func TestShippedEntrypoint_DefinitionsComeFromTheRegistryOnly(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "images", "vornik-agent", "entrypoint.sh")
	tbl := New()
	if err := tbl.AddEntrypointSurfaces(path); err != nil {
		t.Fatalf("AddEntrypointSurfaces: %v", err)
	}
	if f := CheckToolDefinitionsReadRegistryOnly(tbl); len(f) != 0 {
		t.Fatalf("the shipped entrypoint has an ungated advertisement path: %+v", f)
	}
	if f := CheckRegistryFileSourced(tbl); len(f) != 0 {
		t.Fatalf("the shipped entrypoint does not source the registry cleanly: %+v", f)
	}
	if f := CheckAdvertiseTokensAgree(tbl); len(f) != 0 {
		t.Fatalf("the shipped entrypoint's advertise cases disagree with the tokens: %+v", f)
	}
	if f := CheckNeverAdvertisedIsAppendedByName(tbl); len(f) != 0 {
		t.Fatalf("a never-advertised tool is unreachable in the shipped entrypoint: %+v", f)
	}
	if f := CheckShellRegistryMatchesDeclaration(tbl); len(f) != 0 {
		t.Fatalf("the shipped registry file disagrees with internal/agenttools: %+v", f)
	}
}

func TestCheckRegistryFileSourced(t *testing.T) {
	// A second inline declaration beside the sourced file is the copy the
	// design removed, back as data.
	body := "\nBUILTIN_TOOL_NAMES_JSON='[\"file_read\"]'\n" + fixedToolDefinitions
	f := CheckRegistryFileSourced(tableFromShell(t, body))
	var sawInline bool
	for _, x := range f {
		if strings.Contains(x.Detail, "second copy") {
			sawInline = true
		}
	}
	if !sawInline {
		t.Errorf("an inline registry declaration must be reported, got %+v", f)
	}
	// Without the source line, the gates would run on empty registries.
	tbl := tableFromShellAndRegistry(t, fixedToolDefinitions, defaultRegistryFixture, false)
	f = CheckRegistryFileSourced(tbl)
	if len(f) != 1 || f[0].Name != ToolRegistryFile {
		t.Errorf("a missing source line is one finding naming the registry file, got %+v", f)
	}
}

func TestCheckAdvertiseTokensAgree(t *testing.T) {
	// A token the case does not handle, and a case the tokens do not emit.
	body := `
tool_advertised_now() {
    case "$1" in
        always) return 0 ;;
        never) return 1 ;;
        when_moon_full) return 0 ;;
        *) return 1 ;;
    esac
}
` + fixedDefinitionsWithoutAdvertise
	f := CheckAdvertiseTokensAgree(tableFromShell(t, body))
	names := map[string]bool{}
	for _, x := range f {
		names[x.Name] = true
	}
	if !names["when_moon_full"] || !names["when_memory_url"] {
		t.Errorf("expected findings for the stray case and the unhandled token, got %+v", f)
	}
	// No default arm is its own finding.
	body = `
tool_advertised_now() {
    case "$1" in
        always) return 0 ;;
        never) return 1 ;;
        when_memory_url) return 0 ;;
    esac
}
` + fixedDefinitionsWithoutAdvertise
	f = CheckAdvertiseTokensAgree(tableFromShell(t, body))
	var sawDefault bool
	for _, x := range f {
		if strings.Contains(x.Detail, "default arm") {
			sawDefault = true
		}
	}
	if !sawDefault {
		t.Errorf("a missing refusing default arm must be reported, got %+v", f)
	}
}

func TestCheckNeverAdvertisedIsAppendedByName(t *testing.T) {
	// The default fixture declares tool_search as never and appends it by name.
	if f := CheckNeverAdvertisedIsAppendedByName(tableFromShell(t, fixedToolDefinitions)); len(f) != 0 {
		t.Fatalf("a never tool appended by name is the fixed state, got %+v", f)
	}
	// Drop the append: declared and unreachable.
	tbl := tableFromShellAndRegistry(t, strings.Replace(fixedToolDefinitions, "tool_definition_for tool_search", "true", 1), defaultRegistryFixture, true)
	if f := CheckNeverAdvertisedIsAppendedByName(tbl); len(f) != 1 || f[0].Name != "tool_search" {
		t.Fatalf("a never tool nothing appends must be reported, got %+v", f)
	}
}

func TestCheckShellRegistryMatchesDeclaration_StaleFile(t *testing.T) {
	stale := strings.Replace(defaultRegistryFixture, `"file_read"`, `"file_read","ghost_tool"`, 1)
	tbl := tableFromShellAndRegistry(t, fixedToolDefinitions, stale, true)
	var sawGhost bool
	for _, x := range CheckShellRegistryMatchesDeclaration(tbl) {
		if x.Name == "ghost_tool" {
			sawGhost = true
		}
	}
	if !sawGhost {
		t.Error("a name in the generated file that agenttools does not declare must be reported")
	}
}

// skill_fetch must not be advertised through the ungated path. It is not in
// UngatedByDesign, so riding extras_ungated made it advertised where it was not
// callable — which the daemon's alwaysGranted baseline hid on the normal path.
func TestShippedEntrypoint_SkillFetchIsGated(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "images", "vornik-agent", "entrypoint.sh"))
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	body := string(raw)

	if strings.Contains(body, `"$extras_ungated" | jq --argjson tool "$skill_fetch_tool"`) {
		t.Error("skill_fetch is appended to extras_ungated again — advertisement no longer follows execution")
	}
	if _, registered := UngatedByDesign["skill_fetch"]; registered {
		t.Error("skill_fetch is in UngatedByDesign; it is allowlist-gated and alwaysGranted, not exempt")
	}
}

// tableFromShell writes body to a temp entrypoint beside a default generated
// registry fixture and extracts both, so these tests drive the real parser
// rather than hand-building a Table.
func tableFromShell(t *testing.T, body string) *Table {
	t.Helper()
	return tableFromShellAndRegistry(t, body, defaultRegistryFixture, true)
}

// tableFromShellAndRegistry is tableFromShell with the registry fixture and
// the source line under the caller's control.
func tableFromShellAndRegistry(t *testing.T, body, registry string, sourced bool) *Table {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "entrypoint.sh")
	pre := shellFixturePreamble
	if sourced {
		pre = shellFixtureSource + pre
	}
	if err := os.WriteFile(path, []byte(pre+body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ToolRegistryFile), []byte(registry), 0o600); err != nil {
		t.Fatalf("write registry fixture: %v", err)
	}
	tbl := New()
	if err := tbl.AddEntrypointSurfaces(path); err != nil {
		t.Fatalf("AddEntrypointSurfaces: %v", err)
	}
	return tbl
}

// shellFixtureSource is the line the real entrypoint uses to source the
// generated registry.
const shellFixtureSource = `
source "${VORNIK_TOOL_REGISTRY:-$(dirname "${BASH_SOURCE[0]}")/tool_registry.generated.sh}"
`

// shellFixturePreamble is the minimum vocabulary AddEntrypointSurfaces insists
// on before it will report anything, so a fixture can vary one function alone.
// The registry variables live in defaultRegistryFixture, as they do in the
// shipped tree.
const shellFixturePreamble = `
is_builtin_tool() {
    printf '%s' "$BUILTIN_TOOL_NAMES_JSON" | jq -e --arg n "$1" 'index($n) != null' >/dev/null 2>&1
}
exec_tool() {
    case "$name" in
        file_read) echo ok ;;
        tool_search) echo ok ;;
    esac
}
`

// defaultRegistryFixture is a registry file in the generator's shape, small
// enough to read: two tools, one of them never-advertised.
const defaultRegistryFixture = `#!/usr/bin/env bash
BUILTIN_TOOL_NAMES_JSON='["file_read"]'
UNGATED_TOOL_NAMES_JSON='["tool_search"]'
UNGATED_TOOL_PREFIXES_JSON='["mcp__"]'
ADVERTISE_TOKENS_JSON='["always","never","when_memory_url"]'
read -r -d '' TOOL_REGISTRY_JSON <<'TOOL_REGISTRY_EOF' || true
[{"name":"file_read","advertise":"always","definition":{"type":"function","function":{"name":"file_read","parameters":{}}}},{"name":"tool_search","advertise":"never","definition":{"type":"function","function":{"name":"tool_search","parameters":{}}}}]
TOOL_REGISTRY_EOF
`

// fixedToolDefinitions is tool_definitions() and its companions in the shape
// the 2026-09-03 design ships; fixedDefinitionsWithoutAdvertise is the same
// minus tool_advertised_now, for tests that supply their own.
const fixedToolDefinitions = `
tool_advertised_now() {
    case "$1" in
        always) return 0 ;;
        never) return 1 ;;
        when_memory_url) [ -n "${VORNIK_MEM_URL:-}" ] ;;
        *) return 1 ;;
    esac
}
` + fixedDefinitionsWithoutAdvertise

const fixedDefinitionsWithoutAdvertise = `
tool_definition_for() {
    printf '%s' "$TOOL_REGISTRY_JSON" | jq -e --arg n "$1" '.[] | select(.name == $n) | .definition'
}
tool_definitions() {
    printf '%s' "$TOOL_REGISTRY_JSON" | jq --argjson live "$(live_advertise_tokens_json)" --argjson allowed "$(allowed_builtin_tools_json)" --argjson builtin "$BUILTIN_TOOL_NAMES_JSON" --argjson exempt "$UNGATED_TOOL_NAMES_JSON" '[.[] | select(.advertise as $a | $live | index($a) != null) | .definition] | [.[] | select(.function.name as $name | ($exempt | index($name) != null) or (($builtin | index($name) != null) and ($allowed | index($name) != null)))]'
}
rebuild_tools_file() {
    tool_definition_for tool_search > "$1"
}
`
