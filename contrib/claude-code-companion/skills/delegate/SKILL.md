---
name: delegate
description: |
  Teaches Claude when to offload work to a vornik companion workflow
  rather than doing it inline. Use this skill when: (a) the user has the
  vornik-companion plugin installed (check by trying mcp__vornik__catalog),
  (b) the task ahead is large, async-friendly, and doesn't need this
  session's context to make progress.
---

# When to reach for vornik

The vornik companion runs work asynchronously in containers managed by
the vornik daemon — separate from this Claude Code session, against
configured models (often OSS / cheap / local). Results don't block this
session; they land on the next one via the SessionStart digest, or you
can pull them mid-session with `/result <task_id>`.

## Standing rule: make architectural review a merge gate

For any **non-trivial code change or design doc** — features, refactors,
security-sensitive edits, concurrency/shutdown logic, LLDs — treat
`companion-architectural-review` as a **gate before you merge or finalize**,
not an optional extra. The pattern that consistently catches real defects
(shutdown races, prompt-injection bypass, concurrency TOCTOU) that pass tests
but would ship bugs:

1. Commit the work on a branch.
2. Upload the **committed diff as a file** (not inline for anything large):
   `git diff <base>...<branch> > /tmp/review.diff`, then
   `/upload companion-architectural-review '<prompt: name the change + what to scrutinize>' /tmp/review.diff`.
   The staged artifact is the source of truth; the workflow will not fall back
   to stale RAG.
3. **Block on the verdict.** Fix every Critical/Important finding, re-review,
   and merge only when it returns clean. Review design docs the same way before
   planning/implementation.
4. **Delegate proactively** — for long reviews/audits/research, fire the
   delegation at the START of the task so it runs while you keep working,
   rather than serializing on it.

**Shell constraint:** single-quote the `/upload` prompt and use **no**
double-quotes or backticks inside it — they break the command's bash wrapper.

This is a default, not a suggestion: shipping a non-trivial change without a
clean companion review is the exception that needs justifying, not the norm.

## The decision

**Recall BEFORE you delegate (LLD 22).** When the user asks for
research, a doc-review, or any "what do we know about X" task, your
first move should be `mcp__vornik__recall` against the topic — not
`delegate`. The swarm may have already answered the same question
last week. Running compute on a question that's already in the RAG
store is the most expensive form of `delegate` you can make.

Rule of thumb: if `recall` returns ≥1 hit with `score ≥ 0.7` that's
fresher than 30 days, surface that result first and ask the user
whether they want a fresh delegation anyway. Only delegate when the
recall comes back empty or with stale/weak hits.

If the key lacks `memory_read`, the tool returns a clean
"this key lacks memory_read" error — surface it verbatim and skip
straight to the offload decision below.

**Offload to vornik when ALL of these are true:**

- The task takes more than ~30 seconds of context-heavy work.
- The user doesn't need the answer in this turn (they can wait for the
  next session, or you can poll).
- The work would consume a lot of your context window if done inline
  (long diffs to review, deep doc audits, structured data validation,
  research gathering, summarization of long inputs).
- The work has a stable shape that maps to one of the shipped
  workflows (call `mcp__vornik__catalog` to see).
- **`recall` came back empty or with stale/weak hits.**

**Do NOT offload when:**

- The user is mid-conversation and needs the answer to continue.
- The task is small (a few seconds of reasoning) — the round-trip to
  vornik would cost more than just doing it.
- The task needs your session-specific context (memory, files you've
  already read, the current conversation) — vornik agents start fresh
  with only the prompt + their workflow's container.
- The task is sensitive in a way the user hasn't explicitly cleared
  for offload (private files, secrets, etc).

## How to invoke

```
mcp__vornik__delegate(
  workflow="companion-architectural-review",  # or any from catalog()
  prompt="Review this diff for cohesion + coupling...",
  task_type="...",  # optional; defaults to workflow id
)
```

The call returns a `task_id` immediately. The user sees the result on
their next session via SessionStart, or they can `/result
<task_id>` mid-session.

The bare `mcp__vornik__delegate` call above is correct ONLY when the
workflow needs no local files — review of an **inline** diff, research,
summarization of text you paste into the prompt. The moment a workflow
must read a **file you have locally** (a design doc to review, sources to
ingest, a dataset to validate), do NOT name its path here — use
`/upload <workflow> "<prompt>" <path…>` so the bytes are staged as
`inputArtifacts`. See the files rule below.

## Six shipped workflows

| Workflow ID | Best for |
|---|---|
| `companion-architectural-review` | A diff/PR (paste it **inline** in the prompt) or a **document/file** (attach via `/upload` — see the files rule below). v1.1.0+ reviews the staged input as its source of truth and will NOT review project memory, so a doc review with no attached file returns "no reviewable content". |
| `companion-test-coverage-audit` | Find untested symbols in touched files |
| `companion-doc-review` | Freshness, clarity, link rot, doc/code drift |
| `companion-data-validation` | Schema + anomaly checks on a dataset |
| `companion-research-gather` | Sourced research on a topic |
| `companion-report-summarize` | Distill long input into an exec brief |
| `companion-rag-ingest` | Async bulk ingest of docs into project RAG. Use when you have > 64 KiB of source content, or a list of files, or a directory — anything too large for the synchronous `remember()` MCP tool. **Files MUST be passed via `inputArtifacts` so the executor stages them into the container at `/app/workspace/artifacts/in/<name>`; the `rag-ingester` role reads exclusively from `context.inputArtifacts`.** Prefer the `/rag-ingest` slash command — it handles file read + base64 + delegate in one shot so file bytes never burn tokens. Pass `payload.tag:` to label outputs. Producer-tagged `rag-ingester` so dashboards can split ingest provenance. |

> **The files rule: NEVER hand a FILE-BEARING workflow a file PATH in
> the prompt — attach the file.** This covers ingest AND document
> review AND data validation — anything where the agent must read a
> file you have locally. A delegate prompt is just text the
> container-bound agent reads; it cannot see your repo or laptop, so a
> named path is never uploaded. The failure modes:
>
> - **Ingest:** silently no-ops — the 2026-06-05 incident reported
>   COMPLETED with "ingestion skipped" while indexing nothing.
> - **Architectural review of a doc:** the agent can't read the path,
>   so it falls back to `recall()` and reviews whatever stale/multiple
>   revisions are in the RAG store — producing contradictory, wrong
>   findings (e.g. flagging a "contradiction" that is two ingested
>   drafts, or "unshipped" for code that shipped after the snapshot).
>   v1.1.0 of the review workflow refuses this and returns "no
>   reviewable content" when nothing is attached.
>
> Always stage the bytes locally instead: `/rag-ingest`
> (files → RAG) or `/upload <workflow> "<prompt>" <path…>`
> (files → ANY workflow, e.g. `companion-architectural-review`). Both
> read the bytes in a local shell and submit them as base64
> `inputArtifacts`, so file content never burns your tokens. Workflows
> that set `require_input_artifacts` (shown in `catalog()`) make the
> daemon REJECT artifact-less delegations up front.

(Run `mcp__vornik__catalog` for the live list — this user's key may
have a narrower allowlist. The catalog response also surfaces
`memory_read` / `memory_write` booleans so you know whether `recall`
and `remember` are usable.)

## Per-repo scope (migration 75)

One operator can have one companion project that serves multiple
repos (e.g. VORNIK + N8N + OpenPlatform). Without scoping, every
recall would mix chunks from every repo — and VORNIK architecture
chunks would show up when you ask about N8N.

The plugin's SessionStart hook detects the current repo from
`git config --get remote.origin.url` (or the repo basename when no
origin), emits the directive at session start, and you should
follow it: pass `repo_scope: "<detected>"` on every:

- `mcp__vornik__recall`
- `mcp__vornik__remember`
- `mcp__vornik__recent_memory`
- `mcp__vornik__delegate`

When the operator's instruction genuinely spans repos (e.g. "save
this learning about my toolchain"), pass `repo_scope: "*"` —
the daemon stores `*` on the chunk and it surfaces under every
scoped recall. The bare deposit without `repo_scope` is the
"explicit project-wide" opt-out and is rare; default to
the detected scope.

If no SessionStart directive was emitted (the operator opened
Claude outside a git repo, or the daemon was offline at session
start), call without `repo_scope` — the daemon falls back to
project-wide behaviour, same as pre-migration-75 sessions.

## Two adjacent tools — `recall` and `remember`

Beyond `delegate`, the companion server exposes two stateful tools
(LLD 22) that close the loop with project memory:

- **`mcp__vornik__recall`** — semantic search over the project's RAG.
  Cheap (~$0.0001/call), ~100-400 ms. Use it routinely; see the
  "recall before delegate" rule above.
- **`mcp__vornik__remember`** — deposit a note from this session
  into the RAG store. Use sparingly — only when the user explicitly
  asks you to remember something, OR you've reached a non-obvious
  decision the user would benefit from finding next week. Random
  conversation chatter does NOT belong in long-term project memory.

Companion-deposited content lands as class `companion_note` (30-day
TTL, low default confidence). It's never a system-of-record; it's
ambient context for future sessions. Operators can elevate it to a
higher-confidence class via the operator surface if it earns it.

## Scope guardrails

The user's companion key carries:
- A bound project — you can only see / act on tasks in that project.
- An `allowedWorkflows` list — `delegate` rejects anything outside it.
- A `budgetCapUSD` — once hit, future `delegate` calls fail.

You don't need to enforce these yourself; the MCP server does. But when
a `delegate` call fails with one of those errors, surface the message
to the user verbatim and suggest the corresponding fix (a wider grant,
a fresh key, etc).
