---
sources:
    - path: internal/api/companion_mcp.go
      sha256: ae1b233c14ccce621d41b884874776930f401c20cccd0c3330119ea119043642
    - path: contrib/claude-code-companion/.claude-plugin/plugin.json
      sha256: a3c31169e74f59b489ab29c5fb7f7173d19c80927dad6154762583be67e57031
---
# Companion plugin

!!! note "Community Edition"

    Included in the free, open-source **Community Edition**. See [Editions](../editions.md).


The vornik **companion** is a Claude Code plugin that connects your editor
session to a running vornik daemon. It gives you two things without leaving
Claude Code:

- **Project memory** — semantically recall what vornik knows about the repo
  you're in, and deposit notes back into that memory.
- **Async delegation** — hand long-running work (reviews, audits, research,
  bulk ingestion) to vornik's agents, then poll for the result — so the heavy
  lifting runs on vornik's compute instead of burning your editor's context.

The plugin talks to the daemon over MCP-over-HTTP and is gated by its own
scoped key, so it never needs your admin credentials.

## The tools

The companion exposes ten MCP tools:

| Tool | Purpose |
|------|---------|
| `recall` | semantic search over the project's memory (ranked snippets + provenance) |
| `remember` | deposit a note into the project's memory |
| `recent_memory` | the most recently learned chunks, newest first |
| `list_scopes` | list the repo scopes the project's memory is partitioned into |
| `delegate` | queue an async task on vornik; returns a task id and a poll hint |
| `status` | check a delegated task's status |
| `result` | fetch a completed task's output inline |
| `cancel` | cancel a task that hasn't finished |
| `list` | list recent companion-created tasks for the project |
| `catalog` | show which workflows this key may delegate to, with cost estimates |

Several are also wrapped as slash commands — for example `/recall`, `/remember`,
`/delegate`, `/review` (a one-shot architectural review), `/peek` (recent
tasks), and `/upload` (attach files to a delegation).

## Project memory and repo scope

`recall` and `remember` operate on the memory of the vornik **project** your key
is bound to. Because one project can back several repositories, every note
carries a **repo scope** — a token derived from the repo you're working in. A
recall returns matches for the current scope plus anything marked
cross-cutting, so two repos served by the same project don't pollute each
other's results. The session start-up resolves the scope from your checkout's
git remote automatically and passes it on every call.

```text
/recall how does the scheduler lease tasks?
/remember the broker API rejects fractional shares with error 10243 — keep shares whole
```

Notes go through vornik's full ingest pipeline (secret scanning, dedup, policy),
and large content should be sent through the ingest workflow rather than a single
`remember` call.

## Delegation

`delegate` hands a job to vornik and returns immediately with a task id and an
estimated time — you keep working while a swarm agent does the task on vornik's
infrastructure. `status` and `result` poll it. Because the work happens on
vornik and `result` returns only the final output artifact, a long review or
audit doesn't consume your editor's token budget. File-bearing commands like
`/upload` attach files as artifacts so their bytes never enter the model's
context.

The shipped delegation workflows include:

- **architectural review** — a second opinion on a diff, PR, or design doc
  (handy as a pre-merge gate),
- **test-coverage audit**, **doc review**, **data validation**,
- **research gather** and **report summarize**, and
- **RAG ingest** — bulk-load files into the project's memory.

`catalog` lists exactly which of these your key is allowed to run. When you're
about to delegate something vornik may already know, `delegate` can surface a
hint from memory first, so you don't spend compute re-deriving it.

## Setting it up

On the **daemon** side, an operator prepares a companion project and mints a
**companion-scoped key** (a plain API key won't be accepted on the companion
endpoint):

```bash
# confirm the daemon advertises the companion capabilities
curl "$VORNIK_URL/api/v1/capabilities"

# mint a scoped key (printed once)
vornikctl companion grant \
    --project companion-$USER \
    --client claude-code \
    --workflows companion-architectural-review,companion-rag-ingest \
    --budget-usd 5 --memory-read --memory-write
```

The key's allowed workflows, spend cap, and memory permissions are enforced
server-side from the key itself — never from the request.

On the **client** side, point the plugin at your daemon with two environment
variables and install it:

```bash
export VORNIK_URL="https://vornik.internal.example.com:8080"
export VORNIK_COMPANION_TOKEN="sk-vornik-companion-…"   # the key from `companion grant`
```

Add the companion plugin from your vornik checkout's companion plugin directory
(`claude --plugin-dir <path>/contrib/claude-code-companion`), or install it from
the bundled plugin marketplace for a persistent setup. Once loaded, the tools
and slash commands are available in the session, and a start-up digest brings
recently-completed delegations and fresh memory back into context.
