# Vornik

Vornik is an asynchronous, **local-first** orchestration daemon for teams of AI
agents. You define projects, each backed by a swarm of agents and one or more
workflows; you submit tasks (or let a project create its own); Vornik queues
them, runs each agent in its own isolated container, and persists every result.
Work runs continuously in the background — you submit and walk away, then come
back for the output.

The daemon runs on your own infrastructure. Agents run with **no direct network
egress** by default — the daemon makes the outbound calls — so Vornik suits
data-sensitive and air-gapped environments, including with self-hosted,
open-weight models.

## Start here

- **[Getting Started](getting-started/index.md)** — install the daemon, create
  your first project, and run a task end to end.
- **[Concepts](concepts/index.md)** — projects, swarms, roles, workflows, tasks,
  delegation, and memory — and the [architecture](concepts/architecture.md)
  behind them.

## What it does

- **Runs agent teams asynchronously** — durable queues, isolated per-agent
  containers, and structured [recovery](guides/recovery.md) when a step fails.
- **Runs itself when you want it to** — [autonomy](guides/autonomy.md) lets a
  project pursue a standing goal on a schedule, with budgets, rate limits, and
  [approval gates](guides/approvals.md) keeping it in bounds.
- **Keeps work auditable** — the [Autonomy Black Box](features/blackbox.md) gives
  every task a single replayable trace, and the
  [self-healing genome](features/self-healing.md) proposes and trials workflow
  repairs for operator-gated promotion.
- **Learns from its history** — the [instinct layer](features/instinct.md) mines
  past runs into advisory, confidence-scored patterns.
- **Connects to where people work** — Telegram, Slack, email, and
  [GitHub automation](features/forge.md) via the
  [conversation channels](guides/conversation-channels.md), plus a
  [Claude Code companion](features/companion.md).
- **Governs cost and identity** — hard per-project spend caps and dynamic tool
  budgets ([cost and caching](guides/cost-and-caching.md)), a policy-aware
  [memory firewall](features/memory-rag.md), and
  [authentication with role-based access](features/auth.md).

## Find your way around

- **[Features](features/blackbox.md)** — the capabilities above, and how to
  enable them.
- **[Guides](guides/index.md)** — task-focused how-tos for running Vornik day to
  day.
- **[Reference](reference/index.md)** — every [configuration key](reference/configuration.md)
  and [`vornikctl` command](reference/vornikctl.md).
- **[Troubleshooting](troubleshooting/index.md)** — when something won't start or
  a task gets stuck.
