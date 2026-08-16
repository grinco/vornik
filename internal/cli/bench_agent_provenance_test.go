package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

// Regression, 2026-08-16. A long-horizon arm journaled durationMs=0 for all 14
// records while the ledger held the durations: the vornikctl scoring it was 27
// commits stale and predated the fix that reads them. It was reached through a
// bare `vornikctl` on $PATH, and nothing in the run refused or warned.
//
// A harness that cannot be traced to a commit is the case this gate can
// actually detect — a stale-but-committed binary is caught by the printed
// revisions and by MergeJournals, but an unstamped or dirty one cannot be
// reproduced at all and must not silently produce a publishable figure.
func TestScoringProvenanceError(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		allow   bool
		wantErr bool
	}{
		{"a committed build scores", "872a06b969ae", false, false},
		{"an unknown build is refused", agentbench.UnknownHarnessBuild, false, true},
		{"a dirty build is refused", "872a06b969ae+dirty", false, true},
		{"an empty build is refused", "", false, true},
		{"the override is accepted when asked for by name", "872a06b969ae+dirty", true, false},
		{"the override does not change a good build", "872a06b969ae", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := scoringProvenanceError(tc.harness, tc.allow)
			if tc.wantErr && err == nil {
				t.Fatalf("harness %q must be refused", tc.harness)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("harness %q must be accepted, got %v", tc.harness, err)
			}
		})
	}
}

// The refusal has to tell an operator what to DO. A gate whose message does not
// name the remedy gets bypassed with the override rather than fixed.
func TestScoringProvenanceError_NamesTheRemedy(t *testing.T) {
	err := scoringProvenanceError(agentbench.UnknownHarnessBuild, false)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--i-know-this-is-unreproducible") {
		t.Errorf("message must name the override: %v", err)
	}
	if !strings.Contains(err.Error(), "Build the harness") {
		t.Errorf("message must name the fix, not just the override: %v", err)
	}
}

// BinaryBuild reads the OTHER binary's stamp — the daemon under test. Without
// it a journal can say which commit scored a run but not which commit was
// scored, which is half the pairing the incident needed.
func TestBinaryBuild_ReadsAnotherBinarysRevision(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "probe")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build a probe binary: %v (%s)", err, out)
	}

	// Built outside any repo, so it carries no VCS data — which must read as
	// "unknown" rather than as an empty string a caller could mistake for
	// "not applicable".
	if got := agentbench.BinaryBuild(bin); got != agentbench.UnknownHarnessBuild {
		t.Errorf("BinaryBuild(no-vcs binary) = %q, want %q", got, agentbench.UnknownHarnessBuild)
	}
}

func TestBinaryBuild_UnreadableOrAbsent(t *testing.T) {
	if got := agentbench.BinaryBuild(""); got != "" {
		t.Errorf("BinaryBuild(\"\") = %q, want \"\" — no daemon named is not the same as unidentifiable", got)
	}
	if got := agentbench.BinaryBuild(filepath.Join(t.TempDir(), "nope")); got != agentbench.UnknownHarnessBuild {
		t.Errorf("BinaryBuild(missing) = %q, want %q", got, agentbench.UnknownHarnessBuild)
	}
}
