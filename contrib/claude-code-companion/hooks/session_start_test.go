// Package hooks_test runs shell-level tests for the companion plugin's
// SessionStart hook (session-start.sh). Go can't unit-test bash directly, so
// this wrapper execs session_start_test.sh and surfaces pass/fail through the
// standard Go test runner (same pattern as test/agent/tools_test.go).
package hooks_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSessionStartHook(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), "session_start_test.sh")

	cmd := exec.Command("bash", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("session_start_test.sh failed: %v\n---\n%s", err, out.String())
	}
	t.Logf("session_start_test.sh output:\n%s", out.String())
}
