package agent_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGoldenFixturesAreTracked pins the lesson of 2026-09-05: the 64 dispatch
// goldens sat untracked for a week because .gitignore's `*.out` rule swallowed
// them, so the golden passed on the machine that recorded it and CI — which
// had no fixtures — failed on the release push. A fixture that exists only on
// one disk is not a golden. Every file under fixtures/ must be known to git;
// outside a git checkout (an exported tarball) the test has nothing to say.
func TestGoldenFixturesAreTracked(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtures := filepath.Join(filepath.Dir(thisFile), "fixtures")
	if err := exec.Command("git", "-C", fixtures, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skip("not a git checkout — nothing to compare against")
	}
	// Everything on disk under fixtures/ that git does not track, honouring
	// .gitignore (the failure mode) and excluding nothing.
	out, err := exec.Command("git", "-C", fixtures, "ls-files", "--others", "--exclude-standard", "--ignored", ".").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	ignored := strings.TrimSpace(string(out))
	out, err = exec.Command("git", "-C", fixtures, "ls-files", "--others", "--exclude-standard", ".").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	untracked := strings.TrimSpace(string(out))
	if ignored != "" || untracked != "" {
		t.Fatalf("fixture files git does not track — CI cannot see them:\nignored:\n%s\nuntracked:\n%s", ignored, untracked)
	}
}
