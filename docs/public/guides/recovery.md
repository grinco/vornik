---
sources:
    - path: internal/executor/retry_from_step.go
      sha256: 9f854e8b0dc2f79344096c064a515a0a3d8d4d1a278f2b4ded54be023373a0a0
    - path: internal/ui/execution_actions.go
      sha256: 094d12d4180afe3a28dc8befe72fff9cc03fe1d19b671f86c00c294298346886
---
# Recovering failed work

When a task fails, you rarely want to start over from scratch. vornik gives you
graded ways to recover — re-run the whole task, re-run from a specific step, or
apply a structured fix and retry — plus a plain-language explanation of what went
wrong to decide between them.

## Understand it first

Before retrying, get an explanation of the failure:

```bash
vornikctl task explain <taskId> --project my-project
```

This produces an operator-friendly summary of what happened — the entry point
from a failure to a recovery decision. For the complete event-by-event record
behind it, see the [Autonomy Black Box](../features/blackbox.md).

## Retry the whole task

The simplest recovery re-queues the task and runs it again from the first step:

```bash
vornikctl task retry <taskId> --project my-project
# start its attempt counter over as well:
vornikctl task retry <taskId> --project my-project --reset-attempts
```

A fresh execution runs the workflow from the top. Use this when the failure was
transient (a flaky upstream, a rate limit) or when nothing about the run is worth
keeping.

## Re-run from a step

When the early steps were fine and only a later step went wrong, you can **rerun
from that step** instead of repeating the good work. On a failed (terminal)
execution's detail page in the Web UI, choose a completed step and **Rerun from
step**: vornik rewinds the execution to that point — keeping the upstream steps'
results and artifacts — and runs forward again.

Two things to know:

- It **rewinds the same execution** rather than cloning a new one, so the
  execution's URL and audit history are preserved in place.
- vornik warns you when an upstream step had **external side effects** (for
  example, a posted PR review, content indexed into memory, or a spawned
  cross-project task). Those effects are *not* replayed, so the upstream world
  may already reflect the first run — rerun from a step that's safe to treat as
  the new starting point.

Rerun-from-step is a Web UI action; the CLI's `vornikctl execution` command is for
inspecting executions (`list`, `inspect`), not rerunning them.

## Structured recovery

When a step fails into recovery, the lead can offer **structured recovery
options** rather than just retrying blindly. You'll see these as choices on the
failed task — and approving one makes vornik apply it before retrying:

| Action | What it does |
|--------|--------------|
| `retry` | retry the failed step as-is |
| `skip` | skip the failed step and continue |
| `model_fallback` | retry with each role switched to its configured fallback model |
| `reroute_workflow` | re-run on a different workflow (bounded by the project's allowed candidates) |

These are **operator-gated** — vornik proposes, you choose. A malformed or
unrecognised action safely degrades to a plain retry hint rather than doing
something unexpected. The failed-task page also offers a per-failure-class
action card (for example, "requeue" for a rate-limited task, "review project
budget" for a budget failure, "inspect audit trail" for a suspected leak) plus a
universal **steer + retry** that lets you add a correcting instruction before the
next attempt.

## Choosing an approach

- Transient failure, nothing to keep → **retry the whole task**.
- Good work early, one late step wrong → **rerun from that step**.
- A model misbehaved or the workflow was a poor fit → **structured recovery**
  (`model_fallback` / `reroute_workflow`).
- Not sure → run `vornikctl task explain` first.
