package agent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	helperOnce sync.Once
	helperDir  string
	helperErr  error
)

// buildHelper compiles cmd/agent-helper once per test binary and returns the
// directory the shell tests prepend to PATH through VORNIK_HELPER_DIR. In the
// image the binary is on PATH already; outside it the eleven filesystem/git
// tools have no implementation without this (agent-tool dispatch design §5).
func buildHelper(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			helperErr = os.ErrNotExist
			return
		}
		root := filepath.Join(filepath.Dir(thisFile), "..", "..")
		dir, err := os.MkdirTemp("", "vornik-agent-helper-")
		if err != nil {
			helperErr = err
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "vornik-agent-helper"), "./cmd/agent-helper")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			helperErr = err
			t.Logf("go build ./cmd/agent-helper: %v\n%s", err, out)
			return
		}
		helperDir = dir
	})
	if helperErr != nil {
		t.Fatalf("building vornik-agent-helper: %v", helperErr)
	}
	return helperDir
}

// runShellTest runs one of the bash suites with the helper on PATH.
func runShellTest(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "VORNIK_HELPER_DIR="+buildHelper(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n---\n%s", filepath.Base(script), err, out)
	}
	t.Logf("%s output:\n%s", filepath.Base(script), out)
}

// TestToolDispatchGolden runs the byte-for-byte golden over the eleven
// tools now implemented in Go, against fixtures recorded from bash.
func TestToolDispatchGolden(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	runShellTest(t, filepath.Join(filepath.Dir(thisFile), "tool_dispatch_golden_test.sh"))
}
