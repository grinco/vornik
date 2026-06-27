package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCanonicalContext_DotAutonomyPath covers the happy
// path: project uses the documented .autonomy/ convention.
// Both files load; Source flags dot_autonomy.
func TestResolveCanonicalContext_DotAutonomyPath(t *testing.T) {
	dir := t.TempDir()
	mustWriteCtxFile(t, dir, ".autonomy/PROJECT_CONTEXT.md", "# Mission\n\nDeliver Q3 launch.")
	mustWriteCtxFile(t, dir, ".autonomy/USER_GUIDANCE.md", "Never publish without review.")

	ctx := resolveCanonicalContext(dir)
	if !strings.Contains(ctx.ProjectContext, "Deliver Q3 launch") {
		t.Errorf("ProjectContext missing body: %q", ctx.ProjectContext)
	}
	if !strings.Contains(ctx.UserGuidance, "Never publish") {
		t.Errorf("UserGuidance missing body: %q", ctx.UserGuidance)
	}
	if ctx.Source != "dot_autonomy" {
		t.Errorf("Source = %q, want dot_autonomy", ctx.Source)
	}
	if len(ctx.Truncated) != 0 {
		t.Errorf("Truncated should be empty: %v", ctx.Truncated)
	}
	if ctx.Empty() {
		t.Errorf("Empty() should be false")
	}
}

// TestResolveCanonicalContext_LegacyPlainAutonomy: legacy
// workspaces with autonomy/ (no leading dot) still resolve.
func TestResolveCanonicalContext_LegacyPlainAutonomy(t *testing.T) {
	dir := t.TempDir()
	mustWriteCtxFile(t, dir, "autonomy/PROJECT_CONTEXT.md", "legacy content")
	ctx := resolveCanonicalContext(dir)
	if ctx.ProjectContext != "legacy content" {
		t.Errorf("ProjectContext = %q", ctx.ProjectContext)
	}
	if ctx.Source != "plain_autonomy" {
		t.Errorf("Source = %q, want plain_autonomy", ctx.Source)
	}
}

// TestResolveCanonicalContext_LowercaseFilename pin: the
// snake_case casing the original convention drift used still
// works as a fallback.
func TestResolveCanonicalContext_LowercaseFilename(t *testing.T) {
	dir := t.TempDir()
	mustWriteCtxFile(t, dir, ".autonomy/project_context.md", "lowercase wins")
	ctx := resolveCanonicalContext(dir)
	if ctx.ProjectContext != "lowercase wins" {
		t.Errorf("ProjectContext = %q", ctx.ProjectContext)
	}
}

// TestResolveCanonicalContext_PrecedenceUppercaseWins: when
// both casings exist in the same directory, the uppercase
// (canonical) version wins.
func TestResolveCanonicalContext_PrecedenceUppercaseWins(t *testing.T) {
	dir := t.TempDir()
	mustWriteCtxFile(t, dir, ".autonomy/PROJECT_CONTEXT.md", "uppercase")
	mustWriteCtxFile(t, dir, ".autonomy/project_context.md", "lowercase")
	ctx := resolveCanonicalContext(dir)
	if ctx.ProjectContext != "uppercase" {
		t.Errorf("uppercase canonical should win; got %q", ctx.ProjectContext)
	}
}

// TestResolveCanonicalContext_MixedSources: both .autonomy/
// and autonomy/ present → "mixed" tag fires so operators can
// run the (future) canonicaliser.
func TestResolveCanonicalContext_MixedSources(t *testing.T) {
	dir := t.TempDir()
	mustWriteCtxFile(t, dir, ".autonomy/PROJECT_CONTEXT.md", "dot")
	mustWriteCtxFile(t, dir, "autonomy/USER_GUIDANCE.md", "plain")
	ctx := resolveCanonicalContext(dir)
	if ctx.ProjectContext != "dot" {
		t.Errorf("ProjectContext should resolve from .autonomy/: %q", ctx.ProjectContext)
	}
	if ctx.UserGuidance != "plain" {
		t.Errorf("UserGuidance should resolve from autonomy/: %q", ctx.UserGuidance)
	}
	if ctx.Source != "mixed" {
		t.Errorf("Source = %q, want mixed", ctx.Source)
	}
}

// TestResolveCanonicalContext_Absent: no convention files →
// zero result. Empty() returns true; agent falls back to
// pre-feature behaviour.
func TestResolveCanonicalContext_Absent(t *testing.T) {
	dir := t.TempDir()
	ctx := resolveCanonicalContext(dir)
	if !ctx.Empty() {
		t.Errorf("Empty() should be true; got %+v", ctx)
	}
	if ctx.Source != "" {
		t.Errorf("Source = %q, want empty", ctx.Source)
	}
}

// TestResolveCanonicalContext_EmptyWorkspace: empty path → zero
// result without crashing.
func TestResolveCanonicalContext_EmptyWorkspace(t *testing.T) {
	ctx := resolveCanonicalContext("")
	if !ctx.Empty() {
		t.Errorf("empty workspace should return empty CanonicalContext")
	}
}

// TestResolveCanonicalContext_Truncation: a >16KiB file is
// truncated with a marker.
func TestResolveCanonicalContext_Truncation(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("x", canonicalContextMaxBytes+1024)
	mustWriteCtxFile(t, dir, ".autonomy/PROJECT_CONTEXT.md", huge)

	ctx := resolveCanonicalContext(dir)
	if len(ctx.ProjectContext) <= canonicalContextMaxBytes {
		t.Errorf("ProjectContext should keep maxBytes of content (got %d bytes)", len(ctx.ProjectContext))
	}
	if !strings.Contains(ctx.ProjectContext, "[truncated") {
		t.Errorf("truncation marker missing: %q", ctx.ProjectContext[len(ctx.ProjectContext)-100:])
	}
	wantTrunc := []string{"project"}
	if len(ctx.Truncated) != 1 || ctx.Truncated[0] != wantTrunc[0] {
		t.Errorf("Truncated = %v, want %v", ctx.Truncated, wantTrunc)
	}
}

// TestResolveCanonicalContext_SymlinkRejected: symlinks under
// .autonomy/ are treated as absent. Defends against an
// operator-pointed symlink to /etc/passwd making it into
// task.json.
func TestResolveCanonicalContext_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("secret content"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".autonomy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported on this filesystem: %v", err)
	}
	ctx := resolveCanonicalContext(dir)
	if ctx.ProjectContext != "" {
		t.Errorf("symlink target should not leak into ProjectContext; got %q", ctx.ProjectContext)
	}
}

// TestComposeSystemPromptWithCanonicalContext: pins the prompt
// composition rules.
func TestComposeSystemPromptWithCanonicalContext(t *testing.T) {
	emptyCtx := CanonicalContext{}
	loadedCtx := CanonicalContext{ProjectContext: "x", Source: "dot_autonomy"}

	cases := []struct {
		name       string
		role       string
		ctx        CanonicalContext
		wantBlock  bool
		wantPrefix string
	}{
		{"empty role + empty ctx", "", emptyCtx, false, ""},
		{"empty role + loaded ctx", "", loadedCtx, true, "\nYou have canonical project context"},
		{"role + empty ctx", "Be the lead.", emptyCtx, false, "Be the lead."},
		{"role + loaded ctx", "Be the lead.", loadedCtx, true, "Be the lead."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeSystemPromptWithCanonicalContext(tc.role, tc.ctx)
			if tc.wantBlock {
				if !strings.Contains(got, "context.projectContext") {
					t.Errorf("expected canonical-context block in prompt; got %q", got)
				}
			} else {
				if strings.Contains(got, "context.projectContext") {
					t.Errorf("block should NOT be present; got %q", got)
				}
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("prefix mismatch: got %q want prefix %q", got, tc.wantPrefix)
			}
		})
	}
}

// mustWriteCtxFile writes a fixture under root, creating parent
// dirs as needed.
func mustWriteCtxFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
