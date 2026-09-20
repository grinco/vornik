package controlplane

import (
	"path/filepath"
	"strings"
	"testing"
)

// Named roots — config-apply-journal design §9.2b. The workspace-context write
// could not be journalled because resolveTarget joins every op under one
// ConfigDir while the workspace tree lives outside it. That left a SECOND
// recovery protocol beside the journal, which is two mechanisms for one
// invariant.

func TestResolveTarget_DefaultRootIsUnchanged(t *testing.T) {
	cfg := t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg}

	got, err := e.resolveTarget("configs/swarms/x.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "configs/swarms/x.md"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestResolveTarget_NamedRootResolvesElsewhere(t *testing.T) {
	cfg, ws := t.TempDir(), t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg, Roots: map[string]string{"workspace": ws}}

	got, err := e.resolveTarget("workspace/proj-1/.autonomy/PROJECT_CONTEXT.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if strings.HasPrefix(got, cfg) {
		t.Fatal("a named-root op resolved under the config dir")
	}
}

// Containment still applies INSIDE the named root — the guard is the same
// guard, not a second one.
func TestResolveTarget_NamedRootStillRefusesTraversal(t *testing.T) {
	e := &ApplyEngine{ConfigDir: t.TempDir(), Roots: map[string]string{"workspace": t.TempDir()}}

	for _, rel := range []string{
		"workspace/../etc/passwd",
		"workspace/proj/../../escape",
	} {
		if _, err := e.resolveTarget(rel); err == nil {
			t.Fatalf("traversal through a named root was allowed: %q", rel)
		}
	}
}

// A path whose first segment is NOT a configured root resolves under the config
// dir, exactly as before. Otherwise adding a root would silently re-target
// every op that happens to start with that word.
func TestResolveTarget_UnknownFirstSegmentIsAConfigPath(t *testing.T) {
	cfg := t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg, Roots: map[string]string{"workspace": t.TempDir()}}

	got, err := e.resolveTarget("workflows/dev-pipeline.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "workflows/dev-pipeline.md"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// A root NAME on its own is not a target: it names a directory, and an op that
// writes a root is a bug, not a file.
func TestResolveTarget_BareRootNameIsRefused(t *testing.T) {
	e := &ApplyEngine{ConfigDir: t.TempDir(), Roots: map[string]string{"workspace": t.TempDir()}}
	if _, err := e.resolveTarget("workspace"); err == nil {
		t.Fatal("a bare root name was accepted as a write target")
	}
	if _, err := e.resolveTarget("workspace/"); err == nil {
		t.Fatal("a bare root name with a trailing slash was accepted")
	}
}

// An engine with no Roots behaves exactly as it did before they existed.
func TestResolveTarget_NoRootsConfigured(t *testing.T) {
	cfg := t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg}
	got, err := e.resolveTarget("workspace/proj/file.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "workspace/proj/file.md"); got != want {
		t.Fatalf("want the config-rooted path, got %q", got)
	}
}
