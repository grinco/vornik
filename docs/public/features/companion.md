---
sources:
    - path: internal/api/companion_mcp.go
      sha256: 79343c8c6c2bb8dbff8dc79be1e98ee4d7fe6d6fcc5715f6ba394506fe38b105
    - path: contrib/claude-code-companion/.claude-plugin/plugin.json
      sha256: d7ff831aea5ee4a43807c360821d64b3beea78a0f74762220d46b0670f7cddb3
    - path: contrib/codex-companion/.codex-plugin/plugin.json
      sha256: 8c41ebef1d7901cec53c490f40eadd65479780f38759933b1ce960d3c11ccbb2
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

The companion exposes these MCP tools:

| Tool | Purpose |
|------|---------|
| `recall` | semantic search over the project's memory (ranked snippets + provenance) |
| `remember` | deposit a note into the project's memory |
| `recent_memory` | the most recently learned chunks, newest first |
| `list_scopes` | list the repo scopes the project's memory is partitioned into |
| `memory_correct` | soft-refute a wrong or stale memory chunk, optionally storing the correction |
| `whoami` | show this key's project, the repo scope your calls resolve to right now, the database this daemon writes (so a destructive tool can verify its target instead of trusting a name you typed), and this project's embedding readiness — how much of its memory is semantically searchable, plus the embed-queue depth, so a caller can wait for ingest to finish instead of querying a half-indexed corpus |
| `report_problem` | build an anonymized problem report + prefilled issue URL for you to review and submit |
| `delegate` | queue an async task on vornik; returns a task id and a poll hint |
| `status` | check a delegated task's status |
| `result` | fetch a completed task's output inline |
| `cancel` | cancel a task that hasn't finished |
| `list` | list recent companion-created tasks for the project |
| `catalog` | show which workflows this key may delegate to, with cost estimates |
| `skill_propose` | propose a knowledge skill (instructional know-how) as a draft (needs `skill_write`) |
| `skill_search` | find active/trusted knowledge skills by scope, domain, role (needs `skill_read`) |
| `skill_get` | fetch one knowledge skill's full body (needs `skill_read`) |
| `skill_list` | enumerate knowledge skills by maturity for management (needs `skill_read`) |
| `skill_approve` | promote a draft skill to active — the human gate (needs `skill_admin`) |
| `skill_reject` | retire/revoke a knowledge skill (needs `skill_admin`) |
| `skill_set_global` | set/clear a skill's cross-project (global) reach (needs `skill_admin`) |

The `skill_*` tools are the client surface of the daemon-owned **knowledge-skill
store**: instructional know-how authored from any client (a proven procedure, a
hard-won gotcha) that, once approved, is served to swarm roles and every
companion client. They are distinct from the `SWARM-SKILL.md` capability skills
(workflow + roles) and share no storage with project memory. A proposed skill
lands as a `draft` and never fires until an operator with a `skill_admin` key
approves it. A skill can also be marked **global** (`skill_propose global:true`
or `skill_set_global`) so it injects into every project's roles, not just its
home project — the way a procedure captured in your companion project reaches
the autonomy roles. See [Knowledge skills](knowledge-skills.md).

**Proposals are checked for near-duplicates.** Before a skill is written,
`skill_propose` scores it against the whole catalogue — every repo scope, every
maturity including retired, and other projects' global skills. On a hit it
returns `{"blocked": true, "matches": [...]}` and writes nothing. Answer the
block rather than retrying:

- `supersedes: "<id>"` — this replaces that skill. The old one is retired and
  its body kept (still readable by id); it is never overwritten.
- `confirm_distinct: "<why they differ>"` — they are genuinely different. The
  justification is required and is stored on the skill.

You do not need to search first: the preflight already looks wider than
`skill_search`, which filters by repo scope and so hides skills that would be
injected alongside yours anyway. Matching is semantic where an embedding backend
is configured, and falls back to a weaker lexical comparison otherwise — a
preflight can miss, but it never blocks authoring when the embedder is down.

In Claude Code, several tools are also wrapped as slash commands — for example
`/recall`, `/remember`, `/delegate`, `/review` (a one-shot architectural
review), `/peek` (recent tasks), and `/upload` (attach files to a delegation).
In Codex, use the MCP tools directly; the Codex adapter ships a `delegate` skill
that teaches the same recall-before-delegate and file-attachment rules.

## Operator skills

Both companion plugins bundle **five operator skills** that ship enabled by
default and teach your host LLM to drive Vornik's own tooling instead of
improvising. They cross-reference each other, so a session can walk from "where
is this documented" to "set this up" to "is it set up right" to "why is it
broken" to "file it upstream" without you naming the next step.

**`vornik-docs`** — where the documentation lives. It carries the site map and
an order of preference that puts your **installed** CLI's own `--help` first,
then a local checkout, then this site, and the model's own recall last — used to
decide where to look, never as the answer. It exists because the characteristic
failure when answering Vornik questions is a confident, plausible, nonexistent
config key or CLI flag, and that failure is expensive: unknown keys are
frequently ignored, so you get no error and the setting silently does nothing.
The skill also states where the docs deliberately stop, so an honest gap is
reported rather than filled in.

**`configure-vornik`** — configuring a deployment: daemon settings, projects,
swarms, workflows, models, secrets, channels. It leads with the config
hazards that silently no-op an otherwise correct change. The registry tree
holding `projects/`, `swarms/`, and `workflows/` is resolved by a *fallback
chain*, not one environment variable, and `vornikctl`'s chain differs from
the daemon's — so a file can be perfectly valid and still sit in a directory
the daemon never reads. `VORNIK_CONFIGS_DIR` is honoured only when the
directory already contains all three subdirectories; otherwise it is skipped
with no error at all. The skill then pins the apply loop — validate with
`vornikctl doctor`, `vornikctl config reload`, then **confirm** with
`vornikctl config reload-status`, where validation errors actually surface —
and the restart-versus-reload boundary: systemd resolves the daemon's
environment only at start, so unit and env-file edits need a restart.

**`validate-install`** — checking a deployment against the
[reference architecture](../reference/reference-architecture.md): the expected
shape of a healthy install, and where yours diverges. Strictly read-only, which
matters more than it sounds — `POST /api/v1/config/reload` looks like the natural
way to ask whether the registry is valid, and it answers by *applying* the tree,
so an audit could push a half-edited file into service. The skill's discipline is
**configured versus observed**: a config value is a statement of intent, never
evidence of behaviour, so it reads the daemon's resolved state from the boot log
and then the usage ledger that proves the subsystem actually ran. Findings are
split by severity, because the failure mode of a validator is noise — absence of
an optional subsystem is never reported, and neither are your names or model
choices.

**`troubleshoot-vornik`** — diagnosing a deployment that is down, degraded,
or failing tasks, routed by symptom. Daemon down goes to
`vornikctl doctor --offline`, the static escape hatch that needs no running
daemon. Degraded goes to `vornikctl doctor` and `doctor feature`. A failed
task goes to `vornikctl task explain` and then `vornikctl playbook show
<CLASS>` — the failure-class corpus that already carries a written
remediation for the error the executor stamped. The skill also states plainly
which findings `--fix` can repair and which are diagnostic only, so you don't
get sent in a circle.

**`report-problem`** — filing an **anonymized** Vornik problem report (a bug,
a crash, a misbehaving swarm, or an install failure) as a prefilled
`github.com/grinco/vornik` issue. The work is done by the deterministic
`vornikctl report` CLI (rich diagnostics when the daemon is up, `--offline`
static checks when it is down) plus the quickstart installer's own failure URL
for pre-daemon install errors; the skill is the guardrail — review the
anonymized body before submitting, and you file it under your own GitHub
account (nothing is ever posted automatically).

## Project memory and repo scope

`recall` and `remember` operate on the memory of the vornik **project** your key
is bound to. Because one project can back several repositories, every note
carries a **repo scope** — a token derived from the repo you're working in. A
recall returns matches for the current scope plus anything marked
cross-cutting, so two repos served by the same project don't pollute each
other's results. Claude Code resolves the scope from your checkout's git remote
in its SessionStart hook. Codex ships no SessionStart hook, so its `delegate`
skill and plugin prompt instruct the model to derive the same remote token and
pass it explicitly as `repo_scope` on every memory call.

As a backstop for clients without an automatic injector, a companion key can be
minted with a **default repo scope** (`vornikctl companion grant --repo-scope
<token>`). When set, the daemon stamps that scope on any `recall` / `remember` /
`recent_memory` / `delegate` call that omits `repo_scope`, so a forgotten
argument can't silently land a note un-scoped. An explicit per-call `repo_scope`
still overrides the key default — keep passing it on a key reused across repos.

Three scope values are worth distinguishing:

- a **specific token** (`github.com/<org>/<repo>`) — the note is scoped to that
  repo; a recall in a different repo won't see it.
- `"*"` — **cross-cutting**. The note applies across every repo the project
  serves and surfaces in every scoped recall. Use it for project-wide material
  like coding standards, policies, and operator-wide conventions:
  `remember(content="…", repo_scope="*", class="policy")`. It works from any key
  (an explicit `repo_scope` always overrides the key default).
- **NULL** (no scope) — the legacy/uncategorized bucket for notes ingested
  before repo scopes existed. A non-strict recall still surfaces NULL-scoped
  notes across scopes (a migration-grace fallthrough), but `strict_scope=true`
  drops them. Don't deliberately deposit NULL for project-wide notes — use
  `"*"` instead; NULL is meant to be promoted out of via the retag CLI.

`recall` matches the current scope plus `"*"` plus (when `strict_scope` is off)
NULL-scoped notes. `strict_scope=true` narrows to the current scope plus `"*"`
only — handy for spotting NULL-scoped leaks or confirming a scope is actually
populated.

```text
/recall how does the scheduler lease tasks?
/remember the broker API rejects fractional shares with error 10243 — keep shares whole
```

Notes go through vornik's full ingest pipeline (secret scanning, dedup, policy),
and large content should be sent through the ingest workflow rather than a single
`remember` call.

### Digging harder when the fast answer misses

`recall` is tuned for interactive use: one search pass, ranked by fusing semantic
and keyword matches, back in well under a second. When that misses something you
are confident is in there, pass `sufficient` to switch to the slower,
higher-quality retrieval mode:

```text
/recall sufficient=true which design covers the fork-bomb PID limit
```

That mode widens the search when the first pass returns too few strongly-relevant
results, and re-orders candidates with a model rather than by fusion score alone.
It costs an extra model call and takes noticeably longer, which is why it is off by
default — but it is the same mode agents use when assembling context for a task, so
it is the closest thing to "search the way the swarm searches".

### Dating a note

By default a note is filed under the moment you deposited it, and `recall`'s
date filters match on that. When you are recording something that *happened* at
a different time — an incident from last March, a decision taken in Q1, a
meeting note written up a week later — pass `event_time` so date-filtered recall
finds it by when it happened rather than by when you stored it:

```text
/remember event_time=2026-03-14 the ibgateway warm-up outage traced to a stale session cookie
```

It accepts `YYYY-MM-DD` or full RFC3339. Leave it off when you don't know, and
recall falls back to the deposit time exactly as before. A value that isn't a
date is rejected rather than quietly ignored, so a typo can't file the note
under the wrong clock without telling you.

This matters most for bulk ingests: a whole document set deposited in one pass
shares a single deposit timestamp, so without `event_time` a query like "what
changed in July" matches all of it or none of it.

## Delegation

`delegate` hands a job to vornik and returns immediately with a task id and an
estimated time — you keep working while a swarm agent does the task on vornik's
infrastructure. `status` and `result` poll it. Because the work happens on
vornik and `result` returns only the final output artifact, a long review or
audit doesn't consume your editor's token budget. File-bearing workflows must
receive files as `inputArtifacts`; Claude's `/upload` command wraps that flow,
while Codex should call `delegate` directly with base64 `inputArtifacts`. When
the target workflow declares `require_input_artifacts`, the daemon stages your
upload as a raw file rather than extracting it into project memory first — so
the agent reads exactly the bytes you sent, and no client has to opt into that
behaviour.

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

# mint a scoped key (printed once) — memory + knowledge-skill capable
vornikctl companion grant \
    --project companion-$USER \
    --client claude-code \
    --workflows companion-architectural-review,companion-rag-ingest \
    --budget-usd 5 --memory-all --skill-all
```

`--memory-all` and `--skill-all` are the recommended default for a companion
project: they grant RAG (`remember`/`recall`) and the full knowledge-skill
store (`skill_read`/`write`/`admin`) so you can capture and approve procedures
without a second grant. Narrow them per key — e.g. `--skill-read --skill-write`
(propose but not self-approve), or drop `--skill-all` entirely for a
delegate-only key. `--memory-all` = `--memory-read --memory-write`.

Use `--client codex` instead when minting a key for the Codex plugin. For Codex
(no SessionStart scope injector), add `--repo-scope github.com/<org>/<repo>` so
memory calls that omit `repo_scope` inherit the right scope by default.

The key's allowed workflows, spend cap, and memory + skill permissions are
enforced server-side from the key itself — never from the request. The spend cap
counts what the key has spent across the tasks it created, including tasks it
created through the REST API rather than `delegate`; the per-key row on
[`/ui/spend`](../guides/observability.md#spend-per-api-key) is the same
attribution, over a wider set of rows.

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
