package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// TestCleanProjectDir_PreservesAutonomyDir pins the regression
// that surfaced on 2026-05-10: cleanProjectDir runs `git clean
// -fdx --exclude=.worktrees` on the main project workspace
// between task runs, which wiped operator-authored docs living
// in .autonomy/ (USER_GUIDANCE.md, et al) whenever they weren't
// committed to the workspace's git. Result: the deterministic
// USER-task context split silently degraded back to autonomy
// defaults because the env-var-pointed file vanished.
//
// Fix: also exclude .autonomy/ — that's the daemon's per-project
// bookkeeping namespace, and even an untracked operator doc
// there must survive a cleanup triggered by a different task's
// failure path. This test seeds an untracked file in .autonomy/
// and an untracked file at the root, runs cleanProjectDir, and
// asserts the .autonomy/ file survived while the root file was
// wiped.
func TestCleanProjectDir_PreservesAutonomyDir(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	// Seed: one untracked file in .autonomy/ (the operator doc),
	// one untracked file at the project root (cleanup-eligible
	// stray), one untracked file in .worktrees/ (the existing
	// exclusion guard).
	autonomyDir := filepath.Join(projectDir, ".autonomy")
	if err := os.MkdirAll(autonomyDir, 0o755); err != nil {
		t.Fatalf("mkdir .autonomy: %v", err)
	}
	autonomyFile := filepath.Join(autonomyDir, "USER_GUIDANCE.md")
	if err := os.WriteFile(autonomyFile, []byte("operator guidance"), 0o644); err != nil {
		t.Fatalf("write .autonomy file: %v", err)
	}

	worktreesDir := filepath.Join(projectDir, ".worktrees")
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		t.Fatalf("mkdir .worktrees: %v", err)
	}
	worktreeFile := filepath.Join(worktreesDir, "task_keep")
	if err := os.WriteFile(worktreeFile, []byte("sibling task data"), 0o644); err != nil {
		t.Fatalf("write .worktrees file: %v", err)
	}

	strayFile := filepath.Join(projectDir, "stray.tmp")
	if err := os.WriteFile(strayFile, []byte("stray"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	cleanProjectDir(context.Background(), projectDir, zerolog.Nop())

	if _, err := os.Stat(autonomyFile); err != nil {
		t.Errorf(".autonomy/USER_GUIDANCE.md must survive cleanup; got %v", err)
	}
	if _, err := os.Stat(worktreeFile); err != nil {
		t.Errorf(".worktrees content must survive cleanup; got %v", err)
	}
	if _, err := os.Stat(strayFile); !os.IsNotExist(err) {
		t.Errorf("root-level stray must be cleaned; still present: %v", err)
	}
}

// TestCleanProjectDir_NestedAutonomyContentSurvives — operators
// stash hierarchical content in .autonomy/ (per-portal source
// lists, dated scan outputs). The whole subtree must be
// excluded, not just direct children.
func TestCleanProjectDir_NestedAutonomyContentSurvives(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	nested := filepath.Join(projectDir, ".autonomy", "portals", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	nestedFile := filepath.Join(nested, "anti-bot.yaml")
	if err := os.WriteFile(nestedFile, []byte("portal: foo"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	cleanProjectDir(context.Background(), projectDir, zerolog.Nop())

	if _, err := os.Stat(nestedFile); err != nil {
		t.Errorf("nested .autonomy/portals/deep/anti-bot.yaml must survive; got %v", err)
	}
}

// TestCleanProjectDir_NoGitDirIsNoop — defensive: a directory
// without a .git checkout (test harness, bare path) must not
// panic. The existing code returns early via os.Stat; this test
// pins that behavior.
func TestCleanProjectDir_NoGitDirIsNoop(t *testing.T) {
	dir := t.TempDir()
	strayFile := filepath.Join(dir, "should_survive.tmp")
	if err := os.WriteFile(strayFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cleanProjectDir(context.Background(), dir, zerolog.Nop())
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("non-git dir cleanup must be a no-op; file vanished: %v", err)
	}
}

// TestCleanProjectDir_PreservesCustomContextDir — when an operator
// points ProjectAutonomy.ContextFilePath or .UserContextFilePath
// at a path OUTSIDE .autonomy/ (e.g. a "docs/" or
// "operator-guidance/" subdir), the caller-supplied
// extraExcludes must protect that directory too. Pre-fix, only
// .autonomy/ was hardcoded — operators with custom paths still
// lost their docs.
func TestCleanProjectDir_PreservesCustomContextDir(t *testing.T) {
	projectDir := t.TempDir()
	initGitRepo(t, projectDir)

	customDir := filepath.Join(projectDir, "operator-docs")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatalf("mkdir custom dir: %v", err)
	}
	customFile := filepath.Join(customDir, "USER_GUIDANCE.md")
	if err := os.WriteFile(customFile, []byte("custom guidance"), 0o644); err != nil {
		t.Fatalf("write custom file: %v", err)
	}

	cleanProjectDir(context.Background(), projectDir, zerolog.Nop(), "operator-docs")

	if _, err := os.Stat(customFile); err != nil {
		t.Errorf("custom-path operator doc must survive when caller passes the dir as extraExcludes; got %v", err)
	}
}

// TestProjectCleanExcludeDir locks in the helper's safety +
// defaulting contract. Each row pins one decision the cleanup
// path can never silently lose.
func TestProjectCleanExcludeDir(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"default_autonomy_path_skipped", ".autonomy/PROJECT_CONTEXT.md", ""},
		{"nested_autonomy_path_skipped", ".autonomy/portals/anti-bot.yaml", ""},
		{"root_level_returns_empty", "PROJECT_CONTEXT.md", ""},
		{"custom_dir_returned", "operator-docs/USER_GUIDANCE.md", "operator-docs"},
		{"deep_custom_returns_top_dir", "ops/notes/today.md", "ops/notes"},
		{"absolute_path_rejected", "/etc/passwd", ""},
		{"parent_traversal_rejected", "../etc/secrets", ""},
		{"nested_traversal_rejected", "subdir/../../escape.md", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := projectCleanExcludeDir(c.in)
			if got != c.want {
				t.Errorf("projectCleanExcludeDir(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
