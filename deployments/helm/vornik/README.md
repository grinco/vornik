# vornik Helm Chart

Deploys the vornik daemon plus a pgvector-enabled Postgres as a self-contained unit on Kubernetes / RKE2.

## What it installs

| Resource | Default | Purpose |
|---|---|---|
| `StatefulSet/<release>` | 1 replica, privileged | vornik daemon + in-pod podman |
| `StatefulSet/<release>-postgres` | 1 replica | pgvector/pgvector:pg16 |
| `Service/<release>` | ClusterIP :8080 | HTTP API + UI + /metrics |
| `Service/<release>-postgres` | ClusterIP :5432 | internal DB service |
| `Secret/<release>` | Opaque | DB pw + LLM keys + tokens (unless `secrets.existingSecret` set) |
| `ConfigMap/<release>-config` | | rendered `config.yaml` |
| `ConfigMap/<release>-{projects,swarms,workflows}` | optional | registry YAMLs (skipped if empty) |
| `ConfigMap/<release>-pricing` | optional | pricing.yaml override |
| `ServiceAccount/<release>` | created | dedicated audit identity |
| `Ingress/<release>` | disabled by default | UI exposure |

## Prerequisites

- Kubernetes 1.24+ (tested on RKE2).
- A default StorageClass, or set one via `persistence.storageClass`.
- The `vornik` daemon image and the `vornik-agent` image pushed to a registry reachable from every node. See [Building images](#building-images).
- If `runtimeMode: privileged` (default): namespace not bound by `pod-security.kubernetes.io/enforce: restricted`. The chart needs `privileged` (or at minimum `baseline`) PSA level.

## Quick start

```bash
# 1. Build and push the images (from the repo root)
make docker-build docker-push DOCKER_REGISTRY=your.registry.example.com
make build-agent agent-push  AGENT_REGISTRY=your.registry.example.com

# 2. Install the chart
helm install vornik deployments/helm/vornik \
  --namespace vornik --create-namespace \
  --set image.repository=your.registry.example.com/vornik \
  --set agentImage.repository=your.registry.example.com/vornik-agent \
  --set secrets.dbPassword="$(openssl rand -base64 24)" \
  --set secrets.chatApiKey="$YOUR_LLM_KEY" \
  --set chat.endpoint=https://api.anthropic.com \
  --set chat.model=claude-opus-4-7

# 3. Verify
kubectl -n vornik rollout status statefulset/vornik
kubectl -n vornik port-forward svc/vornik 8080:8080
open http://localhost:8080/ui
```

## Building images

From the repo root:

```bash
# vornik daemon
podman build -f deployments/docker/Dockerfile -t <registry>/vornik:<version> .
podman push <registry>/vornik:<version>

# vornik-agent (bundles mcp-bridge + the LLM worker entrypoint)
make build-agent AGENT_IMAGE=<registry>/vornik-agent:<version>
podman push <registry>/vornik-agent:<version>
```

The chart's `image.tag` defaults to `Chart.AppVersion`. Pass `--set image.tag=<version>` if your pushed tag differs.

## Runtime mode: privileged vs host-socket

vornik spawns agent containers by shelling out to `podman`. It has no Kubernetes-native runtime — the pod must contain or reach a usable podman.

- **`runtimeMode: privileged`** *(default)*
  The vornik container runs `privileged: true` and ships its own podman binary (installed into the image). Container storage lives in an `emptyDir` at `/var/lib/containers` so warm containers are cleaned up with the pod.
  Works on any node. Requires a namespace that permits privileged pods.

- **`runtimeMode: host-socket`**
  The pod is non-privileged but mounts `/run/podman/podman.sock` from the node via hostPath, and `CONTAINER_HOST` is set so the bundled podman CLI talks to the node's podman service.
  Better privilege posture — but ties pods to specific nodes (use `nodeSelector` or a DaemonSet shape). The target nodes must have `podman.service` running and the socket world-readable (or SELinux policies aligned).

## Database

The bundled Postgres uses `pgvector/pgvector:pg16`. vornik's migrations run `CREATE EXTENSION IF NOT EXISTS vector` on first boot; the image has the extension binary pre-built so the CREATE succeeds without a superuser dance.

To use an external Postgres (recommended for production):

```yaml
postgres:
  enabled: false
database:
  host: my-postgres.example.com
  port: 5432
  name: vornik
  user: vornik
  sslmode: require
secrets:
  dbPassword: ...
```

The external DB must have `CREATE EXTENSION` privilege on the `vornik` database, or an operator must pre-install the extension.

## Secrets

Default: the chart creates one Secret with every sensitive field. For production, manage the Secret out-of-band (Sealed Secrets, ExternalSecrets, Vault agent injector, etc.) and set `secrets.existingSecret: <name>`. The expected keys are:

| key | required | notes |
|---|---|---|
| `dbPassword` | yes | Postgres password |
| `chatApiKey` | no (but usually yes) | LLM key for dispatcher / Telegram |
| `agentLLMApiKey` | no | falls back to `chatApiKey` |
| `embeddingApiKey` | no | falls back to `chatApiKey` |
| `telegramBotToken` | only if `telegram.enabled` | |
| `apiKeys` | if `api.authEnabled` | JSON array, e.g. `["k1","k2"]` |

## Registry configs (projects / swarms / workflows)

Two ways to supply them:

1. **Inline via values** — small deployments:
   ```yaml
   registry:
     projects:
       dev: |
         id: dev
         swarm: dev-swarm
         workflow: adaptive
     swarms:
       dev-swarm: |
         id: dev-swarm
         roles:
           - name: analyst
             model: claude-sonnet-4-6
     workflows:
       adaptive: |
         workflowId: adaptive
         entrypoint: plan
         steps: {}
   ```

2. **External ConfigMaps** — larger deployments managed via GitOps:
   ```yaml
   registry:
     existingProjectsConfigMap: vornik-projects
     existingSwarmsConfigMap: vornik-swarms
     existingWorkflowsConfigMap: vornik-workflows
   ```
   Each ConfigMap's keys must be `<name>.yaml` filenames; they mount directly under `/etc/vornik/configs/<subdir>/`.

## Chat provider

The daemon's `chat.Provider` interface is pluggable. Seven concrete providers ship today:

- **HTTP** (`chat.provider: http`) — default. OpenAI-compatible endpoint; needs `endpoint` + `api_key` + `model`.
- **Claude subscription** (`chat.provider: claude-subscription` or as a router sub-provider) — talks to `api.anthropic.com/v1/messages` directly with the OAuth tokens written by `claude login`. No subprocess, native `tool_use` blocks, per-delta streaming.
- **Codex subscription** (router sub-provider `codex-subscription`) — talks to the ChatGPT Codex Responses API with tokens from `codex login`. No subprocess, only the tools we declare are visible to the model.
- **Vertex AI** (router sub-provider `vertex`) — talks to Google Vertex AI's OpenAI-compat endpoint with an API key (no GCP OAuth). Requires `chat.router.vertex.{api_key, project_id, location, model}`. Default routes pick this up for `gemini-*` and `google/*` model prefixes.
- **Claude CLI** (`chat.provider: claude-cli`) — deprecated; shells out to the `claude` binary. Prefer `claude-subscription`.
- **Codex CLI** (`chat.provider: codex-cli`) — deprecated; shells out to `codex exec`. Prefer `codex-subscription`.
- **Router** (`chat.provider: router`) — composes multiple providers and dispatches each request to the one whose model prefix matches. Default routes map `claude-*` → claude-subscription, `gpt-*` / `o3-` / `o4-` / `codex` → codex-subscription, `gemini-` / `google/` → vertex, falling back to the CLI variants (or the router's default) when the preferred provider isn't enabled.

The stock chart is primarily targeted at HTTP deployments — bundling authenticated Claude / Codex / GCP credentials into the pod is operator-specific (derive an image, project `~/.claude/.credentials.json` or `~/.codex/auth.json` via a secret volume, set `CLAUDE_CODE_OAUTH_TOKEN` for the claude-subscription provider, or pass the Vertex API key via a Kubernetes secret mounted into the daemon's env and referenced from `extraConfig.chat.router.vertex.api_key`). The Router provider pays off most when agents pick per-role models freely and you want each `claude-*`, `gpt-5*`, or `gemini-*` request to reach its respective backend without re-routing in swarm YAMLs.

The daemon exposes an **internal OpenAI-compatible proxy** at `POST /api/v1/chat/completions`, which forwards requests to whatever `chat.Provider` is wired. Agent containers can point their `VORNIK_LLM_ENDPOINT` at this path to route their own traffic through the same provider (so dispatcher and agents share auth). The proxy honors the request's `model` field when the provider supports per-request override (HTTP, Claude CLI, Codex CLI, and Router all do) — so per-role `model:` settings in swarm YAMLs drive actual per-call routing.

## Observability

- `/metrics` on the main HTTP port (always on)
- Separate metrics listener at `:9090` when `metrics.enabled: true`
- Grafana dashboards live under `deployments/grafana/` in the repo — scrape with a standard `ServiceMonitor` targeting the `metrics` port.

Tracing (OTEL), Prometheus, and Grafana wiring are intentionally **not** part of this chart — bring them via their own charts in your cluster.

## Upgrades

```bash
helm upgrade vornik deployments/helm/vornik \
  --namespace vornik \
  --reuse-values \
  --set image.tag=<new-version>
```

Migrations run automatically on the new pod's startup. The `startupProbe` gives up to 5 minutes for first-boot migrations; subsequent restarts reuse the already-migrated DB and come up in seconds.

## Troubleshooting

```bash
# daemon logs
kubectl -n vornik logs -f statefulset/vornik

# in-pod doctor (same check set as /api/v1/doctor)
kubectl -n vornik exec -it vornik-0 -- vornikctl doctor

# force the daemon to re-read config files
kubectl -n vornik exec -it vornik-0 -- vornikctl reload

# verify pgvector extension
kubectl -n vornik exec -it vornik-postgres-0 -- psql -U vornik -d vornik -c '\dx vector'
```

Common issues:

- **Agents fail with "failed to start container"** — the agent image isn't reachable from inside the vornik pod. In `privileged` mode the bundled podman pulls on demand; check registry auth and that the `agentImage.repository` points at your pushed tag.
- **First-boot stuck in startupProbe** — DB migration is taking longer than 5 minutes, or pgvector extension creation failed. Check `kubectl logs` for `migrations` and `CREATE EXTENSION vector`.
- **Privileged pods rejected** — your namespace enforces restricted PSA. Either switch to `runtimeMode: host-socket` (and accept node coupling) or label the namespace permissive.
