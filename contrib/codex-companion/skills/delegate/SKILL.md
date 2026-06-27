---
name: delegate
description: |
  Teaches Codex when to offload work to a vornik companion workflow
  instead of doing it inline. Use when the vornik MCP tools are available
  and the task is large, async-friendly, or belongs in project memory.
---

# When to reach for vornik

The vornik companion runs work asynchronously in containers managed by the
vornik daemon. It is separate from this Codex session and can use configured
models, isolated runtimes, project workflows, and project RAG memory. Use it
when the work can progress without blocking this turn.

## First rule: recall before delegate

When the user asks for research, doc review, project history, or anything like
"what do we know about X", call `mcp__vornik__recall` first. If it returns a
strong recent hit, summarize that hit and ask whether the user still wants fresh
work delegated.

Use `delegate` only when recall is empty, stale, weak, or the user explicitly
wants a fresh second pass.

## Delegate when

- The task will take more than about 30 seconds of context-heavy work.
- The user does not need the answer in this exact turn.
- The work maps to a companion workflow from `mcp__vornik__catalog`.
- The work would consume a large amount of Codex context if done inline.
- A separate model or isolated container is a better execution environment.

Good fits: architectural review, test-coverage audit, doc review, data
validation, research gathering, report summarization, and bulk RAG ingest.

Do not delegate small tasks, tasks that depend on private session-only context
you cannot put in the prompt or artifacts, or sensitive files the user has not
cleared for offload.

## Merge-gate habit

For non-trivial code changes and design docs, use
`companion-architectural-review` as a second-opinion gate before finalizing.
For large diffs or documents, attach the committed diff or document as an input
artifact. Do not paste huge diffs into the prompt unless they are small enough
to be clearly manageable.

## Tool pattern

1. Call `mcp__vornik__catalog` if you need to discover allowed workflows.
2. Call `mcp__vornik__recall` first for research, review, or project-memory
   questions.
3. Call `mcp__vornik__delegate` with `workflow`, `prompt`, and, when available,
   `repo_scope`.
4. Report the returned `task_id`, workflow, and a concise description of what
   was queued.
5. Use `mcp__vornik__status` or `mcp__vornik__result` only when the user wants
   to poll or pull the result.

Example:

```text
mcp__vornik__delegate(
  workflow="companion-architectural-review",
  prompt="Review the attached diff for shutdown, concurrency, and API contract risks.",
  repo_scope="github.com/grinco/vornik"
)
```

## Files rule

Never hand a file-bearing workflow only a local file path in the prompt. The
vornik agent runs in a container and cannot read Codex's local filesystem.

When a workflow must read local files, read the file bytes locally and pass
them to `mcp__vornik__delegate` as `inputArtifacts`:

```json
{
  "workflow": "companion-architectural-review",
  "prompt": "Review the attached design doc for architectural issues.",
  "repo_scope": "github.com/grinco/vornik",
  "inputArtifacts": [
    {
      "name": "design.md",
      "content": "<base64 file bytes>"
    }
  ]
}
```

For `companion-rag-ingest`, set `skip_auto_extract=true` so the workflow gets
raw staged files and performs the ingest path itself.

If a workflow advertises `require_input_artifacts=true` in `catalog`, a
delegation without `inputArtifacts` is invalid. Stage the bytes first.

## Repo scope

Use a `repo_scope` whenever you can identify the repository. A good scope token
is the canonical git remote, for example:

```text
github.com/grinco/vornik
```

Use `repo_scope="*"` only for cross-repo facts. Omit `repo_scope` only when you
cannot determine the repository or the user explicitly wants project-wide
memory.

Pass the same scope on:

- `mcp__vornik__delegate`
- `mcp__vornik__recall`
- `mcp__vornik__remember`
- `mcp__vornik__recent_memory`
- `mcp__vornik__memory_correct`

## Memory tools

Use `mcp__vornik__remember` only when the user explicitly asks you to remember
something, or when a durable project decision would clearly help future
sessions. Ordinary conversation does not belong in project memory.

Use `mcp__vornik__memory_correct` when a recalled fact is stale or wrong.
Prefer `chunk_ids` from a prior recall when you know the exact stale chunks to
refute. Use claim search when you only have the wrong claim text.

## Scope and budget guardrails

The companion key is bound to one project, optional workflow allowlist, optional
budget cap, and optional memory scopes. The daemon enforces these. If a tool
returns an auth, workflow, memory, or budget error, surface it plainly and give
the operator-side fix, such as minting a new key with `--memory-all` or a wider
`--workflows` list.
