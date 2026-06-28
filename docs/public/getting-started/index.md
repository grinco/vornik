---
sources:
    - path: README.md
      sha256: 44587176fb112be9051a86ca50f4cd4ad539cd6afca1407cf67c6691ba2f4917
---
# Getting Started

This guide takes you from a fresh machine to a running vornik daemon with
your first project, then submits and watches a task run end to end. It
assumes no prior vornik knowledge — if a term is unfamiliar, the
[Concepts](../concepts/index.md) page explains the vocabulary.

## What vornik does

vornik is an asynchronous orchestration daemon. You define **projects**,
each backed by a **swarm** of agents and one or more **workflows**. You
submit **tasks**; vornik queues them, starts the agents in isolated local
containers, runs them through the workflow, and persists the results. Work
runs continuously in the background — you submit and walk away, then come
back for the output.

You can drive it from the CLI, the HTTP API, the Web UI, or a connected
conversation channel (a Telegram bot or email). Around the core,
recent releases add operability and resilience features you will meet later
in the docs — a one-command redacted support bundle, expanded `vornikctl
doctor` checks, centralised log forwarding, structured recovery for failed
steps, and a bounded graceful shutdown — plus security hardening for running
the daemon behind a reverse proxy. None of that is needed to get started; the
defaults are safe and local-first.

## Prerequisites

- **Go 1.25+** — to build the daemon and CLI from source.
- **Podman** (rootless recommended) — vornik runs every agent in its own
  Podman container for isolation. Docker is not used.
- **PostgreSQL** — vornik keeps all durable state (queues, tasks,
  executions, artifacts, memory) in PostgreSQL so work survives restarts.
- **pgvector** — extension for PostgreSQL for vornik RAG
- **Make** — convenience build targets.

!!! note
    vornik is **local-first**: the daemon and all agent containers run on
    the same host. There is no cloud service to sign up for.

## Install

Build the two binaries from source:

```bash
# The daemon
go build -o bin/vornik ./cmd/vornik

# The control CLI
go build -o bin/vornikctl ./cmd/vornikctl
```

Or use the Make target, which produces the same binaries:

```bash
make build
```

Put `bin/` on your `PATH` (or call the binaries by path). The rest of this
guide assumes `vornik` and `vornikctl` are on your `PATH`.

Confirm the build:

```bash
vornik --version
vornikctl version
```

`vornik --version` reports the build you are running; building from the
latest `main` may report a newer version than the most recent tagged
release. See the [Release notes](../release-notes/index.md) for what each
release adds.

## Point vornik at a database

vornik reads its database connection from configuration or environment
variables. The quickest path is environment variables:

```bash
export VORNIK_DATABASE_HOST=localhost
export VORNIK_DATABASE_PORT=5432
export VORNIK_DATABASE_NAME=vornik
export VORNIK_DATABASE_USER=vornik
export VORNIK_DATABASE_PASSWORD=vornik
```

vornik creates and migrates its schema on first start, so an empty
database is all you need. For a full list of settings and the YAML
equivalent, see the [Configuration reference](../reference/configuration.md).

## Start the daemon

```bash
vornik
```

With no flags, vornik looks for configuration in this order: the `--config`
flag, the `VORNIK_CONFIG` environment variable, `./vornik.yaml`, then
`/etc/vornik/vornik.yaml`. To run with an explicit file:

```bash
vornik --config /path/to/vornik.yaml
```

Once it is up, the daemon serves a Web UI and an HTTP API (default
`:8080`). Check it is ready before submitting work:

```bash
curl http://localhost:8080/readyz
# {"status":"ready","checks":[{"name":"database","status":"ok"}]}
```

A `200` from `/readyz` means every dependency the daemon needs is
reachable. A `503` lists which check is failing — see
[Troubleshooting](../troubleshooting/index.md) if you get one.

## Create your first swarm

A project needs a swarm to run on. The fastest way to get one is to
generate it from a built-in preset. The `research` preset (a lead plus a
researcher and a writer) is a good fit for digest, scan, and
summary-style work:

```bash
vornikctl init swarm my-swarm --template research
```

To see every preset and a one-line description of each:

```bash
vornikctl init swarm --list
```

This writes a `SWARM.md` file into your `configs/swarms/` directory and
validates it against the registry before saving.

## Create your first project

Generate a project that uses the swarm you just created and the built-in
`adaptive` workflow:

```bash
vornikctl init project my-project \
  --display-name "My First Project" \
  --swarm my-swarm \
  --workflow adaptive
```

Pass `--dry-run` first if you want to preview the generated YAML without
writing it. The command validates the project against the registry — it
will refuse to create a project that references a swarm or workflow that
does not exist.

!!! note
    You can also create projects without touching the CLI: open the Web UI
    at `/ui/projects/new` for a gallery of templates, or
    `/ui/projects/new/wizard` for a conversational wizard that builds the
    project, swarm, and workflow for you.

Confirm the daemon has loaded your project:

```bash
vornikctl project list
vornikctl project show my-project
```

## Submit your first task

Now submit a task. The `--prompt` flag is the simplest way to describe the
work:

```bash
vornikctl task submit \
  --project my-project \
  --prompt "Summarise the key themes in the attached notes."
```

The command returns a task ID. vornik has queued the task; the scheduler
will pick it up, start the agents, and run them through the project's
workflow.

Useful options:

- `--priority <0-100>` — lower numbers run first; `0` uses the project
  default.
- `--workflow <id>` — run this task on a workflow other than the project
  default.
- `--task-type <label>` — a free-form label that shows up in the task list.
- `--attach <file>` — attach a file as an input; it is snapshotted and
  extracted into project memory so agents can read it.

```bash
vornikctl task submit -p my-project \
  --prompt "Review this report and list open risks." \
  --attach report.pdf \
  --priority 10
```

## Watch it run

List the tasks in your project, optionally filtered by status:

```bash
vornikctl task list --project my-project
vornikctl task list --project my-project --status RUNNING
```

Inspect a single task — its status, workflow, and timestamps:

```bash
vornikctl task get <taskId> --project my-project
```

Follow the live logs while it runs, or read the result excerpt once it has
finished:

```bash
vornikctl task tail <taskId> --project my-project --follow
```

A task moves through `PENDING` → `QUEUED` → `RUNNING` and ends in
`COMPLETED`, `FAILED`, or `CANCELLED`. When it completes, any files the
agents wrote as outputs are persisted as **artifacts** and surfaced through
the API and Web UI.

## Where to go next

- **[Concepts](../concepts/index.md)** — understand projects, swarms,
  workflows, tasks, delegation, and memory before you customise.
- **[Guides](../guides/index.md)** — task-focused how-tos, including
  [workflows and LLM controls](../guides/workflows-and-llm-controls.md) and
  [conversation channels](../guides/conversation-channels.md).
- **[Configuration reference](../reference/configuration.md)** and the
  **[vornikctl reference](../reference/vornikctl.md)** — every setting and
  command.
- **[Troubleshooting](../troubleshooting/index.md)** — when something does
  not start or a task gets stuck.
