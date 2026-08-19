package executor

import "strings"

// Workspace-git guidance injected when the project is mounted as a git worktree.
//
// WHY THIS EXISTS. internal/runtime/manager.go mounts the project's main .git
// READ-ONLY (`%s:%s:ro,z`) while the worktree itself is mounted rw. A git
// worktree keeps its index, HEAD and logs under the MAIN repo's
// .git/worktrees/<name>/, so every write from inside the worktree — `git add`,
// `git commit`, `git stash`, and the index stat-cache write `git status`
// performs — lands on the read-only mount and fails. An agent can read git
// state and can never change it.
//
// Nothing told the agents. Measured over the agent-benchmark ledger on
// 2026-08-18: of 108 executions killed by the degenerate-loop detector, the
// final repeated command was a git command in 96 (89%) — 64 reads whose output
// never changes, 32 writes that can never land. Agents also invented a
// workaround, a second git dir at `.git-local`, which exists in the bench
// workspace, appears in no source code, and later agents now reuse.
//
// Same failure class as the plausibility rules and the pinned-case producer
// contract: a constraint enforced in one place and stated nowhere the agent can
// read. The daemon has always committed the agent's work itself
// (autoCommitLeftoverChanges, run from the host where .git IS writable, just
// before the worktree merges), so an agent never needed to commit — it was
// simply never told.
//
// GATED ON THE CONDITION BEING TRUE. Unlike reporting integrity, this describes
// a deployment fact rather than a universal rule: a project that is not mounted
// as a worktree has a perfectly writable git. Emitting the block there would
// state something false, which is worse than omitting it, so the caller passes
// the same predicate the runtime uses to decide the mount.
//
// STATES THE NORM AND THE SAFE PATH. It tells the agent what it cannot do, what
// happens instead, and what to do rather than retrying — because the observed
// failure is not disobedience, it is an agent retrying a reasonable command
// whose result never changes.
const workspaceGitSystemPromptBlock = `
WORKSPACE GIT — your checkout's git metadata is READ-ONLY, and commits are not your job.

Your work is committed FOR you: the harness commits everything left in the
workspace when the step finishes, so uncommitted changes are never lost. Do not
run git add, git commit, git stash, or anything else that writes git state —
those commands cannot succeed here, and repeating one will not change that.

Read files with your file-reading tools rather than through git. If a git
command returns nothing, an error, or the same output twice, treat that as the
answer and move on: running it again will return the same thing and burns the
step's budget.
`

// composeSystemPromptWithWorkspaceGit appends the workspace-git block when the
// project is mounted as a read-only-git worktree.
//
// Appended after the role identity like the other capability blocks. A no-op
// when readOnlyGit is false so a non-worktree deployment's prompt is unchanged,
// and a no-op on an empty prompt for the reason claim_verification_prompt.go
// documents: when nothing else composes a system prompt the entrypoint applies
// its own default, and emitting a bare block would replace that default.
func composeSystemPromptWithWorkspaceGit(prompt string, readOnlyGit bool) string {
	if !readOnlyGit {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return prompt
	}
	return prompt + "\n" + workspaceGitSystemPromptBlock
}
