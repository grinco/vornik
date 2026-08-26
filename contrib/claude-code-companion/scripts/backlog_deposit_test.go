// Package scripts_test runs shell-level tests for the companion plugin's
// backlog-deposit script. Go can't unit-test bash directly, so this wrapper
// execs the shell suites and surfaces pass/fail through the standard Go test
// runner — the same pattern as hooks/session_start_test.go and
// test/agent/tools_test.go.
//
// Two suites run here:
//   - vornik-backlog-deposit_test.sh — the dedup, secret-scan, insert-only and
//     structure-preservation behaviour.
//   - parity_test.sh — asserts the claude-code and codex copies of the script
//     are byte-identical. A plugin installs as a self-contained directory and
//     cannot reach a sibling, so duplication IS the distribution mechanism —
//     and duplication drifts. Without this, one client could silently dedup or
//     secret-scan differently from the other.
package scripts_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func runShellSuite(t *testing.T, name string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), name)

	cmd := exec.Command("bash", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\n---\n%s", name, err, out.String())
	}
	t.Logf("%s output:\n%s", name, out.String())
}

func TestBacklogDepositScript(t *testing.T) {
	runShellSuite(t, "vornik-backlog-deposit_test.sh")
}

// TestCompanionScriptParity fails the build when the two shipped copies of the
// deposit script drift apart.
func TestCompanionScriptParity(t *testing.T) {
	runShellSuite(t, "parity_test.sh")
}
