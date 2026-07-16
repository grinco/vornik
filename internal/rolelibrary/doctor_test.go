package rolelibrary

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goodArchetype returns a fresh, fully-valid archetype the tests
// mutate one field at a time to exercise each doctor rule in isolation.
func goodArchetype() *RoleArchetype {
	return &RoleArchetype{
		ArchetypeID:        "widget",
		DisplayName:        "Widget",
		Description:        "does a thing",
		Tools:              []string{"file_read", "memory_search"},
		RequiredOutputKeys: []string{"summary"},
		Runtime:            ArchetypeRuntime{CPU: "1", Memory: "2Gi", MaxTokens: 4096},
		ModelTier:          ModelTierStandard,
		PromptParams:       []string{"topic"},
		Prompt:             "Investigate {{.topic}}.",
	}
}

func hasError(fs []Finding, substr string) bool {
	for _, f := range fs {
		if f.Severity == SeverityError && strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

func hasFlag(fs []Finding, substr string) bool {
	for _, f := range fs {
		if f.Severity == SeverityFlag && strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

func TestCheckLibraryCleanArchetype(t *testing.T) {
	if fs := CheckLibrary([]*RoleArchetype{goodArchetype()}, nil); len(fs) != 0 {
		t.Fatalf("clean archetype produced findings: %v", fs)
	}
}

func TestCheckEmptyPromptBody(t *testing.T) {
	// review-20260716-7e65: an empty/whitespace body composes a role with NO
	// system prompt, yet passed the doctor clean.
	a := goodArchetype()
	a.Prompt = "   \n\t "
	a.PromptParams = nil // no splice points, else undeclared-field noise
	fs := CheckLibrary([]*RoleArchetype{a}, nil)
	if !hasError(fs, "prompt") {
		t.Errorf("expected empty-prompt error, got %v", fs)
	}
}

func TestCheckDuplicateArchetypeID(t *testing.T) {
	// review-20260716-7e65: two files with the same archetypeId both loaded and
	// passed clean, leaving the composer's select-by-ID ambiguous.
	a := goodArchetype()
	a.SourceFile = "widget-a.md"
	b := goodArchetype()
	b.SourceFile = "widget-b.md"
	fs := CheckLibrary([]*RoleArchetype{a, b}, nil)
	if !hasError(fs, "duplicate") {
		t.Errorf("expected duplicate-archetypeId error, got %v", fs)
	}
}

func TestCheckUnknownTool(t *testing.T) {
	a := goodArchetype()
	a.Tools = []string{"file_read", "totally_made_up_tool"}
	fs := CheckLibrary([]*RoleArchetype{a}, nil)
	if !hasError(fs, "totally_made_up_tool") {
		t.Errorf("expected unknown-tool error, got %v", fs)
	}
}

func TestCheckSystemHandlerToolAccepted(t *testing.T) {
	a := goodArchetype()
	a.Tools = []string{"file_read", "rag.extract"}
	// Without the handler registered → error.
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "rag.extract") {
		t.Errorf("expected error for unregistered handler, got %v", fs)
	}
	// With the handler registered → accepted.
	if fs := CheckLibrary([]*RoleArchetype{a}, []string{"rag.extract"}); len(fs) != 0 {
		t.Errorf("registered handler should be accepted, got %v", fs)
	}
}

func TestCheckMCPToolAccepted(t *testing.T) {
	a := goodArchetype()
	a.Tools = []string{"file_read", "mcp__scraper__web_fetch"}
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); len(fs) != 0 {
		t.Errorf("mcp__ tool should be accepted, got %v", fs)
	}
}

func TestCheckEmptyRequiredOutputKeys(t *testing.T) {
	a := goodArchetype()
	a.RequiredOutputKeys = nil
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "requiredOutputKeys is empty") {
		t.Errorf("expected empty-requiredOutputKeys error, got %v", fs)
	}
}

func TestCheckEmptyRequiredOutputKeyString(t *testing.T) {
	a := goodArchetype()
	a.RequiredOutputKeys = []string{"summary", "  "}
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "empty string") {
		t.Errorf("expected empty-string key error, got %v", fs)
	}
}

func TestCheckUndeclaredSplicePoint(t *testing.T) {
	a := goodArchetype()
	a.Prompt = "Investigate {{.topic}} for {{.audience}}."
	fs := CheckLibrary([]*RoleArchetype{a}, nil)
	if !hasError(fs, "audience") {
		t.Errorf("expected undeclared-splice error for audience, got %v", fs)
	}
}

func TestCheckDeclaredSplicePointOK(t *testing.T) {
	a := goodArchetype()
	a.PromptParams = []string{"topic", "audience"}
	a.Prompt = "Investigate {{.topic}} for {{.audience}}. {{if .topic}}x{{end}}"
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); len(fs) != 0 {
		t.Errorf("declared splice points should pass, got %v", fs)
	}
}

func TestCheckMalformedTemplate(t *testing.T) {
	a := goodArchetype()
	a.Prompt = "Investigate {{.topic" // unclosed action
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "does not parse") {
		t.Errorf("expected template parse error, got %v", fs)
	}
}

func TestCheckBadRuntime(t *testing.T) {
	a := goodArchetype()
	a.Runtime.MaxTokens = 0
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "maxTokens") {
		t.Errorf("expected maxTokens error, got %v", fs)
	}
	b := goodArchetype()
	b.Runtime.CPU = ""
	if fs := CheckLibrary([]*RoleArchetype{b}, nil); !hasError(fs, "runtime.cpu") {
		t.Errorf("expected cpu error, got %v", fs)
	}
	c := goodArchetype()
	c.Runtime.Memory = ""
	if fs := CheckLibrary([]*RoleArchetype{c}, nil); !hasError(fs, "runtime.memory") {
		t.Errorf("expected memory error, got %v", fs)
	}
}

func TestCheckBadModelTier(t *testing.T) {
	a := goodArchetype()
	a.ModelTier = "wizardly"
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "modelTier") {
		t.Errorf("expected modelTier error, got %v", fs)
	}
}

func TestCheckEmptyArchetypeID(t *testing.T) {
	a := goodArchetype()
	a.ArchetypeID = "  "
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); !hasError(fs, "archetypeId is empty") {
		t.Errorf("expected archetypeId error, got %v", fs)
	}
}

func TestCheckBroadToolsFlagged(t *testing.T) {
	// run_shell → flagged.
	a := goodArchetype()
	a.Tools = []string{"file_read", "run_shell"}
	fs := CheckLibrary([]*RoleArchetype{a}, nil)
	if !hasFlag(fs, "run_shell") {
		t.Errorf("expected run_shell broad flag, got %v", fs)
	}
	// A flag alone is not a failure (no error findings).
	if hasError(fs, "") {
		t.Errorf("broad-tools flag should not produce an error, got %v", fs)
	}

	// Wildcard → flagged.
	b := goodArchetype()
	b.Tools = []string{"file_read", "mcp__scraper__*"}
	if fs := CheckLibrary([]*RoleArchetype{b}, nil); !hasFlag(fs, "wildcard") {
		t.Errorf("expected wildcard flag, got %v", fs)
	}

	// Large list → flagged.
	c := goodArchetype()
	c.Tools = []string{"file_read", "file_write", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_diff", "git_log"}
	if fs := CheckLibrary([]*RoleArchetype{c}, nil); !hasFlag(fs, "large") {
		t.Errorf("expected large-list flag, got %v", fs)
	}
}

func TestCheckLibrarySkipsNil(t *testing.T) {
	if fs := CheckLibrary([]*RoleArchetype{nil, goodArchetype()}, nil); len(fs) != 0 {
		t.Errorf("nil archetype should be skipped, got %v", fs)
	}
}

func TestCheckLibrarySortsErrorsBeforeFlags(t *testing.T) {
	broken := goodArchetype()
	broken.ArchetypeID = "aaa"
	broken.Tools = []string{"bogus"}
	broad := goodArchetype()
	broad.ArchetypeID = "zzz"
	broad.Tools = []string{"run_shell"}
	fs := CheckLibrary([]*RoleArchetype{broad, broken}, nil)
	if len(fs) < 2 {
		t.Fatalf("expected ≥2 findings, got %v", fs)
	}
	if fs[0].Severity != SeverityError {
		t.Errorf("errors should sort first, got %v", fs)
	}
}

func TestCheckTemplateControlFlowSplicePoints(t *testing.T) {
	a := goodArchetype()
	a.PromptParams = []string{"topic", "items", "opt"}
	// Exercises range / with / if-else walk branches; all declared → clean.
	a.Prompt = "{{range .items}}x{{end}} {{with .opt}}{{.topic}}{{else}}none{{end}}"
	if fs := CheckLibrary([]*RoleArchetype{a}, nil); len(fs) != 0 {
		t.Errorf("declared control-flow splice points should pass, got %v", fs)
	}

	// An undeclared field inside a range must still be caught.
	b := goodArchetype()
	b.PromptParams = []string{"topic"}
	b.Prompt = "{{range .items}}{{.topic}}{{end}}"
	if fs := CheckLibrary([]*RoleArchetype{b}, nil); !hasError(fs, "items") {
		t.Errorf("undeclared field in range should be caught, got %v", fs)
	}
}

func TestFindingString(t *testing.T) {
	f := Finding{ArchetypeID: "x", Severity: SeverityError, Message: "boom"}
	if got := f.String(); got != "[error] x: boom" {
		t.Errorf("Finding.String() = %q", got)
	}
	f2 := Finding{Severity: SeverityFlag, Message: "wide"}
	if got := f2.String(); !strings.Contains(got, "<unknown>") {
		t.Errorf("Finding.String() = %q, want <unknown>", got)
	}
}

// TestSeededArchetypesPass loads the shipped configs/role-library and
// asserts every archetype passes the doctor check completely clean —
// no ERRORs (the feature-doctor prereq "≥1 entry passing the check"
// must be satisfiable) AND no FLAGs/WARNINGs. The library is the outer
// capability boundary for every composed automation, so it must stay
// least-privilege by construction: run_shell and other broad grants
// are excluded from the seed, not merely tolerated (security review
// 2026-07-10, review-20260710-96fe.md). Broad-allowlist detection
// coverage is NOT lost by tightening this — see
// TestCheckBroadToolsFlagged, which proves the FLAG mechanism itself
// using a synthetic in-test archetype that DOES grant run_shell.
func TestSeededArchetypesPass(t *testing.T) {
	configs := repoConfigsDir(t)
	got, err := Load(configs)
	if err != nil {
		t.Fatalf("Load shipped library: %v", err)
	}
	if len(got) < 6 {
		t.Fatalf("expected ≥6 seeded archetypes, got %d", len(got))
	}
	fs := CheckLibrary(got, seededSystemHandlers())
	for _, f := range fs {
		t.Errorf("seeded archetype finding (want none): %s", f)
	}
}

// seededSystemHandlers is the set of system-step handler names the
// seeded archetypes may reference. The current seeds use only
// built-in + mcp tools, so this is empty; kept as a seam for future
// system-handler-using archetypes.
func seededSystemHandlers() []string { return nil }

func repoConfigsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/rolelibrary/doctor_test.go → repo root → configs
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "configs")
}
