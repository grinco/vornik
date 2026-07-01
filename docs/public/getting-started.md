# Getting started

This guide gets a local **Vornik Community** daemon running and walks through your
first task.

## Requirements

- **Go** (see [`go.mod`](https://github.com/grinco/vornik/blob/main/go.mod) for the minimum version) — to build from source.
- **A container runtime** (Podman) on the host — Vornik runs each agent in its
  own isolated container.
- **An LLM provider** reachable from the daemon — a self-hosted, open-weight model
  endpoint, or a cloud API key. Agents themselves have no network egress; the
  daemon makes the outbound model calls.

## Install

### One-command quickstart (recommended)

On a Linux host, one command installs any missing prerequisites, builds the
daemon + CLI in an ephemeral container (no Go toolchain needed), starts
PostgreSQL + pgvector in a container, and runs the daemon as a rootless
`systemctl --user` service:

```sh
curl -fsSL https://get.vornik.io | bash
```

When it finishes, open <http://localhost:8080/ui> — a first-run **setup
guide** walks you through connecting an LLM endpoint and key, optional
memory/RAG, and creating your first project. Details and tunables:
[deployments/podman/README.md](https://github.com/grinco/vornik/tree/main/deployments/podman).

### Build from source

```sh
git clone https://github.com/grinco/vornik
cd vornik
go build -o bin/vornik ./cmd/vornik
```

This produces the Community daemon binary `vornik`. The control CLI is
`vornikctl` (see the [CLI reference](cli.md)).

### Release binary / container image

> **Before public release:** download links for the release binary and container
> image (with checksums and a build attestation) land here once the public
> release pipeline is in place. Until then, build from source as above.

## First run

Vornik searches for its config file, in order:

1. the `--config` flag,
2. the path in the `$VORNIK_CONFIG` environment variable,
3. `./vornik.yaml` (or `./config.yaml`) in the working directory,
4. an XDG/home location (`$XDG_CONFIG_HOME/vornik/` or `~/.config/vornik/`),
5. `/etc/vornik/vornik.yaml` (or `/etc/vornik/config.yaml`).

Create a minimal `config.yaml` — keep secrets out of it and point at provider
credentials via environment variables (see [Configuration](configuration.md)) —
then start the daemon:

```sh
vornik              # reads ./config.yaml and starts the orchestration loop
vornik --version    # prints the build version and edition (Community)
```

The daemon reloads its config on `SIGHUP` (`kill -HUP <pid>`, or
`systemctl --user reload vornik`) — no restart needed for most changes.

## Your first task

With the daemon running, use `vornikctl` to scaffold a project and submit a task.
The flags below are the common ones; the [CLI reference](reference/vornikctl.md) is
the full, generated source of truth.

```sh
# Create a project (a swarm + workflow) under ./configs
vornikctl init project my-project --swarm basic-swarm

# Submit a task to it, then watch it run
vornikctl task submit -p my-project --prompt "Summarise README.md"
vornikctl task list   -p my-project
vornikctl task tail   -p my-project <taskId>
vornikctl task get    -p my-project <taskId>
```

Vornik queues the task, leases it to a worker, runs the agent in an isolated
container, and persists the result — submit and walk away, then read it back with
`task get`.

Stuck? Run `vornikctl doctor` to check the daemon's health and feature readiness.

## Next steps

- [Configuration](configuration.md) — where config lives and how to tune the daemon.
- [Architecture](architecture.md) — tasks, leases, the executor, and workflows.
- [Editions](editions.md) — what Community includes and what Enterprise adds.
