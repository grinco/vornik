package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoleLibFile(t *testing.T, dir, name, content string) {
	t.Helper()
	libDir := filepath.Join(dir, "role-library")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const cleanRoleLibEntry = `---
archetypeId: researcher
displayName: "Researcher"
description: "Gathers info."
tools: [file_read, memory_search]
requiredOutputKeys: ["summary"]
runtime: { cpu: "1", memory: "2Gi", maxTokens: 4096 }
modelTier: standard
promptParams: ["topic"]
---
Investigate {{.topic}}.
`

const brokenRoleLibEntry = `---
archetypeId: broken
displayName: "Broken"
description: "bad"
tools: [file_read, not_a_real_tool]
requiredOutputKeys: []
runtime: { cpu: "1", memory: "2Gi", maxTokens: 4096 }
modelTier: standard
promptParams: []
---
Body {{.undeclared}}.
`

const broadRoleLibEntry = `---
archetypeId: coder
displayName: "Coder"
description: "codes"
tools: [file_read, run_shell]
requiredOutputKeys: ["implementation"]
runtime: { cpu: "2", memory: "4Gi", maxTokens: 8192 }
modelTier: complex
promptParams: ["task"]
---
Implement {{.task}}.
`

func TestCheckRoleLibraryNoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	if got := h.checkRoleLibrary(); got.Status != "OK" {
		t.Errorf("no config dir: status = %q, want OK", got.Status)
	}
}

func TestCheckRoleLibraryEmpty(t *testing.T) {
	h := &DoctorHandlers{configDir: t.TempDir()}
	got := h.checkRoleLibrary()
	if got.Status != "OK" {
		t.Errorf("empty library: status = %q, want OK (%s)", got.Status, got.Message)
	}
}

func TestCheckRoleLibraryClean(t *testing.T) {
	dir := t.TempDir()
	writeRoleLibFile(t, dir, "researcher.md", cleanRoleLibEntry)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkRoleLibrary()
	if got.Status != "OK" {
		t.Errorf("clean library: status = %q, want OK (items: %v)", got.Status, got.Items)
	}
}

func TestCheckRoleLibraryBrokenIsError(t *testing.T) {
	dir := t.TempDir()
	writeRoleLibFile(t, dir, "broken.md", brokenRoleLibEntry)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkRoleLibrary()
	if got.Status != "ERROR" {
		t.Fatalf("broken library: status = %q, want ERROR", got.Status)
	}
	if len(got.Items) == 0 {
		t.Error("expected findings in Items")
	}
}

// TestCheckRoleLibraryEnumeratesParseFailures — review-20260717-21f4 #8: the
// doctor must report EVERY malformed file (parse failures alongside structural
// findings) in one pass, not abort on the first. Locks the load-bearing glue
// (LoadWithFindings → prepend parseFindings → CheckLibrary).
func TestCheckRoleLibraryEnumeratesParseFailures(t *testing.T) {
	dir := t.TempDir()
	writeRoleLibFile(t, dir, "researcher.md", cleanRoleLibEntry)
	writeRoleLibFile(t, dir, "bad1.md", "no frontmatter here\n")
	writeRoleLibFile(t, dir, "bad2.md", "also broken\n")
	h := &DoctorHandlers{configDir: dir}
	got := h.checkRoleLibrary()
	if got.Status != "ERROR" {
		t.Fatalf("status = %q, want ERROR (malformed files present)", got.Status)
	}
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "bad1.md") || !strings.Contains(joined, "bad2.md") {
		t.Fatalf("both malformed files must be enumerated; Items = %v", got.Items)
	}
}

func TestCheckRoleLibraryBroadIsWarning(t *testing.T) {
	dir := t.TempDir()
	writeRoleLibFile(t, dir, "coder.md", broadRoleLibEntry)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkRoleLibrary()
	if got.Status != "WARNING" {
		t.Fatalf("broad library: status = %q, want WARNING (items: %v)", got.Status, got.Items)
	}
}

func TestSetSystemHandlerNames(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetSystemHandlerNames([]string{"rag.extract"})
	if len(h.systemHandlerNames) != 1 || h.systemHandlerNames[0] != "rag.extract" {
		t.Errorf("systemHandlerNames = %v", h.systemHandlerNames)
	}
}
