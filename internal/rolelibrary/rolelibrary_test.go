package rolelibrary

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validArchetype = `---
archetypeId: researcher
displayName: "Researcher"
description: "Gathers info."
tools:
  - file_read
  - memory_search
requiredOutputKeys: ["summary"]
runtime: { cpu: "1", memory: "2Gi", maxTokens: 4096 }
modelTier: standard
promptParams: ["topic"]
---
Researcher. Investigate {{.topic}} and write a summary.
`

func TestParseArchetypeRoundTrip(t *testing.T) {
	a, err := ParseArchetype([]byte(validArchetype), "researcher.md")
	if err != nil {
		t.Fatalf("ParseArchetype: %v", err)
	}
	if a.ArchetypeID != "researcher" {
		t.Errorf("ArchetypeID = %q, want researcher", a.ArchetypeID)
	}
	if a.DisplayName != "Researcher" {
		t.Errorf("DisplayName = %q", a.DisplayName)
	}
	if !reflect.DeepEqual(a.Tools, []string{"file_read", "memory_search"}) {
		t.Errorf("Tools = %v", a.Tools)
	}
	if !reflect.DeepEqual(a.RequiredOutputKeys, []string{"summary"}) {
		t.Errorf("RequiredOutputKeys = %v", a.RequiredOutputKeys)
	}
	if a.Runtime.CPU != "1" || a.Runtime.Memory != "2Gi" || a.Runtime.MaxTokens != 4096 {
		t.Errorf("Runtime = %+v", a.Runtime)
	}
	if a.ModelTier != ModelTierStandard {
		t.Errorf("ModelTier = %q", a.ModelTier)
	}
	if !reflect.DeepEqual(a.PromptParams, []string{"topic"}) {
		t.Errorf("PromptParams = %v", a.PromptParams)
	}
	if a.Prompt != "Researcher. Investigate {{.topic}} and write a summary." {
		t.Errorf("Prompt = %q", a.Prompt)
	}
	if a.SourceFile != "researcher.md" {
		t.Errorf("SourceFile = %q", a.SourceFile)
	}
	// The valid archetype passes the doctor check clean.
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); len(fs) != 0 {
		t.Errorf("valid archetype produced findings: %v", fs)
	}
}

func TestParseArchetypeErrors(t *testing.T) {
	cases := map[string]string{
		"no opening marker": "archetypeId: x\n",
		"marker not alone":  "--- x\nbody\n",
		"no closing marker": "---\narchetypeId: x\nbody with no close\n",
		"bad yaml":          "---\narchetypeId: [unterminated\n---\nbody\n",
	}
	for name, content := range cases {
		if _, err := ParseArchetype([]byte(content), "t.md"); err == nil {
			t.Errorf("%s: expected parse error, got nil", name)
		}
	}
}

func TestParseArchetypeTolerantOfBOMAndLeadingWS(t *testing.T) {
	content := append([]byte{}, utf8BOM...)
	content = append(content, []byte("\n  "+validArchetype)...)
	a, err := ParseArchetype(content, "r.md")
	if err != nil {
		t.Fatalf("ParseArchetype with BOM/ws: %v", err)
	}
	if a.ArchetypeID != "researcher" {
		t.Errorf("ArchetypeID = %q", a.ArchetypeID)
	}
}

func TestLoadFromDir(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, LibraryDirName)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(libDir, "researcher.md"), validArchetype)
	other := validArchetype[:len("---\narchetypeId: ")] + "aardvark\n" + validArchetype[len("---\narchetypeId: researcher\n"):]
	writeFile(t, filepath.Join(libDir, "aardvark.md"), other)
	// A non-md file is ignored.
	writeFile(t, filepath.Join(libDir, "README.txt"), "ignore me")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Load returned %d archetypes, want 2", len(got))
	}
	// Sorted by ArchetypeID.
	if got[0].ArchetypeID != "aardvark" || got[1].ArchetypeID != "researcher" {
		t.Errorf("Load order = %q, %q; want aardvark, researcher", got[0].ArchetypeID, got[1].ArchetypeID)
	}
}

// TestLoadWithFindings — the doctor path must ENUMERATE every malformed file as
// a finding (not abort on the first), so the operator sees the full picture
// (review-20260716-7e65 #1). Valid archetypes still load.
func TestLoadWithFindings(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, LibraryDirName)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(libDir, "researcher.md"), validArchetype)
	writeFile(t, filepath.Join(libDir, "bad1.md"), "no frontmatter here\n")
	writeFile(t, filepath.Join(libDir, "bad2.md"), "also broken\n")

	got, findings, err := LoadWithFindings(dir)
	if err != nil {
		t.Fatalf("LoadWithFindings must not return a hard error for per-file parse failures: %v", err)
	}
	if len(got) != 1 || got[0].ArchetypeID != "researcher" {
		t.Fatalf("expected the 1 valid archetype, got %+v", got)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 parse-error findings (bad1.md, bad2.md), got %d: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Severity != SeverityError {
			t.Errorf("parse-error finding must be SeverityError, got %q", f.Severity)
		}
	}
}

func TestLoadParseErrorAborts(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, LibraryDirName)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two malformed files: Load aborts on the FIRST by sorted filename
	// (os.ReadDir order), so the surfaced error must name aaa-bad.md, not
	// zzz-bad.md (review-20260717-21f4 #3 — lock the ordering so a future
	// iteration-order change can't silently swap which error surfaces).
	writeFile(t, filepath.Join(libDir, "zzz-bad.md"), "no frontmatter here\n")
	writeFile(t, filepath.Join(libDir, "aaa-bad.md"), "also broken\n")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load should abort on a malformed file")
	}
	if !strings.Contains(err.Error(), "aaa-bad.md") {
		t.Errorf("Load must surface the first-sorted bad file (aaa-bad.md), got: %v", err)
	}
}

// TestLoadIgnoresReadme is a regression test: configs/role-library/
// ships a README.md documenting the security model (no frontmatter),
// and Load globs every *.md file in the directory. Without an explicit
// skip, Load would try to parse the README as an archetype and abort
// with a "missing opening frontmatter marker" error.
func TestLoadIgnoresReadme(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, LibraryDirName)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(libDir, "researcher.md"), validArchetype)
	writeFile(t, filepath.Join(libDir, "README.md"), "# Role library\n\nNot an archetype.\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with README.md present: %v", err)
	}
	if len(got) != 1 || got[0].ArchetypeID != "researcher" {
		t.Errorf("Load = %v, want exactly [researcher] (README.md must be skipped)", got)
	}
}

func TestLoadMissingDir(t *testing.T) {
	got, err := Load(t.TempDir()) // no role-library subdir
	if err != nil {
		t.Fatalf("Load missing dir: %v", err)
	}
	if got != nil {
		t.Errorf("Load missing dir = %v, want nil", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
