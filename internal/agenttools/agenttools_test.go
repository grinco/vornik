package agenttools

import (
	"reflect"
	"sort"
	"testing"
)

func TestIsBuiltin(t *testing.T) {
	for _, name := range []string{"file_read", "run_shell", "memory_search", "git_show"} {
		if !IsBuiltin(name) {
			t.Errorf("IsBuiltin(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "web_fetch", "mcp__scraper__web_fetch", "FILE_READ", "nope"} {
		if IsBuiltin(name) {
			t.Errorf("IsBuiltin(%q) = true, want false", name)
		}
	}
}

func TestIsMCPTool(t *testing.T) {
	if !IsMCPTool("mcp__scraper__web_fetch") {
		t.Error("mcp__scraper__web_fetch should be an MCP tool")
	}
	for _, name := range []string{"mcp__", "file_read", "", "mcp_scraper"} {
		if IsMCPTool(name) {
			t.Errorf("IsMCPTool(%q) = true, want false", name)
		}
	}
}

func TestNamesSortedAndComplete(t *testing.T) {
	names := Names()
	if len(names) != len(builtinTools) {
		t.Fatalf("Names() returned %d entries, want %d", len(names), len(builtinTools))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names() not sorted: %v", names)
	}
	for _, n := range names {
		if !builtinTools[n] {
			t.Errorf("Names() returned unknown tool %q", n)
		}
	}
}

func TestSetIsCopy(t *testing.T) {
	s := Set()
	if !reflect.DeepEqual(s, builtinTools) {
		t.Fatal("Set() should equal the builtin set")
	}
	// Mutating the returned map must not affect the package state.
	s["injected_tool"] = true
	if builtinTools["injected_tool"] {
		t.Error("Set() returned a live reference, not a copy")
	}
}
