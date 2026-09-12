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

## Network rule: vornik agents cannot browse

Vornik agents run with the tools their role grants and nothing else.
**Most companion workflows cannot reach the internet.** `mcp__vornik__catalog`
now reports this per workflow as `network_access: "none"` or `"possible"` —
read that field, not the workflow's name, and not the prose in its
description.

A workflow with `network_access: "none"` holds no tool that can fetch a URL.
Handing it one produces a fluent answer built from nothing. On 2026-09-11 a
host delegated "load https://developer.hashicorp.com/terraform/docs into
project RAG" to `companion-research-gather`, reported success, and nothing was
loaded. The daemon now REFUSES a URL-bearing prompt to such a workflow rather
than queueing it.

**To load web docs into RAG there is one path. Use it; it is not a menu.**

1. Fetch the pages yourself, with your own web tools.
2. Write them to local files.
3. Read the bytes locally and call `mcp__vornik__delegate` with
   `workflow="companion-rag-ingest"` and the files as base64
   `inputArtifacts` (see the files rule above).

That path is deterministic and agent-free: 318/318 files and 3439 chunks on
the corpus that produced 40 chunks of summary prose through the wrong
workflow.

If a URL is **incidental** — quoted for context in a diff you want reviewed,
not something that needs fetching — re-send the same delegation with
`acknowledge_workflow_cannot_fetch: true`. Set it for that reason only; it
does not make a fetch happen.

When you are unsure which workflow fits, read `mcp__vornik__catalog` and this
skill before delegating. Do not infer a capability from a workflow's name.

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

If a workflow advertises `require_input_artifacts=true` in `catalog`, a
delegation without `inputArtifacts` is invalid. Stage the bytes first.

You do NOT need to set `skip_auto_extract` for those workflows. The daemon
derives it from `require_input_artifacts` and forces it on, because a workflow
that reads a staged file must not have that file auto-extracted into RAG at
upload time — extraction suppresses staging, and roles whose `allowedTools`
omit the `document_*` MCP tools then get 403 with no readable copy of the
artifact at all (T-8f69, 2026-07-25; earlier as B-10).

Set `skip_auto_extract=true` explicitly only to stage a raw file for a workflow
that does NOT declare `require_input_artifacts`. It is never downgraded.

## Repo scope

Codex ships no SessionStart hook, so — unlike the Claude companion — nothing
auto-injects a repo scope for you. You MUST resolve it yourself and pass it on
every memory call. Without it, deposits land NULL-scoped and pollute other
repos' recalls.

**Resolve the canonical token once per session, then reuse it.** Run:

```bash
git config --get remote.origin.url
```

Normalize the result to a stable `host/path` token (same rules the Claude
companion's hook uses, so scopes don't drift between clients):

- strip a trailing `.git`
- strip the scheme (`https://`, `http://`, `ssh://`)
- strip a leading `<user>@` (e.g. `git@`) that precedes the first `:` or `/`
- replace the first `:` with `/`

Examples (all normalize to the SAME token):

```text
https://github.com/grinco/vornik.git  -> github.com/grinco/vornik
git@github.com:grinco/vornik-enterprise.git      -> github.com/grinco/vornik
```

Do NOT hand-guess or lowercase the token (e.g. `github.com/easeit/vornik-ee`
is WRONG for the repo above) — derive it from the remote so it matches the
canonical scope already in memory.

If there is no `origin` remote, use the repository's top-level directory name
(`basename "$(git rev-parse --show-toplevel)"`). Use `repo_scope="*"` only for
genuine cross-repo facts. Omit `repo_scope` only when you truly cannot
determine the repository and the user wants project-wide memory.

> Operator backstop: a companion key may be minted with a default scope
> (`vornikctl companion grant --client=codex --repo-scope <token>`). When set,
> the daemon stamps that scope on any call you make WITHOUT a `repo_scope`. It
> is a safety net, not a substitute — for a key reused across repos, only the
> token you pass per call is correct. Still resolve and pass it yourself.

Unlike Claude Code, Codex has no SessionStart hook announcing the detected
scope, so if you or the operator ever need to double-check "what RAG
project/scope is this key using right now" — especially when the operator
runs several Codex sessions a day against different repos — call
`mcp__vornik__whoami`. It returns `project_id`, `client_kind`,
`default_repo_scope` (the operator backstop above), and
`effective_repo_scope` (what a call omitting `repo_scope` would resolve to
right now; pass a candidate `repo_scope` arg to preview a different one).

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
