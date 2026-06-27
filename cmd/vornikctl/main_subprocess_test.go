// Package main subprocess tests for vornikctl entrypoint.
//
// Strategy: exec the test binary with SWARMCTL_TEST_MAIN=1 so the
// TestMain hook re-runs main() with a synthetic os.Args. This gives
// us real coverage of the entrypoint (including os.Exit / error
// printing) without requiring a network daemon. We exercise three
// canonical, network-free invocations:
//
//   - `vornikctl --help`          (cobra's root help, exit 0)
//   - `vornikctl version`         (version subcommand, exit 0)
//   - `vornikctl bogus-cmd`       (unknown command, exit 1)
//
// Each subprocess case touches cli.SetVersion → cli.Execute → os.Exit
// in main, which is the entire body of main.go.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain delegates to the normal Go test runner unless the env flag
// is set, in which case it invokes main() with whatever os.Args were
// passed via SWARMCTL_TEST_ARGS (semicolon-separated).
func TestMain(m *testing.M) {
	if os.Getenv("SWARMCTL_TEST_MAIN") == "1" {
		argLine := os.Getenv("SWARMCTL_TEST_ARGS")
		args := []string{"vornikctl"}
		if argLine != "" {
			args = append(args, strings.Split(argLine, ";")...)
		}
		os.Args = args
		main()
		// main() calls os.Exit on error; if we reach here it succeeded.
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMainSubprocess re-execs the test binary with the SWARMCTL_TEST_MAIN
// hook so main() runs in a fresh process. Returns combined stdout+stderr
// and the exit code.
func runMainSubprocess(t *testing.T, args ...string) (string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestMain")
	cmd.Env = append(os.Environ(),
		"SWARMCTL_TEST_MAIN=1",
		"SWARMCTL_TEST_ARGS="+strings.Join(args, ";"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(out), exitErr.ExitCode()
		}
		return string(out), -1
	}
	return string(out), 0
}

// TestMainHelp covers main() with `--help`; Cobra prints help and exits
// cleanly. main() shouldn't print the "error:" line.
func TestMainHelp(t *testing.T) {
	out, code := runMainSubprocess(t, "--help")
	if code != 0 {
		t.Fatalf("--help exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "vornikctl") {
		t.Errorf("expected help output to mention 'vornikctl', got:\n%s", out)
	}
}

// TestMainVersion covers main() with the `version` subcommand, which
// prints a version string. This exercises cli.SetVersion writing into
// the cli package's Version variable.
func TestMainVersion(t *testing.T) {
	out, code := runMainSubprocess(t, "version")
	if code != 0 {
		t.Fatalf("version exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "vornikctl version") {
		t.Errorf("expected 'vornikctl version' in output, got:\n%s", out)
	}
}

// TestMainUnknownCommand covers main()'s error-handling branch: Cobra
// returns an error for an unknown command, main() prints "error: ..."
// to stderr and exits non-zero.
func TestMainUnknownCommand(t *testing.T) {
	out, code := runMainSubprocess(t, "definitely-not-a-real-subcommand")
	if code == 0 {
		t.Fatalf("expected non-zero exit for unknown command; output:\n%s", out)
	}
	// Cobra prints either "unknown command" or routes through the error
	// fmt-fprintf in main(); both contain enough signal to assert.
	if !strings.Contains(out, "unknown") && !strings.Contains(out, "error") {
		t.Errorf("expected 'unknown' or 'error' in output, got:\n%s", out)
	}
}
