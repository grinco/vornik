---
sources:
    - path: internal/api/companion_mcp.go
      sha256: 0d9c44d6cdcb3036819a36e97b9212eda3db6da50e2bdebb8518f2f9d0dc4ef7
    - path: contrib/claude-code-companion/.claude-plugin/plugin.json
      sha256: a3c31169e74f59b489ab29c5fb7f7173d19c80927dad6154762583be67e57031
    - path: contrib/codex-companion/.codex-plugin/plugin.json
      sha256: 3a1bda131691e4c40d41aab3d1691956caa3b1b72cf0b358d58e9a52230693a8
---
# Companion plugin

!!! note "Community Edition"

    Included in the free, open-source **Community Edition**. See [Editions](../editions.md).


The vornik **companion** connects your host LLM session to a running vornik
daemon. It ships as a Claude Code plugin and as a Codex plugin; both use the
same companion MCP endpoint and scoped key model. It gives you two things
without leaving your coding session:

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

In Claude Code, several tools are also wrapped as slash commands — for example
`/recall`, `/remember`, `/delegate`, `/review` (a one-shot architectural
review), `/peek` (recent tasks), and `/upload` (attach files to a delegation).
In Codex, use the MCP tools directly; the Codex adapter ships a `delegate` skill
that teaches the same recall-before-delegate and file-attachment rules.

## Project memory and repo scope

`recall` and `remember` operate on the memory of the vornik **project** your key
is bound to. Because one project can back several repositories, every note
carries a **repo scope** — a token derived from the repo you're working in. A
recall returns matches for the current scope plus anything marked
cross-cutting, so two repos served by the same project don't pollute each
other's results. Claude Code resolves the scope from your checkout's git remote
in its SessionStart hook. Codex callers should pass the same remote-derived
token explicitly as `repo_scope`.

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
audit doesn't consume your editor's token budget. File-bearing workflows must
receive files as `inputArtifacts`; Claude's `/upload` command wraps that flow,
while Codex should call `delegate` directly with base64 `inputArtifacts`.

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

Use `--client codex` instead when minting a key for the Codex plugin.

The key's allowed workflows, spend cap, and memory permissions are enforced
server-side from the key itself — never from the request.

On the **client** side, set the companion bearer token and install the plugin:

```bash
export VORNIK_COMPANION_TOKEN="sk-vornik-companion-…"   # the key from `companion grant`
```

For Claude Code, add the plugin from your vornik checkout's companion plugin
directory (`claude --plugin-dir <path>/contrib/claude-code-companion`), or
install it from the bundled plugin marketplace for a persistent setup. Once
loaded, the tools and slash commands are available in the session, and a
start-up digest brings recently-completed delegations and fresh memory back into
context.

For Codex, install `codex-companion` from the bundled Codex marketplace at
`<path>/.agents/plugins/marketplace.json`, or load the plugin directly from
`<path>/contrib/codex-companion`. Do not install the Claude marketplace at
`<path>/.claude-plugin/marketplace.json` into Codex; that package carries the
Claude manifest and will not register the companion MCP server in Codex. The
Codex plugin exposes the same MCP server and a Codex-native `delegate` skill,
but no Claude-only slash commands or SessionStart hook. The bundled Codex MCP
entry targets `http://localhost:8080`; for a remote daemon, override the MCP
entry locally:

```bash
codex mcp add vornik \
  --url <remote>/api/v1/mcp/companion \
  --bearer-token-env-var VORNIK_COMPANION_TOKEN
```

Keep the repo plugin portable: do not put host-specific URLs or shell-style
expressions such as `${VORNIK_URL:-http://localhost:8080}` into the plugin
`.mcp.json`. Codex does not expand that syntax and will fail before the MCP
handshake. If the local override supplies the remote daemon URL, disable only
the plugin-bundled MCP server in `~/.codex/config.toml` while leaving the plugin
itself enabled so its skill remains available:

```toml
[plugins."codex-companion@vornik".mcp_servers.vornik]
enabled = false
```
