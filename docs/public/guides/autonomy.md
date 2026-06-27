---
sources:
    - path: internal/autonomy/manager.go
      sha256: c2a86b17f348cde83afd63fc97f2512aad0ccc8180aa7435dc011fa696b831e8
    - path: internal/registry/project.go
      sha256: a6b13685371ebdecfddfa9870a8ae7fc37653f83374519c7d9cb9dc6ec0aeb82
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
