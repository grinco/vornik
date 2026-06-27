---
description: Delegate work to a vornik companion workflow asynchronously
allowed-tools: mcp__vornik__delegate, mcp__vornik__status, mcp__vornik__catalog
argument-hint: <workflow-id> "<prompt>"
---

# Delegate to vornik

You are about to offload work to a vornik companion workflow rather than
doing it inline. This is the right move when:

- The task takes more than ~30 seconds of context-heavy reasoning that
  doesn't need the user's session to make progress.
- A cheaper / different model can do it well (architectural review,
  test-coverage audit, doc review, data validation, research gather,
  summarization).
- The result can land on the next session via the SessionStart digest
  rather than blocking the user here.

The user's arguments are: `$ARGUMENTS`

> **NEVER hand a FILE-BEARING workflow a file PATH in the prompt —
> attach the file instead.** Paths in a delegate prompt are NOT uploaded;
> the vornik agent runs in a container that can't see the host
> filesystem. The failure modes differ by workflow but are all wrong:
> `companion-rag-ingest` silently no-ops (the 2026-06-05 incident:
> COMPLETED with "ingestion skipped"); `companion-architectural-review`
> of a doc can't read the path and falls back to reviewing stale RAG
> revisions (contradictory, out-of-date findings — v1.1.0 refuses this
> and returns "no reviewable content" when nothing is attached).
> To feed files into ANY workflow, use `/rag-ingest` (files →
> RAG) or `/upload <workflow> "<prompt>" <path…>` (files → any
> workflow, e.g. `companion-architectural-review` for a doc review) —
> both read the bytes locally and stage them as base64 `inputArtifacts`.
> The daemon rejects artifact-less delegations to workflows that set
> `require_input_artifacts` (visible in `catalog()`).

## What to do

1. If the user only gave a prompt (no workflow ID), call
   `mcp__vornik__catalog` to see what workflows their key permits, then
   ask the user which one to use (or pick the obvious match).
2. Call `mcp__vornik__delegate` with the chosen `workflow` and `prompt`.
3. Report the `task_id` back with a one-line summary of what was queued
   and where the user will see the result (next session's digest or
   `/result <task_id>`).

Do not poll for completion inside this slash command — the point of
delegating is to walk away.
