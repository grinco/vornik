# Guides

Task-focused how-tos for running vornik day to day. Each guide assumes you
already have a project up and running — if you don't, start with
[Getting Started](../getting-started/index.md) and skim the
[Concepts](../concepts/index.md) page first.

## Available guides

- **[Conversation channels](conversation-channels.md)** — connect vornik to
  GitHub, Slack, email, and voice so people can talk to it where they
  already work.
- **[Workflows and LLM controls](workflows-and-llm-controls.md)** — author
  your own workflows and tune the safety, tool-discovery, and reliability
  controls that wrap every model call.
- **[Connect your tools (MCP)](mcp-tools.md)** — declare a project's MCP
  servers, restrict each to an explicit set of tools, throttle expensive
  tools, and inspect it all with `vornikctl mcp`.
- **[Artifacts & outbound file delivery](artifacts-and-delivery.md)** — render
  markdown to PDF/HTML and deliver files (or a task's output artifacts) to
  Telegram and email.
- **[Autonomy](autonomy.md)** — let a project create and run its own tasks on a
  schedule, with caps and approval gates that keep it in bounds.
- **[Cross-project orchestration](cross-project.md)** — have one project's
  workflow call another, spawn new projects on demand, or delegate to a remote
  agent over the A2A protocol.
- **[Approvals & human-in-the-loop](approvals.md)** — put a person in the loop
  with approval steps, approval-gated autonomy, and mid-run checkpoints.
- **[Recovering failed work](recovery.md)** — retry a task, rerun from a step,
  or apply a structured recovery action.
- **[Cost and caching](cost-and-caching.md)** — keep model spend predictable
  with budgets, dynamic tool budgets, caching, and rate limits.
- **[Named secrets](secrets.md)** — inject credentials into a project's agents
  as environment variables, scoped to the projects allowed to use them.
- **[Observability](observability.md)** — the operator dashboards, the spend
  and Insight views, and the Prometheus metrics endpoint.
- **[Storage and retention](storage-and-retention.md)** — understand what
  vornik keeps, for how long, and how to manage its long-term context.
- **[The feature doctor](feature-doctor.md)** — diagnose and safely enable
  vornik's opt-in features.
- **[Evals](evals.md)** — author repeatable swarm test suites with pass/fail
  predicates and run them with `vornikctl eval run`, with a regression diff
  across runs.
- **[Clustering and the DMZ webhook relay](clustering.md)** — scale the worker
  tier or isolate webhook ingress in a DMZ with a mutual-TLS relay.

## How to read these

Every guide documents only the configuration and commands you, as the person
running vornik, actually touch. Configuration lives in your project YAML files;
secrets are always supplied through environment variables rather than written
into config. The `vornikctl` command-line tool is your main interface for
inspecting and validating that configuration — see the
[vornikctl reference](../reference/vornikctl.md) for the full command list and
the [configuration reference](../reference/configuration.md) for every key.
