package executor

import (
	"context"
	"os/exec"
)

// gitRunner abstracts running git subcommands so the executor's git-backed
// helpers (gitHEAD, resetWorkspace, cleanProjectDir, claim verification, and
// — converted separately — worktree management) become unit-testable without
// a real repository on disk.
//
// Production uses execGitRunner via the package-level `gitExec`; tests swap
// gitExec for a fake (see withGitRunner in the test file). The default is
// behaviour-preserving: it shells out exactly as the previous inline
// exec.CommandContext(ctx, "git", ...) calls did, including the .Output()
// vs .CombinedOutput() distinction the call sites relied on.
//
// This is the P2 code-quality seam (2026-06-19). It is introduced as a
// package var rather than an Executor field because the git helpers are
// package functions, not methods — routing through the var converts them
// with zero call-site churn. The high-churn worktree.go sites are migrated
// onto this same seam in a follow-up.
type gitRunner interface {
	// output runs `git <args...>` and returns stdout (mirrors exec.Cmd.Output()).
	output(ctx context.Context, args ...string) ([]byte, error)
	// combined runs `git <args...>` and returns stdout+stderr
	// (mirrors exec.Cmd.CombinedOutput()); also used where the caller only
	// cared about the exit status (the former .Run() sites).
	combined(ctx context.Context, args ...string) ([]byte, error)
}

type execGitRunner struct{}

func (execGitRunner) output(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", args...).Output()
}

func (execGitRunner) combined(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", args...).CombinedOutput()
}

// gitExec is the package-level git runner. Production leaves it as the real
// exec-backed implementation; tests swap it via withGitRunner.
var gitExec gitRunner = execGitRunner{}
