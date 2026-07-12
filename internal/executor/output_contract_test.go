package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOutputGlobSatisfied is the regression test for incident
// task_20260712143854_429a3500d692d23c (2026-07-12): deep-research
// subtasks completed without writing their promised findings file
// because nothing verified the output contract. The guard must pass
// only when a FRESH file matching the glob exists under the right root.
func TestOutputGlobSatisfied(t *testing.T) {
	staging := t.TempDir()
	worktree := t.TempDir()
	stepStart := time.Now().Add(-time.Minute)

	write := func(root, rel string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !mtime.IsZero() {
			if err := os.Chtimes(p, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("no file fails the contract", func(t *testing.T) {
		if outputGlobSatisfied(staging, worktree, "artifacts/out/deep-research/*.md", stepStart) {
			t.Fatal("empty dir must not satisfy the contract")
		}
	})

	t.Run("fresh staging file satisfies plain glob", func(t *testing.T) {
		write(staging, "artifacts/out/deep-research/01-x.md", time.Time{})
		if !outputGlobSatisfied(staging, worktree, "artifacts/out/deep-research/*.md", stepStart) {
			t.Fatal("fresh matching file must satisfy the contract")
		}
	})

	t.Run("project/ prefix resolves against the worktree", func(t *testing.T) {
		if outputGlobSatisfied(staging, worktree, "project/artifacts/out/deep-research/*.md", stepStart) {
			t.Fatal("worktree is empty — must not match staging files")
		}
		write(worktree, "artifacts/out/deep-research/02-y.md", time.Time{})
		if !outputGlobSatisfied(staging, worktree, "project/artifacts/out/deep-research/*.md", stepStart) {
			t.Fatal("fresh worktree file must satisfy a project/ glob")
		}
	})

	t.Run("stale inherited file does not satisfy", func(t *testing.T) {
		stale := t.TempDir()
		write(stale, "artifacts/out/deliverable.md", stepStart.Add(-time.Hour))
		if outputGlobSatisfied(stale, worktree, "artifacts/out/deliverable.md", stepStart) {
			t.Fatal("a file inherited from an earlier task (mtime < stepStart) must not satisfy THIS step's contract")
		}
	})

	t.Run("malformed globs fail closed", func(t *testing.T) {
		if outputGlobSatisfied(staging, worktree, "/etc/passwd", stepStart) {
			t.Fatal("absolute glob must fail closed")
		}
		if outputGlobSatisfied(staging, worktree, "../outside/*.md", stepStart) {
			t.Fatal("traversal glob must fail closed")
		}
		if outputGlobSatisfied("", "", "project/x.md", stepStart) {
			t.Fatal("empty roots must fail closed")
		}
	})

	t.Run("empty glob is a no-op contract", func(t *testing.T) {
		if !outputGlobSatisfied(staging, worktree, "  ", stepStart) {
			t.Fatal("blank glob must not gate")
		}
	})
}
