# Deploying vornik to RKE2

End-to-end guide for a production install on an RKE2 cluster.

```
deployments/
├── docker/            ← Dockerfile + entrypoint for the vornik image
├── helm/vornik/       ← Helm chart (vornik + bundled pgvector)
├── podman/            ← local dev compose (pre-k8s path)
├── postgres/          ← SQL schema (used by both the compose file and chart)
├── grafana/           ← dashboards, scraped via ServiceMonitor (later)
└── RKE2.md            ← you are here
```

## 1 — Prerequisites

On your workstation:
- `podman` or `docker` to build images
- `helm` 3.12+
- `kubectl` with a kubeconfig pointing at the RKE2 cluster

On the cluster:
- A reachable container registry (Harbor, ghcr.io, self-hosted, …)
- A default `StorageClass` (RKE2 ships with `local-path`; fine for a single-node test, not for production — use Longhorn or a cloud CSI for real workloads)
- A namespace that permits privileged pods for the vornik StatefulSet, OR a plan to run `runtimeMode: host-socket` (see below)

## 2 — Build and push images

From the repo root:

```bash
REGISTRY=your.registry.example.com
VERSION=$(git describe --tags --always)

# Daemon image
podman build \
  -f deployments/docker/Dockerfile \
  -t ${REGISTRY}/vornik:${VERSION} \
  --build-arg VERSION=${VERSION} \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  .
podman push ${REGISTRY}/vornik:${VERSION}

# Agent image (used by vornik to run tasks)
make build-agent VORNIK_UID=1000 VORNIK_GID=1000
podman tag vornik-agent:latest ${REGISTRY}/vornik-agent:${VERSION}
podman push ${REGISTRY}/vornik-agent:${VERSION}
```

The bundled Postgres image (`pgvector/pgvector:pg16`) comes from Docker Hub by default. Mirror it to your registry if your cluster can't reach Docker Hub.

## 3 — Prepare secrets

vornik needs (at minimum) a DB password and an LLM API key. For first-install convenience pass them via `--set`; switch to an externally-managed Secret before going to production.

```bash
export DB_PASSWORD=$(openssl rand -base64 24)
export CHAT_API_KEY=sk-ant-...   # or your provider's key
```

If `api.authEnabled: true` (default), also generate one or more API keys. The `secrets.apiKeys` value is a JSON array string:

```bash
export API_KEYS='["'"$(openssl rand -hex 32)"'"]'
```

## 4 — Handle the privileged-pod question

vornik spawns LLM agent containers by shelling out to `podman` — it has no Kubernetes-native runtime backend. The pod therefore has to contain or reach a usable podman. Choose one:

### Option A — privileged (default)

The simplest path. Label the namespace to permit it:

```bash
kubectl create namespace vornik
kubectl label namespace vornik \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/enforce-version=latest
```

If your cluster uses a different admission controller (Kyverno, Gatekeeper), create the equivalent exception scoped to the `vornik` ServiceAccount.

### Option B — host-socket

Run podman.service on each node that should schedule vornik, then set:

```yaml
runtimeMode: host-socket
podmanHostSocket: /run/podman/podman.sock
nodeSelector:
  vornik.host/podman: "true"   # label your podman-capable nodes
```

You lose cluster-wide portability but gain a non-privileged pod. Recommended if you have a compliance requirement against privileged workloads.

## 5 — Install the chart

```bash
helm install vornik deployments/helm/vornik \
  --namespace vornik --create-namespace \
  --set image.repository=${REGISTRY}/vornik \
  --set image.tag=${VERSION} \
  --set agentImage.repository=${REGISTRY}/vornik-agent \
  --set agentImage.tag=${VERSION} \
  --set secrets.dbPassword="${DB_PASSWORD}" \
  --set secrets.chatApiKey="${CHAT_API_KEY}" \
  --set secrets.apiKeys="${API_KEYS}" \
  --set chat.endpoint=https://api.anthropic.com \
  --set chat.model=claude-opus-4-7 \
  --set agentLLM.endpoint=https://api.anthropic.com \
  --set agentLLM.model=claude-sonnet-4-6
```

For anything beyond a smoke test, write a `values.yaml` instead of `--set`:

```bash
helm install vornik deployments/helm/vornik \
  --namespace vornik --create-namespace \
  -f my-values.yaml
```

## 6 — Verify

```bash
kubectl -n vornik rollout status statefulset/vornik
kubectl -n vornik rollout status statefulset/vornik-postgres

# daemon logs — look for "migrations applied" lines
kubectl -n vornik logs -f statefulset/vornik

# in-pod health check — same checks as /api/v1/doctor
kubectl -n vornik exec -it vornik-0 -- vornikctl doctor

# pgvector extension check
kubectl -n vornik exec -it vornik-postgres-0 -- \
  psql -U vornik -d vornik -c '\dx vector'

# hit the UI locally
kubectl -n vornik port-forward svc/vornik 8080:8080
open http://localhost:8080/ui
```

## 7 — Exposing externally

The chart includes an optional `Ingress` resource. Enable it and point it at your IngressClass:

```yaml
ingress:
  enabled: true
  className: nginx           # or traefik (RKE2's default)
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: vornik.yourdomain.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - hosts: [vornik.yourdomain.com]
      secretName: vornik-tls
```

**Always** enable `api.authEnabled` before exposing externally, and rotate `secrets.apiKeys` to keys you haven't committed anywhere.

## 8 — Upgrades

```bash
REGISTRY=...
VERSION=$(git describe --tags --always)

# Rebuild & push both images with the new tag, then:

helm upgrade vornik deployments/helm/vornik \
  --namespace vornik \
  --reuse-values \
  --set image.tag=${VERSION} \
  --set agentImage.tag=${VERSION}
```

Migrations are idempotent and run on every start; already-applied versions are skipped. The vornik StatefulSet has `startupProbe` with up to 5 minutes tolerance, enough for multi-migration first-boot; subsequent upgrades typically finish migrations in seconds.

## 9 — Chat provider and the internal proxy

vornik's `chat.Provider` interface has four implementations today:

- **HTTP** (`chat.provider: http`, default) — any OpenAI-compatible endpoint.
- **Claude CLI** (`chat.provider: claude-cli`) — shells out to the `claude` binary and reuses its authentication. Nice on bare-metal / single-VM hosts where the CLI is already logged in.
- **Codex CLI** (`chat.provider: codex-cli`) — same pattern for OpenAI's `codex exec` CLI.
- **Router** (`chat.provider: router`) — composes the above and dispatches each request by matching the model prefix (`claude-*` → Claude, `gpt-*` / `o3-` / `o4-` / `codex` → Codex, else fallback).

For cluster installs, any CLI provider requires a container image that bundles an authenticated CLI — derive from the stock vornik image and install / preseed creds yourself.

The daemon also runs an **internal OpenAI-compatible proxy** at `POST /api/v1/chat/completions` that forwards to whichever `chat.Provider` is wired. Agent containers can be pointed at it via `runtime.agent_llm.endpoint=http://<service>:8080/api/v1` so both the dispatcher and the agents share a single authenticated upstream — no second API key to provision, no per-agent auth. The proxy honors the request's `model` field, so per-role `model:` settings in your swarm YAMLs flow through to per-call Claude (or per-call any other backend) selection.

On the Helm chart this boils down to:

```yaml
chat:
  provider: http                     # or claude-cli if you've built that image
  endpoint: https://api.anthropic.com
  model: claude-sonnet-4-6
# (agent_llm isn't first-class in the chart yet — edit extraConfig if you
# want agents to go through the internal proxy)
extraConfig:
  runtime:
    agent_llm:
      endpoint: "http://<release>.<ns>.svc.cluster.local:8080/api/v1"
      model: "claude-sonnet-4-6"
      api_key: "local"
```

## 10 — What's not in this chart

Intentionally out of scope — add via their own charts / manifests:

- **Prometheus** — scrape `Service/<release>` port `http` path `/metrics`, or port `metrics` when `metrics.enabled: true`.
- **Grafana** — import dashboards from `deployments/grafana/`.
- **OTEL collector** — vornik has an OTLP gRPC exporter; point it at your collector via `extraConfig.tracing`.
- **Role-specialized / multi-node clustering** — now supported via `cluster.enabled: true`. See [§ 11 — Role-specialized cluster](#11--role-specialized-cluster) below.

## 11 — Role-specialized cluster

### 11.1 — When to use it

The default chart (`cluster.enabled: false`) runs a single all-in-one StatefulSet: the daemon serves the UI, the data-plane API, public webhooks, and the executor (Podman) all in one pod. That is the right shape for a single-node install or a small uniform replica set.

Switch to `cluster.enabled: true` when you need:

- **Network segmentation** — public webhook ingress in a DMZ VLAN, isolated from the internal job tier and database.
- **Workload separation** — UI/webhook nodes carry no container runtime and need no privileged mode; only the job (worker) tier needs Podman.
- **Independent scaling** — scale webhook replicas for webhook burst without replicating the executor tier.

The reference topology is six nodes in three tiers:

| Tier | Count | Subnet | Profile | Image | Podman? | DB access |
|------|-------|--------|---------|-------|---------|-----------|
| UI | 2 | trusted | `ui` | thin | no | R/W (primary) |
| Worker | 2 | trusted | `worker` | full | yes | R/W (primary) |
| Webhook | 2 | DMZ VLAN | `webhook` | thin | no | none |

The two webhook nodes receive HTTPS webhook callbacks, verify provider HMAC signatures, and forward verified events over mTLS to the worker tier at `:8443` (the relay-ingress endpoint). That is the **only** firewall hole across the VLAN boundary. The DMZ nodes hold no database credentials, no LLM keys, and no broker keys — only webhook signing secrets and their mTLS client cert.

A DMZ Postgres replica is **not needed** and should not be created: the webhook tier performs no database reads or writes (verification is stateless; enqueue is the job tier's responsibility). Adding a replica would open a second firewall hole for zero benefit.

### 11.2 — Build both images

From the repo root:

```bash
REGISTRY=your.registry.example.com
VERSION=$(git describe --tags --always)

# full image — worker tier (profile: worker / all). DEFAULT build target.
podman build \
  -f deployments/docker/Dockerfile \
  -t ${REGISTRY}/vornik:${VERSION} \
  --build-arg VERSION=${VERSION} \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  .
podman push ${REGISTRY}/vornik:${VERSION}

# thin image — ui and webhook tiers (podman-free, profile: ui / webhook).
podman build \
  -f deployments/docker/Dockerfile \
  --target thin \
  -t ${REGISTRY}/vornik:${VERSION}-thin \
  --build-arg VERSION=${VERSION} \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  .
podman push ${REGISTRY}/vornik:${VERSION}-thin
```

The `thin` image is based on `debian:12-slim` with no Podman, no container storage, and no privileged-mode requirement. It cannot run the `worker` or `all` profile — using it for the worker tier will cause a startup failure. The `full` image (Ubuntu 24.04 + Podman) is the right choice for the worker tier and for single all-in-one installs.

### 11.3 — Label nodes for tier placement

Label each RKE2 node so the chart's `nodeSelector` values can target the right subnet:

```bash
# Trusted-subnet nodes (UI + Worker)
kubectl label node ui-node-1     vornik.io/tier=trusted
kubectl label node ui-node-2     vornik.io/tier=trusted
kubectl label node worker-node-1 vornik.io/tier=trusted
kubectl label node worker-node-2 vornik.io/tier=trusted

# Worker nodes also need the podman-capable label from § 4 (host-socket
# mode) or a privileged-pod admission exception — see § 4 above.
kubectl label node worker-node-1 vornik.host/podman=true
kubectl label node worker-node-2 vornik.host/podman=true

# DMZ VLAN nodes (Webhook)
kubectl label node webhook-node-1 vornik.io/tier=dmz
kubectl label node webhook-node-2 vornik.io/tier=dmz
```

The chart exposes per-role `nodeSelector`, `tolerations`, and `affinity` blocks — all verified present in `values.yaml`:

```yaml
cluster:
  ui:
    nodeSelector:
      vornik.io/tier: trusted
    tolerations: []
    affinity: {}

  worker:
    nodeSelector:
      vornik.io/tier: trusted
    tolerations: []
    affinity: {}

  webhook:
    nodeSelector:
      vornik.io/tier: dmz
    tolerations: []
    affinity: {}
```

Wire these in your `values.yaml` (or pass via `--set`) to keep UI and worker pods on the trusted subnet and webhook pods in the DMZ.

### 11.4 — Generate the mTLS relay bundle

The chart does not generate certificates. Generate the internal CA, worker server cert, and webhook client cert before running `helm install`. Use the provided helper (adapt for production PKI — see note below).

**Step 1 — generate the cert files.**

Pass the Kubernetes Service DNS name for your release via `VORNIK_WORKER_DNS`. The chart derives the worker Service name as `<release>-vornik-worker`; if your Helm release is named `vornik` the Service is `vornik-vornik-worker`.

```bash
# Replace "vornik" with your actual Helm release name.
VORNIK_WORKER_DNS="vornik-vornik-worker" \
  bash deployments/podman/gen-cluster-certs.sh
```

The script accepts multiple comma-separated names if you need the same cert to cover both the compose service name and the Kubernetes Service name:

```bash
VORNIK_WORKER_DNS="vornik-worker,vornik-vornik-worker" \
  bash deployments/podman/gen-cluster-certs.sh
```

The default (no `VORNIK_WORKER_DNS`) produces a cert for `vornik-worker` — correct for podman compose, wrong for Helm. Always set the env var for Helm installs.

The script produces (in `deployments/podman/certs/`):

```
certs/
  ca.crt               — internal CA certificate (PEM)
  worker-server.crt    — server cert for the worker relay-ingress listener
                         SAN: DNS:<worker-dns-name(s)>, DNS:localhost, IP:127.0.0.1
  worker-server.key    — server private key
  webhook-client.crt   — client cert for webhook-node outbound relay
  webhook-client.key   — client private key
```

**Step 2 — load the cert files into a Kubernetes Secret.**

```bash
kubectl -n vornik create secret generic vornik-relay-mtls \
  --from-file=ca.crt=deployments/podman/certs/ca.crt \
  --from-file=server.crt=deployments/podman/certs/worker-server.crt \
  --from-file=server.key=deployments/podman/certs/worker-server.key \
  --from-file=client.crt=deployments/podman/certs/webhook-client.crt \
  --from-file=client.key=deployments/podman/certs/webhook-client.key
```

Remove the local cert files after loading (never commit keys to the repository):

```bash
rm -rf deployments/podman/certs/
```

Expected Secret keys (chart documentation): `ca.crt`, `server.crt`, `server.key`, `client.crt`, `client.key`.

> **Note:** `deployments/podman/gen-cluster-certs.sh` produces self-signed 365-day certs suited for development and initial lab validation. For production, issue certs from your organization's internal PKI, cert-manager, or Vault. Rotate before expiry — a cert rotation requires regenerating the Secret and rolling both the worker and webhook Deployments.

> **Never** commit private keys or certificates to the repository. Add `deployments/podman/certs/` to `.gitignore` if you use the local cert dir.

### 11.5 — NetworkPolicy: enforcing the DMZ boundary

Apply these NetworkPolicies alongside the Helm release to enforce the single firewall rule at the Kubernetes level.

**Deny all egress from webhook pods to Postgres, allow only to worker :8443:**

```yaml
# deny-dmz-to-db.yaml
# Webhook pods in the vornik namespace may NOT reach the postgres Service.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-webhook-to-postgres
  namespace: vornik
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: webhook   # label applied by the chart
  policyTypes:
    - Egress
  egress:
    # Allow DNS (CoreDNS in kube-system)
    - ports:
        - protocol: UDP
          port: 53
    # Allow mTLS relay to the worker Service on :8443 only
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: worker
      ports:
        - protocol: TCP
          port: 8443
    # No other egress rule — Postgres (:5432) and all other internal
    # services are implicitly denied.
```

```yaml
# deny-dmz-ingress-from-webhook.yaml
# The worker relay-ingress port :8443 accepts only mTLS; deny plain
# traffic from any pod other than webhook to :8443.
# (mTLS enforces client-cert auth on the application layer regardless,
#  but defense-in-depth at the network layer is cheap.)
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: worker-relay-ingress-from-webhook-only
  namespace: vornik
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: worker
  policyTypes:
    - Ingress
  ingress:
    # Public API + UI on :8080 — allow from any cluster source
    - ports:
        - protocol: TCP
          port: 8080
    # Relay-ingress on :8443 — webhook pods only
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: webhook
      ports:
        - protocol: TCP
          port: 8443
```

Apply both:

```bash
kubectl -n vornik apply -f deny-dmz-to-db.yaml
kubectl -n vornik apply -f deny-dmz-ingress-from-webhook.yaml
```

> **RKE2 note:** RKE2's default CNI (Canal) supports NetworkPolicy. If you have replaced Canal with a CNI that does not enforce NetworkPolicy (e.g. plain Flannel without the policy engine), these manifests will be silently ignored. Verify enforcement: see §11.8.

### 11.6 — Install the chart in cluster mode

Write a `cluster-values.yaml` for the cluster-mode overrides:

```yaml
# cluster-values.yaml

image:
  repository: your.registry.example.com/vornik
  tag: "2026.x.y"               # the full-image tag (worker tier)

# Thin image: same repo, tag with -thin suffix.
# If both fields are empty the chart derives <image.repository>:<appVersion>-thin.
# Set explicitly when your tag differs from the convention:
cluster:
  thinImage:
    repository: your.registry.example.com/vornik
    tag: "2026.x.y-thin"

  enabled: true

  mtls:
    secretName: vornik-relay-mtls

  ui:
    replicas: 2
    nodeSelector:
      vornik.io/tier: trusted

  worker:
    replicas: 2
    nodeSelector:
      vornik.io/tier: trusted

  webhook:
    replicas: 2
    nodeSelector:
      vornik.io/tier: dmz
    # relayUpstream: auto-derived from the release name. Set explicitly
    # only if your worker Service has a non-standard name:
    # relayUpstream: "https://vornik-vornik-worker.vornik.svc.cluster.local:8443"
```

Install (assumes the `vornik-relay-mtls` Secret already exists from §11.4):

```bash
kubectl create namespace vornik
kubectl label namespace vornik \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/enforce-version=latest

helm install vornik deployments/helm/vornik \
  --namespace vornik \
  -f cluster-values.yaml \
  --set secrets.dbPassword="${DB_PASSWORD}" \
  --set secrets.chatApiKey="${CHAT_API_KEY}" \
  --set secrets.apiKeys="${API_KEYS}" \
  --set cluster.enabled=true \
  --set cluster.mtls.secretName=vornik-relay-mtls
```

> The privileged namespace label is needed for the worker tier only (Podman). The UI and webhook tiers use the thin image which requires no privileged context. If your cluster uses Kyverno or OPA/Gatekeeper, scope the exception to the worker ServiceAccount rather than the whole namespace.

**Upgrade path:** Adding `cluster.enabled=true` to an existing single-node install is a destructive change — the chart switches from one StatefulSet to three separate workloads. Back up the Postgres volume and plan a maintenance window. The reverse (removing `cluster.enabled`) is equally disruptive. For incremental migration, stand up the cluster-mode install in a new namespace and cut traffic over.

### 11.7 — Load balancer and ingress per tier

Each tier exposes `/livez` (liveness) and `/readyz` (readiness) on its HTTP port `:8080`. The daemon makes `/readyz` role-aware: a webhook pod reports not-ready when its mTLS upstream (the worker relay-ingress) is unreachable; a worker pod requires the DB to be reachable; a UI pod requires the DB to be reachable for reads. Point your health targets accordingly.

Recommended ingress layout:

| Traffic | Target Service | Health target | Notes |
|---------|---------------|---------------|-------|
| End-user UI (`/ui`, `/`) | `<release>-vornik-ui:8080` | `/readyz` | Sticky sessions recommended for SSE / HTMX live updates |
| Public webhook HTTPS proxy | `<release>-vornik-webhook:8080` | `/readyz` | The proxy forwards provider callbacks; the webhook pod verifies HMAC then relays |
| Job / data-plane API (`/api/v1/…`) | `<release>-vornik-worker:8080` | `/readyz` | No sticky sessions needed |
| Internal relay-ingress (mTLS) | `<release>-vornik-worker:8443` | not exposed externally | mTLS only; not behind the public ingress |

Example Ingress snippet for the UI tier (mirror for webhook, substituting the Service name):

```yaml
ingress:
  enabled: true
  className: traefik          # RKE2 default; use nginx if you've replaced it
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: vornik.yourdomain.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - hosts: [vornik.yourdomain.com]
      secretName: vornik-tls
```

The chart's top-level `ingress` block targets the UI service when `cluster.enabled: true`. To expose the webhook and worker tiers you will need to create additional Ingress objects out-of-band (values gap — the chart currently exposes only one `ingress` block; per-tier ingress is not yet first-class in `values.yaml`).

### 11.8 — Verify the cluster

**Fleet health:**

```bash
# Confirm all tiers are running
kubectl -n vornik get pods -l app.kubernetes.io/name=vornik

# Fleet view: all nodes, their profile, version, last-seen, lease-ownership map
# Worker is a StatefulSet — exec into pod -0 (or use the pod name directly).
kubectl -n vornik exec -it statefulset/<release>-vornik-worker -- vornikctl cluster status

# Per-node self-check (run on each tier)
kubectl -n vornik exec -it <pod> -- vornikctl doctor feature cluster
```

**Webhook → worker relay path:**

```bash
# From a webhook pod: relay endpoint should be reachable
# Webhook is a Deployment — exec via deploy/<release>-vornik-webhook (no pod index suffix).
kubectl -n vornik exec -it deploy/<release>-vornik-webhook -- \
  wget -qO- --no-check-certificate \
  https://<release>-vornik-worker:8443/internal/v1/relay-health \
  && echo "relay reachable"
```

**NetworkPolicy enforcement (the DMZ invariants):**

```bash
# From a webhook pod: Postgres must be UNREACHABLE (connection refused / timeout)
kubectl -n vornik exec -it deploy/<release>-vornik-webhook -- \
  wget -qO- --timeout=5 \
  http://<release>-vornik-postgres:5432 \
  && echo "FAIL: webhook can reach postgres" \
  || echo "PASS: postgres unreachable from webhook pod"

# From a webhook pod: worker relay port must be REACHABLE
kubectl -n vornik exec -it deploy/<release>-vornik-webhook -- \
  wget -qO- --no-check-certificate --timeout=5 \
  https://<release>-vornik-worker:8443 \
  && echo "relay port open" \
  || echo "FAIL: cannot reach worker:8443"
```

`vornikctl cluster status` shows the full fleet with each node's profile, build version, and last-seen age. Version skew is expected-transient during a rolling upgrade and should resolve once all pods have rolled. The lease-ownership map shows which worker pod currently holds each singleton (autonomy, consolidation, retention, …).

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Pod stuck in `Init`, `0/1 Ready` | First-boot migrations running | Give it 2–5 minutes; watch `kubectl logs` |
| `startupProbe` fails after 5 min | Postgres unreachable or extension creation failed | `kubectl logs` for the DB error; check Postgres pod separately |
| `failed to start container` on every task | Agent image unreachable | In-pod podman can't pull; verify the image tag is pushed and the registry is reachable from the vornik pod |
| `privileged: forbidden` | Namespace enforces restricted PSA | Label the namespace permissive or switch to `runtimeMode: host-socket` |
| OOMKilled under load | Default 2Gi limit too low for concurrent warm pools | Bump `resources.limits.memory` and/or reduce `scheduler.maxConcurrentTasks` |
