package podman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withStubPodman writes a fake `podman` into a temp dir and returns an
// env slice that puts it first on PATH, so the shim execs the stub
// instead of real podman.
func withStubPodman(t *testing.T, script string) []string {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub podman: %v", err)
	}
	return append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestVornikctlShim_ForwardsToContainer pins the shim contract: when the
// `vornik` container is up, it execs the containerized CLI with the
// caller's args — `podman exec -i vornik /usr/local/bin/vornikctl <args>`.
func TestVornikctlShim_ForwardsToContainer(t *testing.T) {
	env := withStubPodman(t, `#!/usr/bin/env bash
case "$1" in
  container)
    case "$2" in
      exists)  exit 0 ;;
      inspect) echo "true" ;;
    esac ;;
  exec) shift; echo "FORWARDED: $*" ;;
esac
`)
	shim, err := filepath.Abs("vornikctl")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shim, "project", "list")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed: %v\noutput: %s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "FORWARDED:") {
		t.Fatalf("shim did not exec podman: %q", got)
	}
	if !strings.Contains(got, "vornik /usr/local/bin/vornikctl project list") {
		t.Errorf("shim must forward args to the containerized CLI; got %q", got)
	}
}

// TestVornikctlShim_ErrorsWhenContainerMissing — a friendly message and
// nonzero exit when the daemon container isn't present, rather than an
// opaque podman error.
func TestVornikctlShim_ErrorsWhenContainerMissing(t *testing.T) {
	env := withStubPodman(t, `#!/usr/bin/env bash
case "$1 $2" in
  "container exists") exit 1 ;;
esac
exit 0
`)
	shim, _ := filepath.Abs("vornikctl")
	cmd := exec.Command(shim, "doctor")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected nonzero exit when container missing; output: %s", out)
	}
	if !strings.Contains(string(out), "not found") {
		t.Errorf("expected a friendly 'not found' message; got %q", out)
	}
}

// TestVornikctlShim_RespectsContainerOverride — VORNIK_CONTAINER renames
// the target container so non-default deployments still work.
func TestVornikctlShim_RespectsContainerOverride(t *testing.T) {
	env := append(withStubPodman(t, `#!/usr/bin/env bash
case "$1" in
  container)
    case "$2" in
      exists)  exit 0 ;;
      inspect) echo "true" ;;
    esac ;;
  exec) shift; echo "FORWARDED: $*" ;;
esac
`), "VORNIK_CONTAINER=vornik-test")
	shim, _ := filepath.Abs("vornikctl")
	cmd := exec.Command(shim, "doctor")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "vornik-test /usr/local/bin/vornikctl doctor") {
		t.Errorf("shim must honor VORNIK_CONTAINER; got %q", out)
	}
}
