// Package agent_test runs shell-level tests for the built-in tools
// implemented in images/vornik-agent/entrypoint.sh. Go can't easily
// unit-test a bash script directly, so this wrapper execs tools_test.sh
// and surfaces its pass/fail through the standard Go test runner.
package agent_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgentBuiltinTools(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), "tools_test.sh")

	cmd := exec.Command("bash", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("tools_test.sh failed: %v\n---\n%s", err, out.String())
	}
	t.Logf("tools_test.sh output:\n%s", out.String())
}
