# vornik Demo Fleet — podman-compose

One-command bringup of the full vornik fleet on a single VM for demos and local testing. The compose file starts:

| Container | Image | Purpose |
|---|---|---|
| `vornik-postgres` | `pgvector/pgvector:pg16` | Database (with vector extension) |
| `vornik` | built from `deployments/docker/Dockerfile` | Daemon + UI + API |

Agent containers are **not** in the compose file — vornik spawns them on-demand via the host's podman socket (see [Runtime model](#runtime-model)).

Production deployment goes via Helm/RKE2; see `deployments/RKE2.md`.

## Quick start

```bash
# 0. (One-time) enable podman's user socket on the VM.
systemctl --user enable --now podman.socket

# 1. Build the agent image on the host. vornik spawns this per task.
make build-agent

# 2. Configure and launch.
cd deployments/podman
cp .env.example .env
# edit .env — set VORNIK_CHAT_API_KEY at minimum
podman-compose up -d

# 3. Verify.
podman-compose ps
curl -s http://localhost:8080/healthz
open http://localhost:8080/ui
```

First boot takes longer than subsequent ones because the vornik image builds from the Dockerfile (≈2 min) and Postgres runs the full v1..v10 migration set (≈10 s).

## Runtime model

```
   HOST VM
   ┌──────────────────────────────────────────────────────────┐
   │                                                          │
   │  podman.sock (rootless or rootful)                       │
   │     ▲                                                    │
   │     │ mounted at /var/run/podman/podman.sock             │
   │     │                                                    │
   │  ┌──┴──────────────────────┐   ┌─────────────────────┐   │
   │  │ vornik container        │   │ postgres container  │   │
   │  │   - reads config/       │   │   pgvector/pg16     │   │
   │  │   - talks to postgres   │───┤                     │   │
   │  │   - asks host podman    │   └─────────────────────┘   │
   │  │     to start agents     │   (compose bridge network)  │
   │  └───────┬─────────────────┘                             │
   │          │ host podman spawns...                         │
   │          ▼                                               │
   │  ┌──────────────────────┐   ┌──────────────────────┐     │
   │  │ agent container 1    │   │ agent container N    │     │
   │  │   vornik-agent:...   │   │   vornik-agent:...   │     │
   │  │   VORNIK_API_URL =   │   │   (each spawned per  │     │
   │  │   host.containers    │   │   task execution)    │     │
   │  │   .internal:8080 ←───┼───┼─────────┐            │     │
   │  └──────────────────────┘   └─────────┼────────────┘     │
   │                                       ▼                  │
   │                                  localhost:8080          │
   │                                  (vornik port-forward)   │
   └──────────────────────────────────────────────────────────┘
```

Key details:

- The vornik daemon runs in a container, but **agents don't** — they're started by the host's podman via the socket mount. This mirrors the bare-metal topology (vornik running via systemd) that most dev installs use, so swarm YAML configs from a host install work as-is.
- vornik binds on `0.0.0.0:8080` inside its container. The compose file exposes that port on the host at `8080`. Agents (on the host podman network) reach vornik via `host.containers.internal:8080`, which vornik injects automatically as `VORNIK_API_URL` — see `internal/service/container.go:agentCallbackURL`.
- Postgres stays inside the compose bridge network; the host port mapping is there only for psql/pgAdmin convenience.

## Configuration

### Environment (`.env`)

All runtime-tunable values live in `.env`. See `.env.example` for the full list with inline docs. The minimum for a working demo is:

```bash
VORNIK_CHAT_API_KEY=sk-ant-...
CHAT_ENDPOINT=https://api.anthropic.com
CHAT_MODEL=claude-opus-4-7
```

### vornik config (`config/vornik.yaml`)

The daemon's YAML config. Uses `${VAR}` placeholders that get expanded at load time from the env values passed by the compose file. Edit in place when you need structural changes (adding gate definitions, tweaking scheduler tuning, etc.); for secrets, prefer `.env`.

### Registry (projects / swarms / workflows)

Mounted **read-only from the repo root** at `../../configs` → `/etc/vornik/configs`. That means `configs/projects/*.yaml`, `configs/swarms/*.yaml`, and `configs/workflows/*.yaml` in the repo are what vornik sees. Edit there, then `podman-compose restart vornik` to pick up changes.

## Operations

```bash
# Tail logs
podman-compose logs -f vornik
podman-compose logs -f postgres

# Restart just vornik (pick up config changes)
podman-compose restart vornik

# In-pod doctor — same checks as /api/v1/doctor
podman exec vornik vornikctl doctor

# Connect to the DB
podman exec -it vornik-postgres psql -U vornik -d vornik

# Verify pgvector is installed
podman exec vornik-postgres psql -U vornik -d vornik -c '\dx vector'

# List containers managed by vornik (compose services + spawned agents)
podman ps --filter "label=vornik.managed=true"

# Stop the fleet, keep state
podman-compose down

# Stop and wipe state (DB and vornik data both)
podman-compose down -v
podman volume rm vornik-postgres-data vornik-data 2>/dev/null || true
```

## Upgrading

The image is built locally from the current checkout. To pick up code changes:

```bash
# Force rebuild
podman-compose build vornik
podman-compose up -d vornik

# Or pull from a registry if you're pinning a version
# (set image: explicitly in the compose file)
podman-compose pull vornik
podman-compose up -d vornik
```

Migrations run automatically on startup; already-applied versions are skipped.

## Exposing beyond the VM

The compose file binds ports to `0.0.0.0` by default — fine for a private VM, not fine for anything on a shared network. Tighten by:

1. Setting `api.auth_enabled: true` in `config/vornik.yaml` and adding keys to `api_keys`.
2. Changing the port mapping in `podman-compose.yaml` from `"8080:8080"` to `"127.0.0.1:8080:8080"` and putting a real reverse proxy (Caddy, nginx) in front with TLS.
3. For the DB port, either remove the mapping entirely (vornik doesn't need it exposed) or restrict to `127.0.0.1`.

Anything more serious should go through the Helm chart in `deployments/helm/vornik` onto a real cluster.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `podman-compose up` fails on build | Go 1.25 download blocked | Pre-pull: `podman pull docker.io/library/golang:1.25` |
| vornik container restart-looping | `.env` missing or secrets unset | `podman-compose logs vornik` — the config loader prints which placeholders are empty |
| `connection refused` to postgres | DB still starting | Wait 30 s; healthcheck has a 30 s `start_period` |
| Agents never start | Podman socket not mounted | Check `ls -l $PODMAN_SOCK` on the host; enable with `systemctl --user enable --now podman.socket` |
| Agents start but time out pulling image | Agent image not on host podman | `make build-agent` on the host, or push to a registry and set `AGENT_IMAGE` to a reachable ref |
| UI shows empty project list | Registry mount empty | Check `../../configs/projects/` exists relative to the compose file |
| `CREATE EXTENSION vector` errors on first boot | You switched images from plain postgres to pgvector mid-flight | Wipe the volume: `podman-compose down -v` then restart |

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
├── podman-compose.yaml    ← single-node fleet (the original entry point)
├── cluster.compose.yaml   ← role-specialized cluster (ui + worker + webhook)
├── gen-cluster-certs.sh   ← generates mTLS certs for the cluster compose
├── .env.example           ← copy to .env and edit (shared by both composes)
├── .gitignore             ← excludes .env, certs/, *.key, *.crt
├── config/
│   ├── vornik.yaml        ← single-node daemon config
│   ├── ui.yaml            ← cluster: profile: ui config
│   ├── worker.yaml        ← cluster: profile: worker config (+ relay_ingress)
│   └── webhook.yaml       ← cluster: profile: webhook config (+ relay)
├── README.md              ← this file
└── SETUP_NOTES.md         ← historical notes from the postgres-only era
```

The postgres init SQL lives at `../postgres/init/00-init.sql` (shared with the chart).
