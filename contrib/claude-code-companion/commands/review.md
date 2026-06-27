---
description: Async architectural review of the current branch's diff
allowed-tools: mcp__vornik__delegate, Bash(git diff:*), Bash(git log:*)
---

# Async architectural review

Shortcut for the most common companion delegation: run the
`companion-architectural-review` workflow on the current branch's diff.

## Use it as a merge gate (recommended default)

Treat companion architectural review as a **gate before merging anything
non-trivial** — features, refactors, security-sensitive changes, design docs.
The pattern that produces the best results:

1. Commit your work on a branch.
2. Write the committed diff to a file and upload it (do **not** inline a large
   diff): `git diff <base>...<branch> > /tmp/review.diff` then
   `/upload companion-architectural-review '<single-quoted prompt naming the change + what to scrutinize>' /tmp/review.diff`.
   Uploading the diff as a staged **input artifact** is the source of truth —
   the workflow (v1.1.0+) reviews the attached file, never project memory
   (RAG holds stale/multiple revisions → contradictory findings).
3. **Block on the verdict.** Fix every Critical/Important finding, re-review,
   and only merge once it comes back clean. This catches real bugs (shutdown
   races, injection-bypass, concurrency TOCTOU) that pass tests but would ship
   defects — it has repeatedly earned its keep.
4. Review **design docs (LLDs) the same way** before planning/implementation:
   `/upload companion-architectural-review '<prompt>' docs/.../my-design.md`.

**Shell constraint (important):** in `/upload`, single-quote the prompt and use
**no** double-quotes or backticks anywhere inside it — they break the command's
bash wrapper. Phrase examples as plain words (e.g. "the act-as rule"), not
`"act as"`.

For long work, **delegate proactively** — kick off reviews/audits/research at
task start so they run while you continue, rather than blocking.

---

To review a small branch diff inline instead of via a file, this command folds
the diff text into the delegate prompt directly. To review a
**document/file**, always attach it via
`/upload companion-architectural-review '<prompt>' <path>` (per the gate
pattern above) — the agent container can't read your repo, so naming a path in
a plain delegate prompt falls back to stale RAG chunks.

## What to do

1. Run `git diff main...HEAD` (or `git diff origin/main...HEAD` if main
   is tracked remotely) to capture the branch's full diff. Cap output
   at ~4000 lines via head/tail if needed — anything longer should
   probably be reviewed in chunks anyway.
2. Run `git log --oneline main...HEAD` to capture the commit context.
3. Call `mcp__vornik__delegate` with:
   - `workflow`: `"companion-architectural-review"`
   - `prompt`: a brief framing line ("Review this branch for
     architectural concerns") followed by the commit list and diff.
4. Report the `task_id` and remind the user they'll see the verdict on
   their next session's digest, or via `/result <task_id>` once
   complete.

The user's free-form additions (extra framing, areas to focus on) are:
`$ARGUMENTS`. Fold those into the prompt when present.
