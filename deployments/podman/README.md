# vornik Demo Fleet — host daemon + podman deps

One-command bringup on a single Linux host for demos and local testing. The
**daemon runs on the host** as a rootless `systemctl --user` service; only
the stateful/auxiliary services run in containers:

| Where | What | Purpose |
|---|---|---|
| host user service | `vornik` (`~/.local/bin/vornik`) | Daemon + UI + API |
| container `vornik-postgres` | `pgvector/pgvector:pg16` | Database (with vector extension) |
| container `vornik-scraper` *(Enterprise)* | `localhost/vornik-scraper:latest` | Headless-browser MCP server |

Agent containers are spawned on-demand by the daemon via your rootless
podman — they are siblings of the deps containers and share the host
filesystem with the daemon (see [Runtime model](#runtime-model)).

> **Why host, not container?** A daemon-in-a-container that drives the host
> podman hits the Docker-out-of-Docker bind-mount trap: it asks the host
> podman to `podman run -v <path>:…`, but `<path>` (its exec scratch /
> workspaces) exists only inside the daemon container, so the host can't
> `statfs` it. Running the daemon on the host removes the boundary. Full
> rationale: `https://docs.vornik.io`.

Production deployment goes via Helm/RKE2; see `deployments/RKE2.md`.

## Quick start

```bash
curl -fsSL https://get.vornik.io | bash      # clone + build + install + start
```

The one-liner pins the **release tag baked into the script** (not a moving
branch); set `VORNIK_REF=main` for bleeding-edge. To verify the script against
its published checksum before piping it to a shell (catches transit/redirect
tampering — not a signature), fetch both first:

```bash
REF=<release>  # a tag from github.com/grinco/vornik/releases that ships quickstart.sh.sha256
base="https://raw.githubusercontent.com/grinco/vornik/$REF/deployments/podman"
curl -fsSLO "$base/quickstart.sh" && curl -fsSLO "$base/quickstart.sh.sha256"
sha256sum -c quickstart.sh.sha256 && VORNIK_REF="$REF" bash quickstart.sh
```

That one-liner (this directory's `quickstart.sh`) installs prerequisites,
builds `vornik` + `vornikctl` in an ephemeral golang container, builds the
agent image, seeds `~/.config/vornik`, brings up the deps, and starts the
`vornik` user service. Then connect an LLM — open
<http://localhost:8080/ui> and the first-run **setup guide** (`/ui/setup`)
tests your endpoint + key and creates a first project. Or from the terminal:

```bash
$EDITOR ~/.config/vornik/vornik.env      # set VORNIK_CHAT_API_KEY (+ CHAT_ENDPOINT/CHAT_MODEL)
systemctl --user restart vornik
curl -s http://localhost:8080/readyz
xdg-open http://localhost:8080/ui
```

### Manual (equivalent steps)

```bash
cd deployments/podman
cp .env.example .env                                    # postgres creds/ports for compose
podman compose -f deps.compose.yaml up -d               # postgres (+ scraper.compose.yaml on EE)
make build-agent                                        # ghcr.io/grinco/vornik-agent:latest
go build -o ~/.local/bin/vornik    ./cmd/vornik         # or use the ephemeral-build one-liner
go build -o ~/.local/bin/vornikctl ./cmd/vornikctl
cp config/vornik.host.yaml ~/.config/vornik/config.yaml
cp vornik.env.example      ~/.config/vornik/vornik.env  # edit: LLM key, run-as user
install -m644 systemd/vornik.service ~/.config/systemd/user/vornik.service
systemctl --user daemon-reload && systemctl --user enable --now vornik
```

First boot runs the full migration set (≈10 s); the ephemeral build adds
≈2–3 min the first time (cached afterward).

## Runtime model

```
   HOST (your user, rootless podman)
   ┌──────────────────────────────────────────────────────────┐
   │  vornik.service (systemctl --user) → ~/.local/bin/vornik   │
   │    - reads ~/.config/vornik/{config.yaml,configs,vornik.env}│
   │    - data under ~/.local/share/vornik                      │
   │    - DB → 127.0.0.1:5432 ; scraper → 127.0.0.1:8787 (EE)    │
   │    - spawns agents via `podman run` (sibling containers)   │
   │         │                                                  │
   │         ├── vornik-postgres (pgvector)   :127.0.0.1:5432   │
   │         ├── vornik-scraper  (EE)         :127.0.0.1:8787   │
   │         └── agent container 1..N                           │
   │               VORNIK_API_URL = host.containers.internal:8080│
   └──────────────────────────────────────────────────────────┘
```

Key details:

- The daemon and the agents it spawns share **one filesystem view**, so the
  exec-scratch and workspace bind mounts that broke under the old
  daemon-in-a-container topology now resolve.
- The daemon binds `0.0.0.0:8080`; agents (sibling containers) reach it via
  `host.containers.internal:8080`, which vornik injects as `VORNIK_API_URL`
  (`internal/service/container.go:agentCallbackURL`). Firewall the host if
  the LAN is untrusted.
- Postgres (and the EE scraper) publish on loopback; the daemon reaches them
  at `127.0.0.1`. The daemon does **not** need `podman.socket` — it shells
  out to the `podman` CLI directly (`internal/runtime/manager.go`).

## Configuration

Everything lives under `~/.config/vornik/` (XDG), seeded on first run and
never clobbered by a re-run:

### `vornik.env` (secrets + env)

Loaded by the systemd unit; expanded into `config.yaml` via `${VAR}`
placeholders. See `vornik.env.example`. Minimum for a working demo:

```bash
VORNIK_CHAT_API_KEY=sk-ant-...
CHAT_ENDPOINT=https://api.anthropic.com
CHAT_MODEL=claude-opus-4-7
```

After editing: `systemctl --user restart vornik`.

### `config.yaml`

The daemon's YAML config (seeded from `config/vornik.host.yaml`). `${VAR}`
placeholders expand from `vornik.env` at load. Edit in place for structural
changes; keep secrets in `vornik.env`.

### `configs/` — registry (projects / swarms / workflows)

Seeded from the repo `configs/` tree on first run and owned by you; the UI
writes project/swarm/workflow edits back here. Edit, then
`systemctl --user restart vornik` to pick up changes.

## Operations

```bash
# Daemon
journalctl --user -u vornik -f                 # logs
systemctl --user restart vornik                # pick up config changes
vornikctl doctor                               # same checks as /api/v1/doctor
vornikctl project list

# Deps
cd deployments/podman
podman compose -f deps.compose.yaml logs -f postgres
podman exec -it vornik-postgres psql -U vornik -d vornik
podman exec vornik-postgres psql -U vornik -d vornik -c '\dx vector'

# Containers vornik manages (deps + spawned agents)
podman ps --filter "label=vornik.managed=true"

# Stop deps, keep state
podman compose -f deps.compose.yaml down
# Stop and wipe DB state
podman compose -f deps.compose.yaml down -v
```

## Upgrading

Re-run the one-liner (it `git pull`s, rebuilds the binaries, and restarts),
or by hand:

```bash
git -C ~/vornik pull --ff-only
go build -o ~/.local/bin/vornik    ./cmd/vornik     # from ~/vornik
go build -o ~/.local/bin/vornikctl ./cmd/vornikctl
systemctl --user restart vornik
```

Migrations run automatically on startup; already-applied versions are skipped.

## Exposing beyond the host

The daemon binds `0.0.0.0:8080` (required for the agent callback) — fine for
a private host, not for a shared network. Tighten by:

1. Setting `api.auth_enabled: true` in `~/.config/vornik/config.yaml` and
   adding keys to `api_keys`.
2. Putting a real reverse proxy (Caddy, nginx) with TLS in front of `:8080`
   and firewalling the raw port.
3. Keeping the Postgres (and scraper) port on `127.0.0.1` (the default).

Anything more serious should go through the Helm chart in
`deployments/helm/vornik` onto a real cluster.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Build fails | Go 1.25 image pull blocked | Pre-pull: `podman pull docker.io/library/golang:1.25` |
| Immutable OS (Bazzite/Silverblue): "could not install" | no `dnf`; podman ships in the image | The installer auto-detects this: it skips already-present tools and installs the compose provider via `pip --user` (no root/reboot). If a core tool is genuinely missing: `sudo rpm-ostree install <pkg> && systemctl reboot`, or install Homebrew. |
| `vornik.service` won't start | missing config / secrets | `journalctl --user -u vornik -e` — the loader prints which placeholders are empty |
| `connection refused` to postgres | DB still starting | Wait 30 s; healthcheck has a 30 s `start_period` |
| Agents never start | agent image missing on host podman | `make build-agent`, or set `AGENT_IMAGE` to a reachable fully-qualified ref |
| Daemon stops after logout | lingering not enabled | `loginctl enable-linger "$USER"` |
| UI shows empty project list | registry not seeded | check `~/.config/vornik/configs/projects/` exists |
| `CREATE EXTENSION vector` errors on first boot | switched from plain postgres mid-flight | wipe the volume: `podman compose -f deps.compose.yaml down -v` then restart |

## Role-specialized cluster (clustering)

`cluster.compose.yaml` brings up a single-host reference cluster that demonstrates the 6-node topology from `https://docs.vornik.io`, scaled to one replica per role. Use it for integration testing the DMZ/relay path and for validating per-role config before a real multi-host deploy.

### Topology

```
  HOST VM (single host — two bridge networks simulate the VLAN boundary)

  ┌──── dmz network ────────────────────────────────────────────────────┐
  │                                                                      │
  │  vornik-webhook (thin image, profile: webhook)                       │
  │  - receives public webhook callbacks on :8083 (host port)           │
  │  - verifies provider HMAC locally (no DB needed)                    │
  │  - relays verified events over mTLS → vornik-worker:8443            │
  │  - NO trusted network access → provably cannot reach postgres       │
  │                                                                      │
  └──────────────────────────────────────┬──────────────────────────────┘
                                         │ mTLS  :8443  ← the ONE cross-boundary path
  ┌──── trusted network ─────────────────▼──────────────────────────────┐
  │                                                                      │
  │  vornik-worker (full image, profile: worker)          ← dmz + trusted
  │  - scheduler + executor (Podman) + all singleton leases             │
  │  - mTLS relay-ingress on :8443 (webhook events from DMZ)            │
  │  - data-plane API on :8082 (host port)                              │
  │                                                                      │
  │  vornik-ui (thin image, profile: ui)                                 │
  │  - serves /ui + data-plane/control API on :8081 (host port)         │
  │  - full DB access; no scheduler/executor/leases                     │
  │                                                                      │
  │  postgres (pgvector:pg16)                                            │
  │  - trusted network only; never reachable from dmz                   │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘
```

### Thin vs. full image targets

| Service | Build target | Base image | Podman |
|---|---|---|---|
| `vornik-ui` | `thin` | Debian 12-slim | no |
| `vornik-worker` | `full` | Ubuntu 24.04 | yes |
| `vornik-webhook` | `thin` | Debian 12-slim | no |

### Network segmentation — single-host compose vs. production VLANs

In this compose the two bridge networks (`vornik-cluster-trusted`, `vornik-cluster-dmz`) provide network-namespace isolation that mirrors production VLAN segmentation: containers on different bridges cannot route to each other without an explicit multi-network attachment. The `vornik-webhook` container is on `dmz` only and cannot reach `postgres` at all.

In production, replace the bridge networks with physically (or logically) isolated VLANs. The firewall rule translates to:

```
DMZ webhook subnet → job-tier hosts : 8443/tcp  (one rule, outbound from DMZ)
```

No Postgres port is ever exposed to the DMZ — the webhook tier is fully stateless with respect to the database.

### Prerequisites

1. **Generate mTLS certs** (one-time, or after rotation):

   ```bash
   bash deployments/podman/gen-cluster-certs.sh
   ```

   This creates `deployments/podman/certs/` (gitignored) with:
   - `ca.crt` — internal CA cert mounted into both worker and webhook
   - `worker-server.crt/key` — server identity for the `:8443` relay-ingress
   - `webhook-client.crt/key` — client identity for the webhook relay outbound

2. **Copy and edit `.env`** (same file as the single-node compose):

   ```bash
   cp deployments/podman/.env.example deployments/podman/.env
   # edit .env — set VORNIK_CHAT_API_KEY and CHAT_MODEL at minimum
   ```

### Bring up

```bash
cd deployments/podman
podman-compose -f cluster.compose.yaml up -d
```

### Verify network segmentation

```bash
# Confirm webhook is on dmz only — should NOT show trusted network
podman inspect vornik-cluster-webhook | grep -A5 '"Networks"'

# Confirm postgres is on trusted only — should NOT show dmz network
podman inspect vornik-cluster-postgres | grep -A5 '"Networks"'

# Confirm worker straddles both networks
podman inspect vornik-cluster-worker | grep -A5 '"Networks"'

# Webhook cannot reach postgres (firewall assertion — should time out / refuse)
podman exec vornik-cluster-webhook \
    bash -c 'timeout 3 bash -c "echo > /dev/tcp/postgres/5432" 2>&1 && echo REACHABLE || echo NOT-REACHABLE (expected)'
```

### Cluster status

```bash
# Fleet view (all nodes + lease ownership map)
podman exec vornik-cluster-worker vornikctl cluster status

# Per-node self-check
podman exec vornik-cluster-ui      vornikctl doctor
podman exec vornik-cluster-worker  vornikctl doctor
podman exec vornik-cluster-webhook vornikctl doctor feature cluster
```

### Host ports (cluster compose)

| Service | Default host port | Purpose |
|---|---|---|
| `postgres` | 5433 | DB debugging (psql/pgAdmin) — change via `CLUSTER_POSTGRES_PORT` |
| `vornik-ui` | 8081 | UI + API — change via `CLUSTER_UI_PORT` |
| `vornik-worker` | 8082 | Worker API — change via `CLUSTER_WORKER_API_PORT` |
| `vornik-worker` | 8443 | mTLS relay-ingress — change via `CLUSTER_WORKER_RELAY_PORT` |
| `vornik-webhook` | 8083 | Public webhook ingress — change via `CLUSTER_WEBHOOK_PORT` |

These default to different ports than the single-node compose (8080/5432) so both can run on the same host simultaneously.

### Webhook nodes have no DB

The `vornik-webhook` service is intentionally missing any `database:` config block and receives no `VORNIK_DATABASE_PASSWORD` env var. Its `node.profile: webhook` config would trigger a loud startup warning if DB credentials were accidentally supplied. The mTLS relay path is the only channel by which webhook events reach the database (via the worker's `enqueueVerifiedWebhook` handler).

## Files in this directory

```
deployments/podman/
├── quickstart.sh          ← the get.vornik.io one-liner (host daemon + deps)
├── deps.compose.yaml      ← single-node deps: postgres (CE + EE)
├── scraper.compose.yaml   ← scraper overlay (Enterprise-only; absent in CE)
├── vornik.env.example     ← copy to ~/.config/vornik/vornik.env (daemon secrets/env)
├── systemd/
│   └── vornik.service     ← the `systemctl --user` unit the quickstart installs
├── cluster.compose.yaml   ← role-specialized cluster (ui + worker + webhook)
├── gen-cluster-certs.sh   ← generates mTLS certs for the cluster compose
├── .env.example           ← copy to .env (postgres creds/ports for the deps compose)
├── .gitignore             ← excludes .env, certs/, *.key, *.crt
├── config/
│   ├── vornik.host.yaml   ← host daemon config (seeds ~/.config/vornik/config.yaml)
│   ├── ui.yaml            ← cluster: profile: ui config
│   ├── worker.yaml        ← cluster: profile: worker config (+ relay_ingress)
│   └── webhook.yaml       ← cluster: profile: webhook config (+ relay)
├── README.md              ← this file
└── SETUP_NOTES.md         ← historical notes from the postgres-only era
```

The postgres init SQL lives at `../postgres/init/00-init.sql` (shared with the chart).
