---
sources:
    - path: internal/autonomy/manager.go
      sha256: 4304c0b1c0fa689a2ad41febb4d21b0b190b45536ad0d46a4b2fc1db13de37bb
    - path: internal/registry/project.go
      sha256: 3fda8198ac3b13b11f7b85239d2dd7718c7c7662bde56295873112decae05bc3
---
# Autonomy — self-running projects

Most projects run tasks you submit. An **autonomous** project also runs tasks
*it decides to create* — on an interval, toward a standing goal. It's how you
turn vornik from "do this when I ask" into "keep this running and tell me when
something needs me."

Autonomy is **off by default**. A project with autonomy disabled never starts an
evaluation loop, so turning it on is an explicit, per-project decision.

## How it works

When a project has autonomy enabled, the daemon runs one evaluation loop for it.
On each tick it looks at the project's goal and current state and decides whether
to create a task. There are three decision modes:

| `autonomy.mode` | What fires each tick |
|-----------------|----------------------|
| `llm` *(default)* | a lead model is given the goal and project state and decides whether to create a task (and what), or to do nothing |
| `cron` | the goal text is fired verbatim as the task prompt every tick — a deterministic time-driven loop |
| `backlog` | the first unchecked `- [ ]` item from a backlog file is fired, then ticked off |

## Enabling it

Autonomy is configured per project under an `autonomy` block:

```yaml
autonomy:
  enabled: true
  mode: llm
  goal: "Scan the configured feeds and open a digest task when something material lands."
  pollInterval: "30m"        # must carry a unit ("30m", "60s") — a bare number falls back to 5m
  maxTasksPerHour: 4         # 0 = unlimited
  requireApproval: false     # true parks created tasks for operator approval
```

Useful keys:

| Key | Meaning |
|-----|---------|
| `autonomy.enabled` | master switch for the project (default `false`) |
| `autonomy.goal` | the standing objective (required for `llm`/`cron`) |
| `autonomy.mode` | `llm` / `cron` / `backlog` |
| `autonomy.pollInterval` | tick cadence (Go duration; default `5m`) |
| `autonomy.maxTasksPerHour` | per-hour cap on self-created tasks |
| `autonomy.allowedTaskTypes` | restrict what task types autonomy may create |
| `autonomy.requireApproval` | create tasks as awaiting-approval instead of queued |
| `autonomy.duplicateWindow` | how long a completed task suppresses an identical one (default `24h`; `0` for cron-style) |
| `autonomy.workflow_id` | override the workflow a `backlog`- or `cron`-mode tick dispatches into (see [Backlog autonomy and agent deposits](#backlog-autonomy-and-agent-deposits)) |

## Staying in control

Autonomous work runs inside the same guardrails as everything else, and a few
that are specific to it. On each tick, *before* any model cost is incurred, the
loop checks — in order — the per-hour task cap, the shared rate limits, and the
project's [spend caps](cost-and-caching.md#hard-spend-caps) (a hard-cap breach
skips the tick entirely). It also **won't schedule on top of in-flight work**:
if the project already has a queued or running task, the tick is skipped. And it
**deduplicates** — an identical prompt that recently completed (within the
duplicate window) won't be created again, so a steady loop doesn't pile up
copies.

Additional bounds worth knowing:

- **Approval gate.** With `requireApproval: true`, every autonomously created
  task lands in an awaiting-approval state and runs only once you approve it
  (see [Approvals](approvals.md)). Stale approvals are auto-cancelled after a
  configurable window (`autonomy.approval_timeout_hours`, default 96).
- **Tighter tool budget.** When [dynamic tool budgets](cost-and-caching.md#dynamic-per-role-tool-budgets)
  are enabled, unattended autonomous tasks are held to the tighter
  `tool_budget.autonomy_max_factor` ceiling rather than the operator ceiling.
- **Circuit breaker.** A daemon-level breaker
  (`autonomy.circuit_breaker.*`) automatically disables a project's autonomy if
  it sees sustained failures, and alerts you.

To stop autonomy, set `autonomy.enabled: false` (or toggle it from the project
page in the UI) — the loop is cancelled.

## Backlog autonomy and agent deposits

`backlog` mode reads a plain checklist file — `BACKLOG.md` by default
(`autonomy.backlogFilePath` to point at another workspace-relative path) —
and fires the first pending line as a task, ticking it off when the task is
accepted. You can hand-author that file, but agents can feed it too: any role
permitted to call the `backlog_deposit` tool can record an off-scope finding —
a bug, an optimisation, an inefficiency, or a refactor candidate it noticed
while working on something else — without derailing its current task. A
reviewer, for example, can use it for problems it spots in a diff that aren't
in scope for that PR's verdict.

Before each tick reads the file, the workspace is refreshed to the tip of
`origin/main` — so every iteration works against the latest code, picking up
external contributions (merged PRs) since the last run. Your local `- [x]` /
`- [!]` consumption marks are preserved across that refresh, so an item is
never re-run. The refresh is best-effort: if the fetch fails, the tick proceeds
against the current workspace rather than stalling.

### The marker grammar

One consumer owns every read/modify/write of the file, and it recognises four
markers:

| Marker | Meaning | Written by | Picked up by the next tick? |
|---|---|---|---|
| `- [ ]` | Pending, ready to run | You (hand-authored), or you flipping a `- [?]` | Yes — first-in, first-out |
| `- [?]` | Proposed, awaiting your review | `backlog_deposit` — always | No — inert by construction |
| `- [x]` | Done — the task **completed** (raised its PR) | Marked at dispatch, then confirmed by the reconcile pass when the task completes | No |
| `- [!]` | Blocked — the task ended unsuccessfully | The reconcile pass (below) | No — flip to `- [ ]` by hand to retry once you've addressed the cause |

An item is only truly *done* when its task **completed** (raised its PR). Each
tick's reconcile pass checks the dispatched items: a task that ended
**unsuccessfully** — failed, cancelled, or closed after giving up (e.g. an
infinite-rework guard trip) — has its line flipped from `- [x]` to `- [!]`
(blocked). This way the item is neither silently skipped as done nor
auto-retried into a storm: you flip `- [!]` → `- [ ]` by hand to retry once
you've addressed the cause (granted a permission, fixed a flaky test, split a
too-big item), or delete the line if it's not worth retrying. (A task closed
*after* a clean completion keeps its `- [x]` — only a failure indication flips
it.) A task that can't **publish** its change — e.g. the forge rejected the
push — is a separate case: it parks in `AWAITING_INPUT` with a mail-in patch
attached and keeps its `- [x]`, so it waits on you rather than looping.

Agent-authored deposits always land as `- [?]`, never `- [ ]`, and there's no
config knob to change that. A deposit's title and detail are agent-authored
text, and once a `- [ ]` line is consumed it becomes a verbatim task prompt
with no human in between — so a deposit that could land straight into `- [ ]`
would let a confused or compromised agent's own words steer a *future*
autonomous task. The FIFO scan only ever matches `- [ ]`, so a `- [?]` item is
structurally unreachable by autonomy until you edit the box yourself. That's
the per-item approval gate: agent-authored text never becomes an autonomous
prompt without a human decision.

### Granting the deposit tool

```yaml
permissions:
  allowedTools:
    - "backlog_deposit"   # plus the role's usual tools (file_read, run_shell, git_*, ...)
```

The agent calls it with a `kind` (`bug` | `optimisation` | `inefficiency` |
`refactor`), a short `title`, a `detail`, and an optional `evidence` string.
The daemon validates the fields, secret-scans the rendered line (a match is
blocked, never appended), rate-caps accepted deposits per task with
`backlogDeposits.maxPerTask` (default **10**), and dedups against existing
lines — an exact title match or a close paraphrase (fuzzy match) of an open
item is rejected as a duplicate. A close match against an already-closed
(`- [x]` / `- [!]`) item needs `regression: true` plus non-empty `evidence`
to re-open it, and is still refused for 7 days after that item was closed, so
a flapping check can't spin the same "regression" back in immediately. None
of these rejections fail the agent's task — it gets a structured reason back
and moves on:

```yaml
backlogDeposits:
  maxPerTask: 10   # default; per-task cap on accepted deposits
```

### Shipping deposits as draft PRs

`backlog`-mode ticks normally dispatch into the project's default workflow
(commonly `dev-pipeline`, which commits straight to the local clone — no PR,
no review). Point backlog mode at the purpose-built delivery pipeline instead
with `autonomy.workflow_id: "backlog-item"`: it runs the same
analyze → implement (TDD) → test → review loop, then a deterministic
`publish` step pushes the branch and opens a **draft pull request** — no
agent ever runs `git push` or a forge CLI. See
[Backlog-origin pull requests](../features/forge.md#backlog-origin-pull-requests)
for what that PR looks like.

That publish step needs an outbound repo to target — there's no inbound
webhook naming one for backlog-originated work — so `backlog-item` also
requires `github.repo` (and a working GitHub App installation; see
[Forge](../features/forge.md)) on the project. Setting `autonomy.workflow_id`
to a workflow that doesn't exist is loud, not silent: it's flagged at config
load/reload, and the tick itself skips (recorded, not a silent fallback to
the default workflow) rather than guessing.

Putting it together, a project with backlog autonomy pointed at draft PRs
looks like:

```yaml
github:
  app_id: 123456
  installation_id: 78901234
  private_key_path: /etc/vornik/secrets/forge-app.pem
  # Outbound repo for work with no inbound event to name one —
  # backlog-item's draft PRs target this.
  repo: "your-org/your-repo"

autonomy:
  enabled: false            # flip on when you're ready for deposits to become PRs
  mode: "backlog"
  workflow_id: "backlog-item"
  maxTasksPerHour: 1
  pollInterval: "45m"
  allowedTaskTypes:
    - "backlog"

permissions:
  allowedTools:
    - "backlog_deposit"

backlogDeposits:
  maxPerTask: 10
```

With `autonomy.enabled: false`, deposits still flow — any role holding the
tool can propose `- [?]` items — but nothing consumes them yet. Flip
`enabled: true` only once you're ready to have hand-approved (`- [ ]`) items
turn into draft pull requests.

If a `backlog-item` run exhausts its retries or hard-fails a step, it
terminates with no PR opened. The next tick's reconcile pass finds the
terminal-failed item and flips its box from `- [x]` to `- [!]` in the backlog
file, so nothing is silently lost — investigate, then flip it back to
`- [ ]` to retry, or delete the line if it's not worth retrying.

## Watching it

Every evaluation — whether it created a task or not — is recorded with an
outcome (created, no-action, rate-limited, budget-blocked, duplicate, and so
on). Inspect the audit trail and a rollup from the CLI:

```bash
vornikctl autonomy evaluations --project my-project --limit 50
vornikctl autonomy summary     --project my-project --hours 24
```

The project's home page shows a **countdown to the next autonomy tick** and the
last evaluation's outcome, and the dashboard surfaces a next-evaluation tile
across all autonomous projects plus a count of tasks awaiting approval.

> The `vornikctl autonomy` commands are read-only audit views. Enable or disable
> autonomy through the project's configuration or the Web UI — there is no
> `vornikctl autonomy enable` command.
