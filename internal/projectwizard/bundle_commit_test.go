package projectwizard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrderedRelPaths_ProjectFileLast(t *testing.T) {
	files := map[string]string{
		"projects/x.yaml":   "proj",
		"workflows/b.md":    "wf-b",
		"swarms/x-swarm.md": "swarm",
		"workflows/a.md":    "wf-a",
	}
	order := orderedRelPaths(files)
	if len(order) != 4 {
		t.Fatalf("expected 4 entries, got %d: %v", len(order), order)
	}
	if order[len(order)-1] != "projects/x.yaml" {
		t.Fatalf("project file must be last, got order %v", order)
	}
	// Workflows before the swarm, swarm before the project — the
	// dependency chain the design mandates.
	swarmIdx := relPathIndex(order, "swarms/x-swarm.md")
	projIdx := relPathIndex(order, "projects/x.yaml")
	for _, wf := range []string{"workflows/a.md", "workflows/b.md"} {
		if relPathIndex(order, wf) > swarmIdx {
			t.Errorf("workflow %q must land before the swarm; order=%v", wf, order)
		}
	}
	if swarmIdx > projIdx {
		t.Errorf("swarm must land before the project; order=%v", order)
	}
}

func relPathIndex(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// bundleFilesForCommitTest materializes+renders validComposedBundle()
// into the same file-set shape stageAndCommitBundle consumes —
// project/swarm/one workflow.
func bundleFilesForCommitTest(t *testing.T) map[string]string {
	t.Helper()
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("renderMaterializedBundle: %v", err)
	}
	return files
}

func TestStageAndCommitBundle_HappyPath(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)

	if err := stageAndCommitBundle(liveDir, "sess-1", files); err != nil {
		t.Fatalf("stageAndCommitBundle: %v", err)
	}

	for rel, body := range files {
		got, err := os.ReadFile(filepath.Join(liveDir, rel))
		if err != nil {
			t.Fatalf("expected %s to land in the live tree: %v", rel, err)
		}
		if string(got) != body {
			t.Errorf("%s landed with wrong content", rel)
		}
	}

	// Staging dir + journal must be gone after a successful commit.
	stageDir := stagingDirFor(liveDir, "sess-1")
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Errorf("expected staging dir removed after success, stat err = %v", err)
	}
}

func TestStageAndCommitBundle_NoLiveConfigDir(t *testing.T) {
	if err := stageAndCommitBundle("", "sess-1", map[string]string{"projects/x.yaml": "y"}); err == nil {
		t.Fatal("expected an error with no live config dir wired")
	}
}

func TestStageAndCommitBundle_NoFiles(t *testing.T) {
	if err := stageAndCommitBundle(t.TempDir(), "sess-1", nil); err == nil {
		t.Fatal("expected an error with an empty file set")
	}
}

func TestStageAndCommitBundle_RejectsUnsafeSessionID(t *testing.T) {
	if err := stageAndCommitBundle(t.TempDir(), "../escape", map[string]string{"projects/x.yaml": "x"}); err == nil {
		t.Fatal("expected unsafe session id to be rejected")
	}
}

func TestStageAndCommitBundle_RejectsUnsafeRelPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "escape.yaml")
	err := stageAndCommitBundle(root, "sess-safe", map[string]string{"projects/../../escape.yaml": "x"})
	if err == nil {
		t.Fatal("expected unsafe rel path to be rejected")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe rel path must not write outside live config dir, stat=%v", statErr)
	}
}

// TestStageAndCommitBundle_DependencyOrder_ProjectLast is the design's
// own assertion (§5.6 step 3 / §8 "journaled commit: dependency
// order"): by the time the rename sequence reaches the project file,
// every workflow and the swarm must already be live, and the project
// file itself must not exist yet. The injected renameFn performs the
// real rename for every target EXCEPT the project file, where it
// first asserts that intermediate state and then fails (simulating a
// crash/error right before the activating rename).
func TestStageAndCommitBundle_DependencyOrder_ProjectLast(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)

	origRename := renameFn
	defer func() { renameFn = origRename }()
	renameFn = func(oldpath, newpath string) error {
		if strings.Contains(newpath, string(filepath.Separator)+"projects"+string(filepath.Separator)) {
			// Assert the mid-sequence state: workflow(s) and the swarm
			// already landed, the project file has not.
			if _, err := os.Stat(filepath.Join(liveDir, "workflows", "research-digest.md")); err != nil {
				t.Errorf("expected the workflow to have landed before the project file, stat err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(liveDir, "swarms", "ai-news-digest-swarm.md")); err != nil {
				t.Errorf("expected the swarm to have landed before the project file, stat err = %v", err)
			}
			if _, err := os.Stat(newpath); !os.IsNotExist(err) {
				t.Errorf("project file must not exist yet at the point its rename is attempted, stat err = %v", err)
			}
			return errInjectedRenameFailure
		}
		return origRename(oldpath, newpath)
	}

	err := stageAndCommitBundle(liveDir, "sess-2", files)
	if err == nil {
		t.Fatal("expected the injected project-file rename failure to propagate")
	}
}

var errInjectedRenameFailure = errInjected("injected rename failure")

type errInjected string

func (e errInjected) Error() string { return string(e) }

// TestStageAndCommitBundle_RollbackRemovesLandedFiles: the same
// injected failure as above, but this test asserts the END state
// (post-rollback) rather than the mid-sequence snapshot — every
// previously-landed file must be gone and the staging dir cleaned up,
// i.e. the live tree is back to exactly its pre-commit state.
func TestStageAndCommitBundle_RollbackRemovesLandedFiles(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)

	origRename := renameFn
	defer func() { renameFn = origRename }()
	renameFn = func(oldpath, newpath string) error {
		if strings.Contains(newpath, string(filepath.Separator)+"projects"+string(filepath.Separator)) {
			return errInjectedRenameFailure
		}
		return origRename(oldpath, newpath)
	}

	err := stageAndCommitBundle(liveDir, "sess-3", files)
	if err == nil {
		t.Fatal("expected an error")
	}

	for rel := range files {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("rollback must remove landed file %s, stat err = %v", rel, statErr)
		}
	}
	if _, err := os.Stat(stagingDirFor(liveDir, "sess-3")); !os.IsNotExist(err) {
		t.Errorf("expected staging dir removed after rollback, stat err = %v", err)
	}
}

// TestStageAndCommitBundle_LiveDirectoryBlocked exercises the
// "prepare live directory" failure branch (MkdirAll on the live
// destination, distinct from the rename itself): a plain FILE
// occupying the path where the live "workflows" directory needs to
// exist makes MkdirAll fail before any rename is attempted.
func TestStageAndCommitBundle_LiveDirectoryBlocked(t *testing.T) {
	liveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(liveDir, "workflows"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}
	files := bundleFilesForCommitTest(t)

	err := stageAndCommitBundle(liveDir, "sess-5", files)
	if err == nil {
		t.Fatal("expected the blocked live directory to fail the commit")
	}
	if _, statErr := os.Stat(stagingDirFor(liveDir, "sess-5")); !os.IsNotExist(statErr) {
		t.Errorf("expected staging dir removed, stat err = %v", statErr)
	}
}

// TestRollbackLandedTargets_IgnoresRemoveErrorsButLogsThem: a remove
// failure during rollback (e.g. a permission race) must not panic or
// escalate — it's logged and rollback continues to the next target.
func TestRollbackLandedTargets_IgnoresRemoveErrorsButLogsThem(t *testing.T) {
	origRemove := removeFn
	defer func() { removeFn = origRemove }()
	calls := 0
	removeFn = func(_ string) error {
		calls++
		return errInjectedRenameFailure // generic, non-IsNotExist error
	}
	rollbackLandedTargets(t.TempDir(), []journalTarget{
		{RelPath: "workflows/a.md"},
		{RelPath: "projects/x.yaml"},
	})
	if calls != 2 {
		t.Errorf("expected rollback to attempt both targets despite the injected error, got %d calls", calls)
	}
}

func TestStageAndCommitBundle_FailureOnFirstTarget(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)

	origRename := renameFn
	defer func() { renameFn = origRename }()
	calls := 0
	renameFn = func(oldpath, newpath string) error {
		calls++
		if calls == 1 {
			return errInjectedRenameFailure
		}
		return origRename(oldpath, newpath)
	}

	err := stageAndCommitBundle(liveDir, "sess-4", files)
	if err == nil {
		t.Fatal("expected an error")
	}
	for rel := range files {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("no file should have landed, but %s exists (stat err = %v)", rel, statErr)
		}
	}
	if _, err := os.Stat(stagingDirFor(liveDir, "sess-4")); !os.IsNotExist(err) {
		t.Errorf("expected staging dir removed, stat err = %v", err)
	}
}
