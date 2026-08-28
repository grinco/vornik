package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// swarmBareImage is a swarm-file blob whose role carries a BARE agent image —
// the exact canonical-defect that re-broke the repo 3× (the agent-image
// 3×-round-trip incident, 2026-07-23-agent-image-qualification-audit.md). The
// mirror seam must qualify it before it reaches source.
const swarmBareImage = `---
swarmId: basic-swarm
displayName: Dev swarm
roles:
    - name: "lead"
      model: "zai.glm-5"
      runtime:
        image: "vornik-agent:latest"
`

// staleRoleLibrary is a wholly-canonical file with NO normalizer-covered field:
// v1 has no normalizer for it, so it must mirror VERBATIM (the documented limit).
const staleRoleLibrary = `---
roles:
    - name: coder
      prompt: an outdated canonical prompt that only a v2 reconciler could refresh
`

func writeMirrorSource(t *testing.T) (sourceRoot, sourceConfigsDir string) {
	t.Helper()
	sourceRoot = t.TempDir()
	sourceConfigsDir = filepath.Join(sourceRoot, "configs")
	if err := os.MkdirAll(filepath.Join(sourceConfigsDir, "swarms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceConfigsDir, "roles"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sourceRoot, sourceConfigsDir
}

// TestMirrorOneFile_NormalizesBareAgentImageBeforeSourceWrite is the mirror-seam
// regression for the agent-image 3×-round-trip incident: a mirror write carrying
// a bare canonical image (drafted from stale DEPLOYED content) must be qualified
// BEFORE it reaches source. Asserts both the source write and the returned notes.
func TestMirrorOneFile_NormalizesBareAgentImageBeforeSourceWrite(t *testing.T) {
	sourceRoot, sourceConfigsDir := writeMirrorSource(t)
	target, staged, notes, err := mirrorOneFile(
		sourceRoot, sourceConfigsDir, "configs/swarms/basic-swarm.md",
		[]byte(swarmBareImage), zerolog.Nop())
	if err != nil || !staged {
		t.Fatalf("mirrorOneFile: staged=%v err=%v", staged, err)
	}
	if len(notes) != 1 || notes[0].Name != "agent-image-qualify" {
		t.Fatalf("expected one agent-image-qualify note, got %+v", notes)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `image: "ghcr.io/grinco/vornik-agent:latest"`) {
		t.Errorf("source write must carry the QUALIFIED image line:\n%s", got)
	}
	if strings.Contains(string(got), `image: "vornik-agent:latest"`) {
		t.Errorf("source write still carries the bare image line:\n%s", got)
	}
}

// TestMirrorOneFile_WhollyStaleCanonicalFile_MirroredVerbatim encodes the honest
// scope boundary (LLD §7 LIMIT / F12a): a wholly-stale canonical file with no
// normalizer-covered field is still mirrored VERBATIM. v1 does NOT close the
// canonical-file class — that is owned by the v2 reconciler (§10).
func TestMirrorOneFile_WhollyStaleCanonicalFile_MirroredVerbatim(t *testing.T) {
	sourceRoot, sourceConfigsDir := writeMirrorSource(t)
	target, staged, notes, err := mirrorOneFile(
		sourceRoot, sourceConfigsDir, "configs/roles/role-library.md",
		[]byte(staleRoleLibrary), zerolog.Nop())
	if err != nil || !staged {
		t.Fatalf("mirrorOneFile: staged=%v err=%v", staged, err)
	}
	if len(notes) != 0 {
		t.Errorf("a wholly-stale canonical file must NOT be normalized in v1, got %+v", notes)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != staleRoleLibrary {
		t.Errorf("canonical file must mirror verbatim.\n got: %q\nwant: %q", got, staleRoleLibrary)
	}
}

// TestMirrorOneFile_HostLocalSwarmPath_NotNormalized is the host-local safety
// case (review A5): a projects/…/swarms/x.md-shaped path is NOT matched by
// agent-image-qualify, so even a bare image there mirrors verbatim.
func TestMirrorOneFile_HostLocalSwarmPath_NotNormalized(t *testing.T) {
	sourceRoot, sourceConfigsDir := writeMirrorSource(t)
	if err := os.MkdirAll(filepath.Join(sourceConfigsDir, "projects", "acme", "swarms"), 0o755); err != nil {
		t.Fatal(err)
	}
	target, staged, notes, err := mirrorOneFile(
		sourceRoot, sourceConfigsDir, "configs/projects/acme/swarms/x.md",
		[]byte(swarmBareImage), zerolog.Nop())
	if err != nil || !staged {
		t.Fatalf("mirrorOneFile: staged=%v err=%v", staged, err)
	}
	if len(notes) != 0 {
		t.Errorf("host-local swarm path must NOT be normalized, got %+v", notes)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != swarmBareImage {
		t.Errorf("host-local file must mirror verbatim (bare image preserved):\n%s", got)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"-C", dir, "init", "-q"},
		{"-C", dir, "config", "user.email", "test@vornik.local"},
		{"-C", dir, "config", "user.name", "vornik-test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "--all").CombinedOutput()
	if err != nil {
		// No commits yet.
		return 0
	}
	n := 0
	for _, r := range strings.TrimSpace(string(out)) {
		n = n*10 + int(r-'0')
	}
	return n
}

func lastCommitMessage(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%B").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	return string(out)
}

// TestNewProposalMirror_CommitTrailerOnNormalization asserts the end-to-end
// mirror closure: when a normalization fires, the source file is qualified AND
// the git commit carries a `mirror-normalized: <name>` trailer (review A6).
func TestNewProposalMirror_CommitTrailerOnNormalization(t *testing.T) {
	sourceRoot, sourceConfigsDir := writeMirrorSource(t)
	gitInit(t, sourceRoot)
	t.Setenv("VORNIK_CONFIGS_SOURCE_DIR", sourceConfigsDir)

	c := &Container{Logger: zerolog.Nop()}
	mirror := c.newProposalMirror()
	if mirror == nil {
		t.Fatal("newProposalMirror returned nil with a source dir set")
	}
	if err := mirror("cpp_test_1", map[string][]byte{
		"configs/swarms/basic-swarm.md": []byte(swarmBareImage),
	}); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(sourceConfigsDir, "swarms", "basic-swarm.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `image: "ghcr.io/grinco/vornik-agent:latest"`) {
		t.Errorf("committed source must carry the qualified image:\n%s", got)
	}
	msg := lastCommitMessage(t, sourceConfigsDir)
	if !strings.Contains(msg, "mirror-normalized: agent-image-qualify") {
		t.Errorf("commit message missing normalization trailer:\n%s", msg)
	}
	if !strings.Contains(msg, "control-plane: apply cpp_test_1") {
		t.Errorf("commit message missing proposal subject:\n%s", msg)
	}
}

// TestNewProposalMirror_NoChurnCommit_Idempotent proves the §3.4 steady state is
// stable, not a drift oscillation: after the first apply qualifies + commits the
// image, a SECOND apply carrying the same stale-deployed bytes normalizes to the
// identical qualified source → empty git diff → NO spurious commit (A3).
func TestNewProposalMirror_NoChurnCommit_Idempotent(t *testing.T) {
	sourceRoot, sourceConfigsDir := writeMirrorSource(t)
	gitInit(t, sourceRoot)
	t.Setenv("VORNIK_CONFIGS_SOURCE_DIR", sourceConfigsDir)

	c := &Container{Logger: zerolog.Nop()}
	mirror := c.newProposalMirror()

	files := map[string][]byte{"configs/swarms/basic-swarm.md": []byte(swarmBareImage)}
	if err := mirror("cpp_first", files); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	if n := commitCount(t, sourceConfigsDir); n != 1 {
		t.Fatalf("first apply should produce exactly 1 commit, got %d", n)
	}

	// Second apply with the same stale-deployed bytes: normalizes to the same
	// qualified source already on disk → nothing to commit.
	if err := mirror("cpp_second", map[string][]byte{
		"configs/swarms/basic-swarm.md": []byte(swarmBareImage),
	}); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	if n := commitCount(t, sourceConfigsDir); n != 1 {
		t.Errorf("second apply must NOT produce a churn commit; commit count = %d, want 1", n)
	}
}
