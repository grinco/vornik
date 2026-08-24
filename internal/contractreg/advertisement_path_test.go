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

func TestCheckAdvertisementPathsGated_FiresWhenTheAppendIsUnfiltered(t *testing.T) {
	body := `
tool_definitions() {
    printf '%s' "$base_tools" | jq --argjson ungated "$extras_ungated" \
        '([.[] | select(.function.name as $name | $exempt | index($name) != null)]) + $ungated + ($gated | map(select(.function.name as $name | $allowed | index($name) != null)))'
}
`
	findings := CheckAdvertisementPathsGated(tableFromShell(t, body))

	if len(findings) != 1 {
		t.Fatalf("an unconditional $ungated append must be reported, got %d findings", len(findings))
	}
	if !strings.Contains(findings[0].Detail, "UNGATED_TOOL_NAMES_JSON") {
		t.Errorf("the finding must name the registry to filter against: %s", findings[0].Detail)
	}
}

func TestCheckAdvertisementPathsGated_SilentWhenFiltered(t *testing.T) {
	body := `
tool_definitions() {
    printf '%s' "$base_tools" | jq \
        '([.[] | select(.x)]) + ($ungated | map(select(.function.name as $name | $exempt | index($name) != null))) + $gated'
}
`
	if f := CheckAdvertisementPathsGated(tableFromShell(t, body)); len(f) != 0 {
		t.Fatalf("a filtered append is the fixed state and must be silent, got %+v", f)
	}
}

// An extraction that broke must not read as "the filter is present". The marker
// is recorded only on a positive match, so an empty table is a finding — same
// principle as CheckUngatedExemptionAgreement's empty-extraction guard.
func TestCheckAdvertisementPathsGated_EmptyTableIsAFinding(t *testing.T) {
	if f := CheckAdvertisementPathsGated(New()); len(f) != 1 {
		t.Fatalf("a broken extraction must fail the build, not pass silently; got %+v", f)
	}
}

// The shipped entrypoint must be in the fixed state. This is the assertion that
// would have caught the original defect.
func TestShippedEntrypoint_FiltersTheUngatedAppend(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "images", "vornik-agent", "entrypoint.sh")
	tbl := New()
	if err := tbl.AddEntrypointSurfaces(path); err != nil {
		t.Fatalf("AddEntrypointSurfaces: %v", err)
	}

	if f := CheckAdvertisementPathsGated(tbl); len(f) != 0 {
		t.Fatalf("the shipped entrypoint has an ungated advertisement path: %+v", f)
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

// tableFromShell writes body to a temp entrypoint and extracts it, so these
// tests drive the real parser rather than hand-building a Table.
func tableFromShell(t *testing.T, body string) *Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entrypoint.sh")
	// The extractor refuses a fixture with no gate or dispatch vocabulary — an
	// empty extraction must not read as agreement — so the fixture carries a
	// minimal one and varies only the part under test.
	if err := os.WriteFile(path, []byte(shellFixturePreamble+body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tbl := New()
	if err := tbl.AddEntrypointSurfaces(path); err != nil {
		t.Fatalf("AddEntrypointSurfaces: %v", err)
	}
	return tbl
}

// shellFixturePreamble is the minimum vocabulary AddEntrypointSurfaces insists
// on before it will report anything, so a fixture can vary tool_definitions()
// alone.
const shellFixturePreamble = `
BUILTIN_TOOL_NAMES_JSON='["file_read"]'
UNGATED_TOOL_NAMES_JSON='["tool_search"]'
UNGATED_TOOL_PREFIXES_JSON='["mcp__"]'
is_builtin_tool() {
    case "$1" in
        file_read) return 0 ;;
        *) return 1 ;;
    esac
}
exec_tool() {
    case "$name" in
        file_read) echo ok ;;
    esac
}
`
