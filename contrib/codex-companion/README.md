# codex-companion (Codex plugin)

Async-offload companion for Codex. It connects Codex to the vornik companion
MCP endpoint so long reviews, audits, validations, research, summarization, and
project RAG memory work can run in vornik-managed containers instead of inside
the current Codex turn.

This is the Codex adapter for the same daemon-side contract used by
`contrib/claude-code-companion`. The MCP tool surface is shared; the host-client
packaging is different.

## What you get

- MCP tools from the daemon: `delegate`, `status`, `result`, `cancel`, `list`,
  `catalog`, `whoami`, `recall`, `remember`, `recent_memory`, `list_scopes`,
  `memory_correct`, and `report_problem`.
- A `delegate` skill that teaches Codex when to recall, delegate, attach files
  with `inputArtifacts`, and pull results.
- A `knowledge` skill for the daemon-owned knowledge-skill store.
- The operator lifecycle skills, mirrored from the Claude Code bundle:
  `vornik-docs` (where the documentation lives, with the real site map, and
  `vornikctl --help` ahead of recall so Codex quotes flags that exist),
  `configure-vornik` (find the config tree the daemon actually reads,
  scaffold, then validate → reload → confirm), `validate-install` (check a
  deployment against the published reference architecture, read-only, on
  resolved state rather than configured state), `troubleshoot-vornik`
  (route by symptom through the doctor, the failure-class playbook, and
  task post-mortems), and `report-problem` (file an anonymized issue on
  `github.com/grinco/vornik` that the user submits themselves). They
  cross-reference each other so Codex can walk the lifecycle.
- The same companion bearer-key model as Claude Code, but minted with
  `--client=codex`.

Codex plugins do not currently use the Claude Code plugin manifest shape for
slash commands or SessionStart hooks. This adapter therefore does not ship the
Claude `commands/` or `hooks/` directories. Codex should call MCP tools
directly and, when file bytes are needed, stage them as `inputArtifacts` in the
`delegate` tool call.

## Prerequisites

1. A running vornik daemon. The companion capability flags should be enabled:

   ```bash
   curl ${VORNIK_URL:-http://localhost:8080}/api/v1/capabilities | jq '.features'
   ```

2. A companion project:

   ```bash
   vornikctl init project --template companion companion-$USER
   ```

3. A Codex-scoped companion API key:

   ```bash
   vornikctl companion grant \
       --project=companion-$USER \
       --client=codex \
       --label=$(hostname)/$USER \
       --workflows=companion-architectural-review,companion-test-coverage-audit,companion-doc-review,companion-data-validation,companion-research-gather,companion-report-summarize,companion-rag-ingest \
       --budget-usd=25 \
       --memory-all \
       --repo-scope=github.com/grinco/vornik
   ```

   Copy the printed `sk-vornik-companion-...` secret. It is shown once.

   `--repo-scope` is strongly recommended for Codex. Codex ships no SessionStart
   scope injector (see below), so a model that forgets to pass `repo_scope`
   would otherwise deposit NULL-scoped chunks. With a default scope on the key,
   the daemon stamps it on any memory call that omits `repo_scope`; an explicit
   per-call scope still overrides it. Set it to the canonical git-remote token
   of the repo this key is used in. For a key reused across repos, prefer
   passing `repo_scope` per call (the `delegate` skill explains how to derive
   the canonical token) and treat the key default as a backstop.

## Configure

Set the companion bearer token before starting Codex:

```bash
export VORNIK_COMPANION_TOKEN="sk-vornik-companion-...."
```

The bundled Codex plugin points at
`http://localhost:8080/api/v1/mcp/companion`. Codex requires plugin MCP URLs to
be concrete absolute URLs; shell-style expressions such as
`${VORNIK_URL:-http://localhost:8080}` are not expanded by the plugin loader.
For a remote daemon, add a local MCP override with the URL you need:

```bash
codex mcp add vornik \
  --url https://vornik.internal.example.com:8080/api/v1/mcp/companion \
  --bearer-token-env-var VORNIK_COMPANION_TOKEN
```

`VORNIK_COMPANION_TOKEN` intentionally has no fallback.

## Use

Inside Codex, call the vornik MCP tools directly:

- `mcp__vornik__catalog` to see the workflows this key may use.
- `mcp__vornik__recall` before paying for fresh research or review work.
- `mcp__vornik__delegate` to queue async work.
- `mcp__vornik__status` and `mcp__vornik__result` to pull progress and output.
- `mcp__vornik__remember` and `mcp__vornik__memory_correct` for project memory.

For local files, never put a path in the delegate prompt and assume the agent
can read it. Read and base64-encode the file locally, then pass it in
`delegate.inputArtifacts`:

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

Workflows that declare `require_input_artifacts` (`companion-architectural-review`,
`companion-doc-review`, `companion-rag-ingest`) receive raw staged files: the
daemon forces `skip_auto_extract` for them so upload-time extraction can't
suppress staging. Pass `skip_auto_extract=true` yourself only when staging a raw
file for a workflow that makes no such declaration.

## Validate

From the repo root, using the Codex plugin-creator validator (ships with Codex
under `~/.codex`):

```bash
python3 ~/.codex/skills/.system/plugin-creator/scripts/validate_plugin.py contrib/codex-companion
```
