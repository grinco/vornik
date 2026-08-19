package executor

import (
	"strings"
	"testing"
)

// Measured 2026-08-18 over the agent-benchmark ledger: of 108 executions killed
// by the degenerate-loop detector, the final repeated command was a git command
// in 96 (89%) — 64 reads whose output never changes, 32 writes that can never
// land, because internal/runtime/manager.go mounts the project's main .git
// read-only while the worktree is rw. A worktree keeps its index/HEAD/logs under
// that main .git, so `git add` / `git commit` / `git stash` fail from inside it.
//
// Nothing told the agents, and they invented `.git-local` — a second git dir
// present in the workspace, absent from all source, now reused by later agents.
// The daemon has always committed their work itself (autoCommitLeftoverChanges),
// so committing was never their job.
func TestWorkspaceGitBlock_StatesTheRuleAndTheSafePath(t *testing.T) {
	got := composeSystemPromptWithWorkspaceGit("You are the coder role.", true)

	if !strings.Contains(got, "You are the coder role.") {
		t.Error("the role identity must survive composition")
	}
	for _, want := range []string{"READ-ONLY", "committed FOR you", "git commit"} {
		if !strings.Contains(got, want) {
			t.Errorf("block is missing %q — an agent needs the rule, what happens instead, "+
				"and the named commands it must not retry\n%s", want, got)
		}
	}
	// The observed failure is an agent retrying a command whose result never
	// changes, so the block has to say that repeating is futile — naming the
	// prohibition alone would not have prevented any of the 96.
	if !strings.Contains(got, "same") {
		t.Errorf("block never tells the agent a repeat returns the same result:\n%s", got)
	}
}

// A project that is not mounted as a worktree has a perfectly writable git.
// Emitting the block there would state something false, which is worse than
// omitting it — hence the gate is the runtime predicate, not an operator knob.
func TestWorkspaceGitBlock_SilentWhenGitIsWritable(t *testing.T) {
	const prompt = "You are the coder role."
	if got := composeSystemPromptWithWorkspaceGit(prompt, false); got != prompt {
		t.Errorf("non-worktree deployment must get an unchanged prompt, got:\n%s", got)
	}
}

// Mirrors claim_verification_prompt.go's guard: when nothing else composed a
// system prompt the entrypoint applies its own default, and emitting a bare
// block would REPLACE that default with a paragraph about git.
func TestWorkspaceGitBlock_DoesNotManufactureAPrompt(t *testing.T) {
	if got := composeSystemPromptWithWorkspaceGit("", true); got != "" {
		t.Errorf("empty prompt must stay empty, got:\n%s", got)
	}
	if got := composeSystemPromptWithWorkspaceGit("   ", true); got != "   " {
		t.Errorf("whitespace-only prompt must stay unchanged, got:\n%q", got)
	}
}
