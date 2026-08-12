# vornik-companion (Claude Code plugin)

Async-offload companion for Claude Code. Delegate reviews, audits, validations,
research, and summarization to vornik-managed agents; results surface back via
a SessionStart hook so the loop closes without you context-switching.

Designed against the vornik companion contract documented in
`https://docs.vornik.io`. The same MCP tool
surface is client-generic, so a future Codex / opencode / Gemini-CLI adapter
plugs in without any vornik-side change.

## What you get

- **20 MCP tools** — the task surface `delegate`, `status`, `result`, `cancel`,
  `list`, `catalog`, `whoami`, `report_problem`; the LLD-22 RAG set `recall`, `remember`,
  `recent_memory`, `list_scopes`, `memory_correct`; and the knowledge-skill set
  `skill_propose`, `skill_search`, `skill_get`, `skill_list`, `skill_approve`,
  `skill_reject`, `skill_set_global`.
- **18 slash commands** — every tool above that is worth a one-liner, exposed
  under the plugin namespace: `/vornik-companion:delegate`,
  `:peek`, `:status`, `:result`, `:review`, `:whoami`, `:recall <query>`,
  `:remember <note>`, `:upload`, `:rag-ingest`, `:memory-correct`, and the
  `:skill-*` set. Run `/help` for the current list — it is authoritative.
- **SessionStart hook** — pulls every task that completed since your
  last session ended **and** the most recently-touched RAG entries in
  the project. The host LLM opens each session knowing what the swarm
  finished AND what the project just learned, without having to ask.
- **`recall_hint` on every `delegate`** — when the key carries
  `memory_read`, the daemon runs a bounded semantic-neighbour check
  on the delegate prompt and attaches the strongest hits to the
  response. The host LLM surfaces "vornik already knows X — still
  delegate?" before swarm compute starts.
- **Six skills:**
  - `delegate` — when to reach for the companion rather than spending
    Claude's own tokens on a long task, and the "recall before delegate"
    rule that keeps memory-redundant work off the swarm.
  - `vornik-docs` — where the documentation lives, with the real site map
    and the rule that the installed CLI's own `--help` outranks recall.
    Guards against the characteristic failure: a confident, plausible,
    nonexistent config key.
  - `configure-vornik` — configuring a deployment: finding the config
    tree the daemon actually reads, scaffolding projects and swarms, and
    the validate → reload → confirm loop.
  - `validate-install` — checking a deployment against the published
    reference architecture: strictly read-only, resolved state over
    configured state, and a severity split that keeps absent optional
    subsystems out of the report.
  - `troubleshoot-vornik` — diagnosing a deployment that is down,
    degraded, or failing tasks, routing by symptom through the doctor,
    the failure-class playbook, and task post-mortems.
  - `report-problem` — filing an anonymized bug, crash, or install
    failure as a prefilled `github.com/grinco/vornik` issue the user
    reviews and submits themselves.

  The last four form the operator lifecycle and cross-reference each
  other: validate → configure to fix a divergence, configure →
  troubleshoot when a change doesn't take, troubleshoot → report when the
  diagnostic ladder bottoms out. `validate-install` starts from the
  reference shape and looks for divergence; `troubleshoot-vornik` starts
  from a symptom and works toward a cause.

## Prerequisites

1. A running vornik daemon. The `companion-v1` and `companion-mcp` capability
   flags must both be `true`:
   ```
   curl $VORNIK_URL/api/v1/capabilities | jq '.features'
   ```
2. A companion project. The shipped template gets you there in one command
   — note `vornikctl init project` takes the project ID positionally;
   the `--param` flag is for additional template parameters beyond the ID:
   ```
   vornikctl init project --template companion companion-$USER
   ```
   The template materialises into your deployed configs tree. Post-2026-05-27
   `make install` ships the companion-* workflows alongside the template, so
   a fresh install resolves cleanly. If your install pre-dates that, copy
   `configs/workflows/companion-*.md` from the vornik repo into your
   deployed configs/workflows/ dir manually.
3. A companion-scoped API key (the bearer Claude Code will use):
   ```
   vornikctl companion grant \
       --project=companion-$USER \
       --client=claude-code \
       --label=$(hostname)/$USER \
       --workflows=companion-architectural-review,companion-test-coverage-audit,companion-doc-review,companion-data-validation,companion-research-gather,companion-report-summarize,companion-rag-ingest \
       --budget-usd=25 \
       --memory-all
   ```
   Copy the printed `sk-vornik-companion-<user>....` secret — it's shown
   once.

   `--memory-all` grants both `recall` and `remember` (LLD 22).
   Use `--memory-read` alone if you only want the host LLM to query
   the RAG store, not deposit into it. Omitting both keeps the key
   delegate-only (the pre-LLD-22 default).

## Install

Two paths. Use `--plugin-dir` for quick local-dev (no marketplace, just point
Claude at the directory); skip ahead to the marketplace section once you want
the plugin to persist across sessions without a flag.

### Local-dev: `--plugin-dir`

```bash
# Set the environment variables the plugin's .mcp.json references.
# Put these in your shell profile (~/.zshrc, ~/.bashrc) so every Claude Code
# session inherits them.
export VORNIK_URL="http://localhost:8080"
export VORNIK_COMPANION_TOKEN="sk-vornik-companion-example.xxxx"   # captured above

# Point Claude Code at this plugin directory for the session.
claude --plugin-dir /path/to/vornik/contrib/claude-code-companion
```

Inside the session, run `/help` and verify the plugin's skills show up under
the `vornik-companion:` namespace.

### Persistent install: via the vornik marketplace

For a permanent install (no `--plugin-dir` flag every time), use the
marketplace shipped at `.claude-plugin/marketplace.json` in the vornik
repo root. Inside Claude Code:

```
> /plugin marketplace add /opt/vornik/vornik
> /plugin install vornik-companion@vornik
```

After that every future Claude session sees the plugin without any
flag. Refresh after a `git pull` of the vornik repo with:

```
> /plugin marketplace update vornik
```

The marketplace and the plugin's `.claude-plugin/plugin.json` both
lint clean under the official validator:

```
$ claude plugin validate /opt/vornik/vornik
$ claude plugin validate /opt/vornik/vornik/contrib/claude-code-companion
```

## MCP wiring

The plugin's `.mcp.json` references two environment variables:

```json
{
  "mcpServers": {
    "vornik": {
      "type": "http",
      "url": "${VORNIK_URL:-http://localhost:8080}/api/v1/mcp/companion",
      "headers": {
        "Authorization": "Bearer ${VORNIK_COMPANION_TOKEN}"
      }
    }
  }
}
```

`VORNIK_URL` falls back to `http://localhost:8080` when unset.
`VORNIK_COMPANION_TOKEN` has no fallback — Claude Code will report a missing-
env-var error if you forgot to export it. Rotate the token by re-exporting
and starting a fresh session.

## Try it

In a Claude Code session, with the plugin loaded:

```
> /vornik-companion:delegate companion-architectural-review "Review this diff..."
```

Claude calls the `delegate` MCP tool; you get a `task_id` back immediately.
On your **next session** the SessionStart hook will list any tasks that
completed in between and inject a digest. You can also pull a result
mid-session:

```
> /vornik-companion:result <task_id>
```

Or list everything outstanding:

```
> /vornik-companion:peek
```

Running several sessions a day against different projects/repos? Check
which RAG project/scope the current session is using:

```
> /vornik-companion:whoami
```

## Workflows shipped on the vornik side

The `companion` project template wires seven workflows out of the box. Each
is a one-step delegation that drops `artifacts/out/<file>.md` for your
inspection. The exact prompts live under
`configs/workflows/companion-*.md` in the vornik repo — edit those, not
this plugin, when you want to tune behaviour.

| Workflow | Use for |
|---|---|
| `companion-architectural-review` | Second-opinion on a diff or PR |
| `companion-test-coverage-audit` | Gap analysis on touched files |
| `companion-doc-review` | Freshness + clarity + link-rot |
| `companion-data-validation` | Schema + anomaly checks on a dataset |
| `companion-research-gather` | Sourced research on a topic |
| `companion-report-summarize` | Distill long input into an exec brief |
| `companion-rag-ingest` | **Async bulk ingest of documents into project RAG memory.** The host LLM uploads files via `inputArtifacts` (or the `/rag-ingest` slash command — which is the recommended entry point); the executor stages each file into the agent container at `/app/workspace/artifacts/in/<name>`. The `rag-ingester` role reads its source list from `context.inputArtifacts`, copies each entry to `artifacts/out/`, and the executor's `ingestOutputArtifacts` hook commits them through the full `IngestText` pipeline (gate stack, embedding, classifier). Use this for batches larger than the 64 KiB cap on the synchronous `remember()` MCP tool, or anywhere you want operator-controlled provenance (`producer_role=rag-ingester`, classifier-assigned `content_class`). Backed by the `companion-ingest` swarm preset (`vornikctl init swarm <name> --template companion-ingest`) which adds the `rag-ingester` role to the standard reviewer / analyst / summarizer trio. |

## Troubleshooting

**Plugin doesn't show up after `claude --plugin-dir <path>`**
Check that the manifest lives at `<path>/.claude-plugin/plugin.json` (not at
`<path>/plugin.json` directly). Run `claude --plugin-dir <path>` again with
`/help` and look for entries under the `vornik-companion:` namespace. If the
namespace is missing, the manifest path is wrong.

**`/plugin` shows it disabled or missing after rsync into `~/.claude/plugins/`**
Claude Code doesn't scan `~/.claude/plugins/` for bare directories — it
discovers plugins via marketplaces. Either use `--plugin-dir` for this
session, or set up a local marketplace.

**Capabilities endpoint shows `companion-mcp: false`**
The daemon is missing one of `apiKeyRepo`, `taskRepo`, `taskCreator`, or
`projectRegistry`. That usually means the daemon is running in test mode
or a partial wiring. Check the daemon startup logs.

**`bearer token is not a companion-scoped key`**
Your key was minted via `vornikctl key create` rather than `vornikctl
companion grant`. The companion MCP server only accepts keys with a
non-NULL `client_kind` column.

**`workflow X not in this key's allowedWorkflows`**
Add the workflow to the grant or mint a new key with a broader allowlist.
`vornikctl companion grant ... --workflows=workflow-1,workflow-2,...`.

**Tasks don't appear in `/vornik-companion:peek`**
By design, `list()` only surfaces tasks created via the companion MCP
(i.e. `creation_source = COMPANION`) and within the last 14 days. Use
`/vornik-companion:status <id>` to look up an older task by ID.

## What's deliberately NOT in this plugin (yet)

- **Pre-write validation hook** — opt-in shadow `delegate()` fired by
  `PreToolUse(Write|Edit)`. Phase-2 work.
- **Reverse-direction A2A** — letting vornik workflow steps call back
  into Claude Code as a remote agent. Phase-3 work.
- **Cost-aware autosizing** — automatic model-tier selection per
  workflow based on accept/reject signals. Phase-4 work.

See `https://docs.vornik.io` in the
vornik repo for the full roadmap.
