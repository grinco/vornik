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

### AI-assisted install (recommended)

If you use a coding agent (Claude Code, Codex, Gemini CLI, …), let it do the
whole thing — paste this:

> Set up Vornik for me. Follow the runbook at https://agents.vornik.io

[AGENTS.md](https://github.com/grinco/vornik/blob/main/AGENTS.md) is an
agent-executable runbook — every step is a command plus a verifiable check.
The agent installs the stack (via the same `get.vornik.io` script as below),
connects your LLM key, runs a hello-world task, and wires itself in with a
companion project and persistent RAG memory, asking you before anything
privileged.

### One-command quickstart

One command works on **both Linux and macOS** — it detects your OS and runs the
right installer, so you (or your agent) never pick a variant:

```sh
curl -fsSL https://get.vornik.io | bash
```

On **Linux** it installs any missing prerequisites, builds the daemon + CLI in
an ephemeral container (no Go toolchain needed), starts PostgreSQL + pgvector in
a container, and runs the daemon as a rootless `systemctl --user` service. On
**macOS** it provisions a small Linux VM and runs that same stack inside it (see
[macOS](#macos-inside-a-linux-vm) below for why and how). The script detects the
OS itself, so the command is identical either way.

This pins the **release tag baked into the script** (not a moving branch); set
`VORNIK_REF=main` for bleeding-edge. To verify the script against its published
checksum before running it (catches transit/redirect tampering — not a
signature), fetch both first:

```sh
REF=<release>  # a tag from github.com/grinco/vornik/releases that ships quickstart.sh.sha256
base="https://raw.githubusercontent.com/grinco/vornik/$REF/deployments/podman"
curl -fsSLO "$base/quickstart.sh" && curl -fsSLO "$base/quickstart.sh.sha256"
sha256sum -c quickstart.sh.sha256 && VORNIK_REF="$REF" bash quickstart.sh
```

When it finishes, open <http://localhost:8080/ui> — a first-run **setup
guide** walks you through connecting an LLM endpoint and key, optional
memory/RAG, and creating your first project. Details and tunables:
[deployments/podman/README.md](https://github.com/grinco/vornik/tree/main/deployments/podman).

### macOS (inside a Linux VM)

macOS has no native container runtime, and Vornik's zero-egress agent isolation
depends on a Linux-only mechanism (a network-less container reaching the daemon
over a bind-mounted unix socket). To preserve that isolation **exactly** as on
Linux, the macOS installer provisions a lightweight Linux VM
([Lima](https://lima-vm.io)) and runs the same stack inside it. The one-liner
above auto-detects macOS and does this for you — the **same command**, no macOS
variant to remember:

```sh
curl -fsSL https://get.vornik.io | bash
```

It installs Lima (via Homebrew), creates a `vornik` VM (Apple-Silicon-first;
Intel is best-effort), runs the standard install inside it, and forwards the
UI/API to your Mac at <http://localhost:8080/ui>. Manage it with the installed
`vornikctl` shim, which runs against the VM:

```sh
vornikctl status          # daemon health inside the VM
vornikctl logs            # daemon logs
vornikctl backup out.tar  # snapshot (DB via pg_dump + config) to a Mac path
vornikctl delete --force  # destroy the VM — run backup first; this erases all data
```

Notes: your data (including PostgreSQL) lives **inside the VM** for durability;
persist across a VM rebuild with `vornikctl backup`/`restore`. Only
`~/.config/vornik` is shared from the Mac (editable; run `vornikctl` reload after
editing). The UI port-forward means any local Mac process can reach the daemon
API — fine for a single-operator install.

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
