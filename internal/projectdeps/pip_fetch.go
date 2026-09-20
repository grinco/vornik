package projectdeps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultPipBin is the interpreter-mediated pip invocation. `python3 -m pip`
// rather than a bare `pip` so the materialisation targets the same interpreter
// the agent image runs.
var DefaultPipBin = []string{"python3", "-m", "pip"}

// PipFetcher returns a Fetcher that installs a hash-pinned lockfile into a
// staging directory.
//
// The argv is FIXED. The only operator-controlled input is the lockfile's path
// and contents — there is no `install:` field and no shell (design §5.1),
// because a project config is editable through the control plane, which is
// reachable from chat, API, A2A and MCP alike, and the 2026-08-03 ruling
// forbids a remote-triggered exec on every one of them.
//
// --require-hashes is passed as well as validated: RequireHashPinned refuses a
// lockfile this design cannot key, and pip's own flag is what refuses an
// artifact whose bytes do not match the hash it was pinned to. They answer
// different questions and both are wanted.
func PipFetcher(bin []string, lockfilePath string) Fetcher {
	if len(bin) == 0 {
		bin = DefaultPipBin
	}
	return func(ctx context.Context, dir string) error {
		target := filepath.Join(dir, SiteDir)
		if err := os.MkdirAll(target, 0o700); err != nil {
			return fmt.Errorf("create site dir: %w", err)
		}
		args := append(append([]string{}, bin[1:]...),
			"install",
			"--require-hashes",
			"--no-deps", // the lockfile is already the closed set
			"--disable-pip-version-check",
			"--no-input",
			"--target", target,
			"--requirement", lockfilePath,
		)
		cmd := exec.CommandContext(ctx, bin[0], args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// pip's own diagnostics are the useful half of this failure —
			// a hash mismatch or an unreachable index reads nothing like
			// "exit status 1". Bounded so one broken lockfile cannot fill
			// the journal.
			return fmt.Errorf("pip install into %s failed: %w: %s", target, err, tailLines(string(out), 40))
		}
		return nil
	}
}

// tailLines keeps the last n lines of a command's output.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "…\n" + strings.Join(lines[len(lines)-n:], "\n")
}
