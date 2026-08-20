package contractreg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A PREFIX exemption is the widest grant in this registry: it exempts an
// open-ended set of names rather than one, and nothing about the shape makes
// that visible at the call site. So it gets the same both-directions agreement
// check that UngatedByDesign got, for a strictly stronger reason.
func TestCheckUngatedPrefixAgreement(t *testing.T) {
	populated := func() *Table {
		tbl := New()
		for p := range UngatedPrefixesByDesign {
			tbl.Add(KindAgentToolUngatedPrefix, p, "entrypoint.sh:UNGATED_TOOL_PREFIXES_JSON")
		}
		return tbl
	}

	t.Run("agreement is silent", func(t *testing.T) {
		if f := CheckUngatedPrefixAgreement(populated()); len(f) != 0 {
			t.Errorf("matching prefix lists must produce no findings, got %+v", f)
		}
	})

	t.Run("shell exempts a prefix the registry does not", func(t *testing.T) {
		tbl := populated()
		tbl.Add(KindAgentToolUngatedPrefix, "run_", "entrypoint.sh:UNGATED_TOOL_PREFIXES_JSON")
		findings := CheckUngatedPrefixAgreement(tbl)
		if len(findings) == 0 {
			t.Fatal("a shell-only prefix exempts an unbounded set of names with no recorded reason and must fail")
		}
		if joined := findingText(findings); !strings.Contains(joined, "run_") {
			t.Errorf("the finding must name the offending prefix; got %q", joined)
		}
	})

	t.Run("registry declares a prefix the shell does not exempt", func(t *testing.T) {
		tbl := New()
		tbl.Add(KindAgentToolUngatedPrefix, "something_else__", "entrypoint.sh:UNGATED_TOOL_PREFIXES_JSON")
		findings := CheckUngatedPrefixAgreement(tbl)
		if len(findings) == 0 {
			t.Fatal("a registry-only prefix means the container gates tools the design says it does not — must fail")
		}
		// Both directions must be reported, not just the stray one.
		joined := findingText(findings)
		for p := range UngatedPrefixesByDesign {
			if !strings.Contains(joined, p) {
				t.Errorf("the finding must name the unexempted registry prefix %q; got %q", p, joined)
			}
		}
	})

	// The guard that makes the check non-vacuous. Two empty sets agree, so a
	// broken parse would read as success — the same trap CheckUngatedExemption-
	// Agreement guards against.
	t.Run("empty extraction is itself a failure", func(t *testing.T) {
		findings := CheckUngatedPrefixAgreement(New())
		if len(findings) == 0 {
			t.Fatal("an empty extraction must fail rather than compare two empty sets")
		}
		if joined := findingText(findings); !strings.Contains(joined, "parse") {
			t.Errorf("the finding must say the parse broke; got %q", joined)
		}
	})

	t.Run("every registered prefix carries a reason", func(t *testing.T) {
		for p, reason := range UngatedPrefixesByDesign {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("prefix %q is exempted with no recorded reason — the whole point of the registry", p)
			}
		}
	})
}

// The registries are only worth checking if they are actually extracted from the
// shell. This drives AddEntrypointSurfaces over a fixture in the real shape.
func TestAddEntrypointSurfacesExtractsUngatedRegistries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entrypoint.sh")
	fixture := `#!/bin/bash
BUILTIN_TOOL_NAMES_JSON='["file_read","tool_result_read"]'
UNGATED_TOOL_NAMES_JSON='["tool_search","tool_result_read"]'
UNGATED_TOOL_PREFIXES_JSON='["mcp__"]'
is_builtin_tool() {
    case "$1" in
        file_read) return 0 ;;
        *) return 1 ;;
    esac
}
exec_tool() {
    local name="$1"
    case "$name" in
        file_read)
            echo hi
            ;;
    esac
}
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tbl := New()
	if err := tbl.AddEntrypointSurfaces(path); err != nil {
		t.Fatalf("AddEntrypointSurfaces: %v", err)
	}

	exempt := tbl.Set(KindAgentToolInlineExempt)
	for _, want := range []string{"tool_search", "tool_result_read"} {
		if !exempt[want] {
			t.Errorf("UNGATED_TOOL_NAMES_JSON entry %q was not extracted", want)
		}
	}
	prefixes := tbl.Set(KindAgentToolUngatedPrefix)
	if !prefixes["mcp__"] {
		t.Errorf("UNGATED_TOOL_PREFIXES_JSON entry mcp__ was not extracted, got %v", prefixes)
	}
}

// A stray `"$name" = "x"` comparison in the gate is an exemption outside the
// registry: invisible to the JSON parse, and exactly the drift the fail-closed
// rewrite could reintroduce. It must still be harvested so the agreement check
// reports it as unregistered.
func TestAddEntrypointSurfacesHarvestsStrayInlineExemption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entrypoint.sh")
	fixture := `#!/bin/bash
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
    local name="$1"
    if [ "$name" = "sneaky_tool" ]; then
        return 0
    fi
    case "$name" in
        file_read)
            echo hi
            ;;
    esac
}
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tbl := New()
	if err := tbl.AddEntrypointSurfaces(path); err != nil {
		t.Fatalf("AddEntrypointSurfaces: %v", err)
	}
	if !tbl.Set(KindAgentToolInlineExempt)["sneaky_tool"] {
		t.Fatal("a stray inline exemption must be harvested, or it bypasses the allowlist unreviewed")
	}
	findings := CheckUngatedExemptionAgreement(tbl)
	if len(findings) == 0 {
		t.Fatal("a harvested stray exemption absent from UngatedByDesign must be reported")
	}
	if joined := findingText(findings); !strings.Contains(joined, "sneaky_tool") {
		t.Errorf("the finding must name the stray exemption; got %q", joined)
	}
}

// A malformed registry must be an error, not a silently empty set — an empty
// set is what makes an agreement check vacuously green.
func TestAddEntrypointSurfacesRejectsMalformedUngatedRegistries(t *testing.T) {
	base := `#!/bin/bash
BUILTIN_TOOL_NAMES_JSON='["file_read"]'
%s
is_builtin_tool() {
    case "$1" in
        file_read) return 0 ;;
        *) return 1 ;;
    esac
}
exec_tool() {
    local name="$1"
    case "$name" in
        file_read)
            echo hi
            ;;
    esac
}
`
	cases := map[string]struct{ decls, wantErr string }{
		"names": {
			decls:   "UNGATED_TOOL_NAMES_JSON='[\"tool_search\",]'\nUNGATED_TOOL_PREFIXES_JSON='[\"mcp__\"]'",
			wantErr: "UNGATED_TOOL_NAMES_JSON",
		},
		"prefixes": {
			decls:   "UNGATED_TOOL_NAMES_JSON='[\"tool_search\"]'\nUNGATED_TOOL_PREFIXES_JSON='[not json]'",
			wantErr: "UNGATED_TOOL_PREFIXES_JSON",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "entrypoint.sh")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(base, tc.decls)), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			err := New().AddEntrypointSurfaces(path)
			if err == nil {
				t.Fatal("a malformed registry must error rather than extract an empty set")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error must name the broken registry %q; got %v", tc.wantErr, err)
			}
		})
	}
}
