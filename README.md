# Vornik

**Vornik** is an asynchronous, local-first orchestration daemon for teams of AI
agents. You define projects — each backed by a swarm of agents and one or more
workflows — submit tasks, and Vornik queues them, runs each agent in its own
isolated container, and persists every result. It runs on your own
infrastructure; agents run with **no direct network egress** by default, so
Vornik suits data-sensitive and air-gapped environments with self-hosted,
open-weight models.

> **Editions.** This repository is **Vornik Community Edition** (AGPL-3.0) — the
> complete orchestration core, fully usable on its own for personal and
> small-team work. A proprietary **Enterprise Edition** adds advanced
> capabilities on the same core. See [Editions](docs/public/editions.md) for the
> feature matrix.

## Quick start

**One command** brings up the full stack — PostgreSQL + pgvector and the Vornik
daemon, in containers via Podman Compose. It installs Podman Compose if needed,
configures the host so the daemon can spawn agent containers, starts everything,
and prints the URL to open when it's ready:

```sh
curl -fsSL https://raw.githubusercontent.com/grinco/vornik/main/deployments/podman/quickstart.sh | bash
```

The daemon creates and migrates its own schema on first boot, so all you supply
is an empty database — which the script provisions for you. When it finishes,
open <http://localhost:8080/ui>; add an LLM key to `deployments/podman/.env` and
re-run `podman compose up -d vornik` to start running tasks. Details and tunables:
[deployments/podman/README.md](deployments/podman/README.md).

> PostgreSQL + pgvector is the recommended backend — the memory/RAG vector
> search needs it. For a quick dependency-free look at the core, the daemon also
> runs on SQLite; see [Getting started](docs/public/getting-started.md).

### From source

```sh
git clone https://github.com/grinco/vornik
cd vornik
go build -o bin/vornik ./cmd/vornik     # the Community daemon
```

Create a `config.yaml` (see [Configuration](docs/public/configuration.md) — keep
secrets in environment variables), then start the daemon and submit a task with
the control CLI (`vornikctl`):

```sh
./bin/vornik                                  # reads ./config.yaml
vornikctl init project my-project --swarm basic-swarm
vornikctl task submit -p my-project --brief "Summarise README.md"
vornikctl task tail   -p my-project <taskId>
```

Full walkthrough: [Getting started](docs/public/getting-started.md).

## Documentation

| Guide | What it covers |
|---|---|
| [Getting started](docs/public/getting-started.md) | Install, first run, your first task |
| [Architecture](docs/public/architecture.md) | Daemon, tasks, leases, executor, workflows, MCP |
| [Configuration](docs/public/configuration.md) | Where config lives + the key reference |
| [CLI reference](docs/public/cli.md) | `vornik` (daemon) and `vornikctl` (control) |
| [Editions](docs/public/editions.md) | Community vs Enterprise feature matrix |
| [Contributing](docs/public/contributing.md) | Dev setup, the CLA, the PR bar |
| [Security](docs/public/security.md) | Supported versions + reporting a vulnerability |
| [Support](docs/public/support.md) | Community help and commercial support |

The full documentation site is published at <https://docs.vornik.io>.

## Requirements

- **Go** — see [`go.mod`](go.mod) for the minimum version
- **Podman** — agents run in isolated containers
- **PostgreSQL with [pgvector](https://github.com/pgvector/pgvector)** — durable
  task and project state; pgvector backs the memory/RAG vector search (the
  `pgvector/pgvector` image ships it). SQLite runs the core but cannot do vector
  search.
- **An LLM provider** — a self-hosted open-weight endpoint, or a cloud API

## Build & test

```sh
make build    # go build ./...
make test     # go test ./...  (integration tests need PostgreSQL)
make lint     # gofmt + go vet
```

## License

[AGPL-3.0](LICENSE) — © Vadim Grinco. Contributions are accepted under a CLA
(see [Contributing](docs/public/contributing.md)).
