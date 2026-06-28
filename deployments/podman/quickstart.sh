#!/usr/bin/env bash
#
# Vornik Community Edition — one-command quickstart.
#
#   curl -fsSL https://raw.githubusercontent.com/grinco/vornik/main/deployments/podman/quickstart.sh | bash
#
# What it does, in order:
#   1. Installs Podman + a Compose provider + git (if missing) via your distro's
#      package manager.
#   2. Enables the user Podman socket (and login lingering) so the Vornik daemon
#      — itself a container — can spawn agent containers as siblings on the host.
#   3. Fetches this repo (compose scaffolding + the image build context).
#   4. Brings up PostgreSQL + pgvector and the Vornik daemon with podman compose.
#      The daemon creates and migrates its schema on first boot.
#   5. Waits for readiness and prints how to connect.
#
# Re-running is safe (idempotent). Tunables via environment:
#   VORNIK_REPO_URL   git URL to clone           (default: https://github.com/grinco/vornik)
#   VORNIK_REF        branch/tag to check out     (default: main)
#   VORNIK_DIR        where to place the checkout (default: $HOME/vornik)
#   VORNIK_SKIP_FETCH 1 = use VORNIK_DIR as-is, no clone/pull (offline/dev)
#   VORNIK_HTTP_PORT  host port for the UI/API    (default: 8080)
#   POSTGRES_PORT     host port for PostgreSQL     (default: 5432)
#   PODMAN_SOCK       explicit podman socket path  (default: auto-detected)
#
set -euo pipefail

REPO_URL="${VORNIK_REPO_URL:-https://github.com/grinco/vornik}"
REF="${VORNIK_REF:-main}"
DIR="${VORNIK_DIR:-$HOME/vornik}"
HTTP_PORT="${VORNIK_HTTP_PORT:-8080}"
PG_PORT="${POSTGRES_PORT:-5432}"

c_blue=$'\033[1;36m'; c_yellow=$'\033[1;33m'; c_red=$'\033[1;31m'; c_green=$'\033[1;32m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue"   "$c_off" "$*"; }
ok()   { printf '%s ok%s %s\n' "$c_green"  "$c_off" "$*"; }
warn() { printf '%s !!%s %s\n' "$c_yellow" "$c_off" "$*" >&2; }
die()  { printf '%s xx%s %s\n' "$c_red"    "$c_off" "$*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || die "This quickstart targets Linux (rootless Podman). For macOS/Windows or k8s, see deployments/podman/README.md and docs/public/getting-started.md."

# ---------------------------------------------------------------------------
# 1. Prerequisites.
# ---------------------------------------------------------------------------
have_compose() { podman compose version >/dev/null 2>&1 || command -v podman-compose >/dev/null 2>&1; }

need=()
command -v podman >/dev/null 2>&1 || need+=(podman)
command -v git    >/dev/null 2>&1 || need+=(git)
command -v curl   >/dev/null 2>&1 || need+=(curl)
have_compose                      || need+=(podman-compose)

if [ "${#need[@]}" -gt 0 ]; then
  log "Installing prerequisites: ${need[*]}"
  sudo=""; [ "$(id -u)" -eq 0 ] || sudo="sudo"
  if   command -v dnf     >/dev/null 2>&1; then $sudo dnf install -y "${need[@]}"
  elif command -v apt-get >/dev/null 2>&1; then $sudo apt-get update && $sudo apt-get install -y "${need[@]}"
  elif command -v zypper  >/dev/null 2>&1; then $sudo zypper install -y "${need[@]}"
  elif command -v pacman  >/dev/null 2>&1; then $sudo pacman -Sy --noconfirm "${need[@]}"
  else die "No supported package manager found (dnf/apt-get/zypper/pacman). Install: ${need[*]} — then re-run."
  fi
fi
have_compose || die "A Compose provider is still unavailable. Install 'podman-compose' (or the docker compose v2 plugin) and re-run."

compose=(podman compose)
podman compose version >/dev/null 2>&1 || compose=(podman-compose)
ok "Using compose provider: ${compose[*]}"

# ---------------------------------------------------------------------------
# 2. Host config — let a container reach Podman to spawn sibling agents.
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  SCTL=(systemctl --user)
  loginctl enable-linger "$(id -un)"            >/dev/null 2>&1 || warn "could not enable login lingering — agents may stop when you log out"
  SOCK="${PODMAN_SOCK:-${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock}"
else
  SCTL=(systemctl)
  SOCK="${PODMAN_SOCK:-/run/podman/podman.sock}"
fi

# Enable + start the podman socket. A PREVIOUS podman run can leave the
# socket file behind; systemd's podman.socket then fails
# "Failed to create listening socket (...): Address already in use", the
# unit goes to 'failed', and vornik connecting to the stale file later
# gets "connection refused" (the file is present but nothing is listening).
# The old check (`[ -S "$SOCK" ]`) passed for a stale file and proceeded
# to a daemon that couldn't spawn agents. So: verify the unit is actually
# LISTENING; if it isn't, clear the failed state, remove the stale socket
# file, and restart the unit so it can bind to the now-free path.
socket_listening() { "${SCTL[@]}" is-active --quiet podman.socket 2>/dev/null; }
"${SCTL[@]}" enable podman.socket >/dev/null 2>&1 || warn "could not enable podman.socket"
"${SCTL[@]}" start podman.socket  >/dev/null 2>&1 || true
if ! socket_listening; then
  warn "podman.socket not listening; clearing a possible stale socket at $SOCK and restarting"
  "${SCTL[@]}" reset-failed podman.socket >/dev/null 2>&1 || true
  rm -f "$SOCK"
  "${SCTL[@]}" start podman.socket >/dev/null 2>&1 || warn "could not start podman.socket — start it manually: ${SCTL[*]} start podman.socket"
fi

# Liveness gate (not just file presence): a stale socket file passes
# `[ -S ]` but vornik still gets "connection refused". Wait briefly for
# socket activation, then FAIL FAST so the operator fixes the socket
# BEFORE we bring up a daemon whose runtime manager hard-fails on an
# unreachable podman (vornik can't do its job without spawning agents).
for _ in $(seq 1 20); do
  socket_listening && break
  sleep 0.5
done
if socket_listening && [ -S "$SOCK" ]; then
  ok "Podman socket live: $SOCK"
else
  die "Podman socket is not listening at $SOCK. vornik needs it to spawn agent containers. Fix it (e.g. ${SCTL[*]} restart podman.socket, or rm -f $SOCK and retry), then re-run."
fi

# ---------------------------------------------------------------------------
# 3. Fetch the compose scaffolding + image build context.
# ---------------------------------------------------------------------------
if [ "${VORNIK_SKIP_FETCH:-}" = "1" ]; then
  [ -f "$DIR/deployments/podman/podman-compose.yaml" ] || die "VORNIK_SKIP_FETCH=1 but $DIR is not a Vornik checkout."
  log "Using existing checkout (no fetch): $DIR"
elif [ -d "$DIR/.git" ]; then
  log "Updating existing checkout at $DIR"
  git -C "$DIR" pull --ff-only --quiet || warn "git pull failed — continuing with the existing checkout"
else
  log "Cloning $REPO_URL ($REF) -> $DIR"
  git clone --depth 1 --branch "$REF" "$REPO_URL" "$DIR"
fi

cd "$DIR/deployments/podman"
[ -f .env ] || { cp .env.example .env && ok "Created .env from .env.example"; }

# ---------------------------------------------------------------------------
# 4. Bring up PostgreSQL (pgvector) + the Vornik daemon.
# ---------------------------------------------------------------------------
log "Starting PostgreSQL + pgvector and Vornik (first run builds the image, ~2 min)..."
PODMAN_SOCK="$SOCK" VORNIK_HTTP_PORT="$HTTP_PORT" POSTGRES_PORT="$PG_PORT" \
  "${compose[@]}" -f podman-compose.yaml up -d --build postgres vornik

# ---------------------------------------------------------------------------
# 5. Wait for readiness and report.
# ---------------------------------------------------------------------------
log "Waiting for the daemon to become ready (schema migrates automatically)..."
ready=""
for _ in $(seq 1 120); do
  if curl -fsS "http://localhost:${HTTP_PORT}/readyz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 2
done

echo
if [ -n "$ready" ]; then
  ok "Vornik is up and ready."
else
  warn "Vornik did not report ready within the timeout. Check logs: podman logs vornik"
fi

cat <<EOF

  ${c_green}Connect${c_off}
    UI       http://localhost:${HTTP_PORT}/ui
    API      http://localhost:${HTTP_PORT}
    Health   curl http://localhost:${HTTP_PORT}/readyz

  ${c_green}Run tasks${c_off} — add an LLM key, then restart the daemon:
    edit   ${DIR}/deployments/podman/.env      # set VORNIK_CHAT_API_KEY (+ CHAT_ENDPOINT / CHAT_MODEL)
    apply  (cd ${DIR}/deployments/podman && ${compose[*]} up -d vornik)

  ${c_green}Manage${c_off}
    logs   podman logs -f vornik
    stop   (cd ${DIR}/deployments/podman && ${compose[*]} stop postgres vornik)
    down   (cd ${DIR}/deployments/podman && ${compose[*]} down)        # add -v to also wipe data

EOF
