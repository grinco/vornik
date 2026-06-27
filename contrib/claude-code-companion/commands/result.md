---
description: Pull the final result of a completed vornik task into context
allowed-tools: mcp__vornik__result, mcp__vornik__status
argument-hint: <task_id>
---

# Pull a vornik task's result

The user wants the final output of task `$ARGUMENTS` pulled into your
context window.

Call `mcp__vornik__result` with `task_id=$ARGUMENTS`. Branch on the shape:

- **`complete: false`** — the task hasn't finished yet. Show the current
  status from the result and suggest the user retry in a bit, or use
  `/status $ARGUMENTS` for the live read.
- **`complete: true`** — the task is terminal. The `artifacts` array
  carries the task's output documents inline: one entry per artifact
  with `name`, `artifact_id`, and `content` (the body). Summarize the
  key findings for the user — don't dump the raw artifact unless they
  ask; distill it.
  - An entry with `truncated: true` means the inline budget (64 KiB
    across all artifacts) cut the body short — say so, and offer to
    delegate a `companion-report-summarize` run on the task if the
    user needs the rest distilled.
  - An entry with `content_error` could not be read server-side;
    surface the error.
  - (Pre-2026-06-07 daemons returned an `artifacts_url` instead —
    that REST surface is NOT reachable with a companion key; if you
    see that shape, tell the user to upgrade the daemon.)

If `last_error` is present, lead with that — the host LLM (you) is in a
better position to suggest a recovery than the vornik agent was.
