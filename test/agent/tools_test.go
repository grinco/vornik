// Package agent_test runs shell-level tests for the built-in tools
// implemented in images/vornik-agent/entrypoint.sh. Go can't easily
// unit-test a bash script directly, so this wrapper execs tools_test.sh
// and surfaces its pass/fail through the standard Go test runner.
package agent_test

import (
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

	runShellTest(t, script)
}
