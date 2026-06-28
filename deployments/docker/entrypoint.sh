#!/usr/bin/env bash
# entrypoint.sh — wraps vornik with container-startup plumbing.
#
# Responsibilities:
#   1. Ensure $VORNIK_DATA_DIR exists and is writable.
#   2. If the operator mounted a ConfigMap-only config (read-only), copy
#      it into a writable scratch location so vornik's "config reload"
#      path (vornikctl reload) doesn't fail on the read-only filesystem.
#   3. If CONTAINER_HOST is set (host-socket mode), verify connectivity
#      before starting vornik so the pod fails fast on a broken socket.
#   4. exec into vornik (or whatever CMD the user passed — useful for
#      debug `bash` runs).

set -euo pipefail

: "${VORNIK_CONFIG:=/etc/vornik/config.yaml}"
: "${VORNIK_CONFIGS_DIR:=/etc/vornik/configs}"
: "${VORNIK_DATA_DIR:=/var/lib/vornik}"

# Data dir is mounted from a PVC; make sure subdirs exist with
# permissions the daemon can write to. vornik creates them on demand
# too, but pre-creating avoids a log line on first boot.
mkdir -p \
    "${VORNIK_DATA_DIR}/artifacts" \
    "${VORNIK_DATA_DIR}/tasks" \
    "${VORNIK_DATA_DIR}/workspaces"

# If the main config is on a read-only mount (ConfigMap projected at
# /etc/vornik/config.yaml), copy it to a writable scratch location and
# point vornik at the copy. Avoids "readonly filesystem" errors from any
# in-place config updates vornikctl might attempt.
if [[ -f "${VORNIK_CONFIG}" && ! -w "${VORNIK_CONFIG}" ]]; then
    cp "${VORNIK_CONFIG}" "${VORNIK_DATA_DIR}/config.yaml"
    export VORNIK_CONFIG="${VORNIK_DATA_DIR}/config.yaml"
fi

# Host-socket mode sanity check. When CONTAINER_HOST is set, the user
# opted out of in-pod podman and is relying on a mounted socket.
# Verify the socket is reachable before starting vornik, otherwise the
# first task execution will fail with an opaque "connection refused"
# deep in the runtime manager's call stack. The quickstart install
# (deployments/podman/quickstart.sh) gates on the socket being LIVE
# before bringing the daemon up, so this probe passes in the install
# flow; exit 1 here is the loud fail-fast for a genuinely broken socket.
if [[ -n "${CONTAINER_HOST:-}" ]]; then
    echo "[entrypoint] CONTAINER_HOST=${CONTAINER_HOST} — probing podman socket..."
    if ! podman --remote info >/dev/null 2>&1; then
        echo "[entrypoint] ERROR: podman socket at ${CONTAINER_HOST} is not reachable." >&2
        echo "[entrypoint] Check the hostPath mount and that the node runs podman.service" >&2
        echo "[entrypoint] (rootless: \$XDG_RUNTIME_DIR/podman/podman.sock; rootful: /run/podman/podman.sock)." >&2
        exit 1
    fi
    echo "[entrypoint] podman socket OK."
fi

# If the user passed the default "vornik" command (common case), exec
# the binary directly. Otherwise run their command (bash / vornikctl /
# whatever) — useful for `kubectl exec` debugging.
if [[ "${1:-vornik}" == "vornik" ]]; then
    exec /usr/local/bin/vornik
fi

exec "$@"
