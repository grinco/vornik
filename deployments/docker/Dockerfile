# vornik — production container images
#
# TWO final targets share one builder stage:
#
#   full  (DEFAULT)  — Ubuntu 24.04 + Podman runtime + agent-execution
#                      dependencies. For node.profile: worker or all.
#                      Requires privileged mode (or host podman socket).
#
#   thin             — Debian 12-slim, podman-free, minimal attack surface.
#                      For node.profile: ui, webhook, and future roles that
#                      do NOT run the executor. No privileged mode needed;
#                      safe to place in the DMZ webhook tier.
#
# Build commands (from repo root):
#
#   podman build -f deployments/docker/Dockerfile -t vornik:full .
#   podman build -f deployments/docker/Dockerfile --target thin -t vornik:thin .
#
# Note: the thin image cannot run worker/all roles (no podman).
# That is intentional: smaller image, no privileged mode, minimal
# attack surface for the UI/webhook tier.
#
# Two deployment modes are supported by the full image:
#
#   1. Privileged pod (default in the Helm chart)
#      securityContext.privileged: true + an emptyDir at
#      /var/lib/containers. Simplest; works on RKE2 / vanilla k8s
#      without exposing the host's podman socket.
#
#   2. Host-socket mount (advanced)
#      Mount /run/podman/podman.sock into the pod and set
#      CONTAINER_HOST=unix:///var/run/podman/podman.sock. Avoids
#      privileged mode but couples every pod to the specific node
#      running the podman service. Not recommended for multi-node
#      clusters.

# -----------------------------------------------------------------------
# Stage 1: build Go binaries (vornik daemon + vornikctl CLI + mcp-bridge)
# -----------------------------------------------------------------------
FROM docker.io/library/golang:1.25 AS builder

WORKDIR /src

# Cache dependencies in a separate layer so code edits don't bust it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG BUILD_DATE=unknown
ARG LDFLAGS="-X main.Version=${VERSION} -X main.BuildDate=${BUILD_DATE}"

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="" -o /out/vornik        ./cmd/vornik        \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="" -o /out/vornikctl      ./cmd/vornikctl

# -----------------------------------------------------------------------
# Stage 2 (thin): podman-free, minimal image for ui/webhook/future roles
#
# Debian 12-slim provides bash (needed by entrypoint.sh) without the bulk
# of Ubuntu. No podman, no container storage — this node profile never
# spawns agent containers.
#
# wget is included (tiny busybox-like binary) so compose/k8s healthchecks
# can probe the /livez liveness endpoint without curl or any heavy client.
# -----------------------------------------------------------------------
FROM docker.io/library/debian:12-slim AS thin

ENV DEBIAN_FRONTEND=noninteractive \
    VORNIK_CONFIG=/etc/vornik/config.yaml \
    VORNIK_CONFIGS_DIR=/etc/vornik/configs \
    VORNIK_DATA_DIR=/var/lib/vornik

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        tini \
        wget \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/vornik /var/lib/vornik

COPY --from=builder /out/vornik        /usr/local/bin/vornik
COPY --from=builder /out/vornikctl      /usr/local/bin/vornikctl
COPY deployments/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/vornik /usr/local/bin/vornikctl

# 8080: HTTP API + UI + /metrics (always)
# 9090: dedicated metrics server (only if metrics.enabled=true)
EXPOSE 8080 9090

VOLUME ["/var/lib/vornik"]

# tini reaps short-lived child processes cleanly.
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
CMD ["vornik"]

# -----------------------------------------------------------------------
# Stage 3 (full): runtime with podman — DEFAULT target (last stage)
#
# Ubuntu 24.04 ships a recent enough podman (>= 4.x) to work cleanly
# inside a container. fuse-overlayfs + crun are both required for
# podman-in-podman to work without privileged mode when kernels are
# new enough; we install both so either mode works.
#
# This MUST be the last stage so a plain `podman build ... -t vornik:latest .`
# (no --target) produces the full image — back-compat with Helm + existing CI.
# -----------------------------------------------------------------------
FROM docker.io/library/ubuntu:24.04 AS full

ENV DEBIAN_FRONTEND=noninteractive \
    VORNIK_CONFIG=/etc/vornik/config.yaml \
    VORNIK_CONFIGS_DIR=/etc/vornik/configs \
    VORNIK_DATA_DIR=/var/lib/vornik \
    CONTAINER_STORAGE_CONFIG=/etc/containers/storage.conf

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        podman \
        crun \
        fuse-overlayfs \
        uidmap \
        slirp4netns \
        tini \
        iproute2 \
        jq \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/vornik /var/lib/vornik /var/lib/containers /etc/containers

# Drop a minimal containers storage config that tells podman to use
# fuse-overlayfs. When the pod runs privileged on a modern kernel,
# overlayfs native can work too; fuse-overlayfs is the safe default that
# works across both modes.
RUN printf '%s\n' \
    '[storage]' \
    'driver = "overlay"' \
    'runroot = "/var/lib/containers/run"' \
    'graphroot = "/var/lib/containers/storage"' \
    '[storage.options]' \
    'mount_program = "/usr/bin/fuse-overlayfs"' \
    '[storage.options.overlay]' \
    'mountopt = "nodev,metacopy=on"' \
    > /etc/containers/storage.conf

# Default podman policy: allow any image (vornik pulls its agent image
# at runtime). Operators who want signature enforcement can override
# this file via a ConfigMap mount at /etc/containers/policy.json.
RUN printf '%s\n' \
    '{"default":[{"type":"insecureAcceptAnything"}]}' \
    > /etc/containers/policy.json

# Registries config: prefer docker.io by default; operators override
# via Helm if they use a private registry.
RUN printf '%s\n' \
    'unqualified-search-registries = ["docker.io", "quay.io"]' \
    > /etc/containers/registries.conf

COPY --from=builder /out/vornik        /usr/local/bin/vornik
COPY --from=builder /out/vornikctl      /usr/local/bin/vornikctl
COPY deployments/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/vornik /usr/local/bin/vornikctl

# 8080: HTTP API + UI + /metrics (always)
# 9090: dedicated metrics server (only if metrics.enabled=true)
EXPOSE 8080 9090

VOLUME ["/var/lib/vornik", "/var/lib/containers"]

# tini reaps the many short-lived podman / agent processes cleanly.
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
CMD ["vornik"]
