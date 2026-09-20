package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/registry"
)

// CA-19's residue: the journal's generation_before/after pair is supposed to be
// evidence that the running registry re-parsed what the apply wrote. It could
// not be, because the marker was a digest over the resolved project set — it
// says WHAT is resolved, never that anything was re-read. An apply whose reload
// silently did nothing produced the same two values as one that worked.
//
// These tests pin the two halves of the closure: the marker now carries a real
// activation counter, and the verification refuses when that counter has not
// moved.

func writeOp(t *testing.T, root, rel, content string) controlplane.JournaledOp {
	t.Helper()
	target := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return controlplane.JournaledOp{
		Path:          rel,
		ContentSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
	}
}

// activate promotes a snapshot the way a reload does. Load on an empty tree is
// a legitimate activation — it resolves zero projects — which is what makes it
// the right stand-in here: the point of the counter is that an activation
// counts even when nothing about the resolved content changed.
func activate(t *testing.T, r *registry.Registry, dir string) {
	t.Helper()
	if err := r.Load(dir); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

// The marker must change when the registry re-parses, even if the resolved
// project set is byte-identical — that case is exactly the one the digest
// could not report.
func TestConfigGeneration_MovesOnReparseWithIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	before := c.configGeneration()
	activate(t, c.Registry, dir)
	after := c.configGeneration()

	if before == after {
		t.Fatalf("generation marker did not move across a re-parse: %q", before)
	}
	if !strings.Contains(after, ":") {
		t.Fatalf("marker should carry a counter and a digest, got %q", after)
	}
}

// The content check that already shipped must keep working: a file that does
// not hold what the apply intended fails the journal.
func TestVerifyConfigGeneration_RefusesRevertedContent(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	op := writeOp(t, dir, "configs/swarms/x.md", "intended")
	before := c.configGeneration()
	activate(t, c.Registry, dir)

	// Something put the old bytes back after the apply.
	if err := os.WriteFile(filepath.Join(dir, "configs/swarms/x.md"), []byte("reverted"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before)
	if err == nil {
		t.Fatal("want a refusal when the target does not hold the applied content")
	}
	if !strings.Contains(err.Error(), "does not hold the applied content") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The new half: bytes correct on disk, but the registry never re-parsed. Before
// this counter existed, this passed — which is precisely CA-19.
func TestVerifyConfigGeneration_RefusesWhenTheRegistryDidNotReparse(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	op := writeOp(t, dir, "configs/swarms/x.md", "intended")
	before := c.configGeneration()
	// NO activation here: this is the silent-no-op reload.

	err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before)
	if err == nil {
		t.Fatal("want a refusal when the registry generation did not advance")
	}
	if !strings.Contains(err.Error(), "did not re-parse") {
		t.Fatalf("the refusal must say what was not confirmed, got: %v", err)
	}
}

// The happy path: content correct AND the registry re-parsed.
func TestVerifyConfigGeneration_AcceptsAReparseThatHoldsTheContent(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	op := writeOp(t, dir, "configs/swarms/x.md", "intended")
	before := c.configGeneration()
	activate(t, c.Registry, dir)

	if err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before); err != nil {
		t.Fatalf("want acceptance, got %v", err)
	}
}

// An empty op set is not a claim about anything, and must not be turned into
// one by the new check: a restart-only apply reloads nothing.
func TestVerifyConfigGeneration_EmptyOpsIsNotAClaim(t *testing.T) {
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml"), Registry: registry.New()}
	if err := c.verifyConfigGeneration(context.Background(), nil, c.configGeneration()); err != nil {
		t.Fatalf("empty ops must pass, got %v", err)
	}
}

// §9.2c step 3. verifyConfigGeneration re-reads each op's target to confirm the
// bytes survived the reload. It resolved every path under the config dir, so a
// workspace-rooted op would send it looking in the wrong tree and fail the
// journal on a write that SUCCEEDED. Found while designing the cutover, which
// is why the cutover is four steps and not a registration.
func TestVerifyConfigGeneration_ResolvesANamedRoot(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	c := &Container{
		ConfigPath: filepath.Join(dir, "config.yaml"),
		Registry:   registry.New(),
		applyRoots: map[string]string{"workspace": ws},
	}

	op := writeOp(t, ws, "proj-1/.autonomy/PROJECT_CONTEXT.md", "the context")
	op.Path = "workspace/proj-1/.autonomy/PROJECT_CONTEXT.md"
	before := c.configGeneration()
	activate(t, c.Registry, dir)

	if err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before); err != nil {
		t.Fatalf("a workspace-rooted op failed verification of a successful write: %v", err)
	}
}

// And it must still CATCH a reverted workspace write — the root awareness may
// not turn the check into a pass-through.
func TestVerifyConfigGeneration_CatchesARevertedWorkspaceWrite(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	c := &Container{
		ConfigPath: filepath.Join(dir, "config.yaml"),
		Registry:   registry.New(),
		applyRoots: map[string]string{"workspace": ws},
	}

	op := writeOp(t, ws, "proj-1/.autonomy/PROJECT_CONTEXT.md", "intended")
	op.Path = "workspace/proj-1/.autonomy/PROJECT_CONTEXT.md"
	before := c.configGeneration()
	activate(t, c.Registry, dir)
	if err := os.WriteFile(filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"), []byte("reverted"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before); err == nil {
		t.Fatal("a reverted workspace write passed verification")
	}
}
