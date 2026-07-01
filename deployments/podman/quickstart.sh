#!/usr/bin/env bash
#
# Vornik Community Edition — one-command quickstart.
#
#   curl -fsSL https://raw.githubusercontent.com/grinco/vornik/main/deployments/podman/quickstart.sh | bash
#
# Topology: the Vornik daemon runs ON THE HOST as a rootless
# `systemctl --user` service; only PostgreSQL+pgvector (and, in Enterprise,
# the scraper) run in containers. The daemon spawns each task's agent as a
# sibling container via your rootless podman, so the daemon, its exec
# scratch, the agent workspaces, and the agent containers all share one
# filesystem view. (The previous daemon-in-a-container design broke here:
# the host podman could not statfs bind-mount sources that existed only
# inside the daemon container.) See
# https://docs.vornik.io
#
# What it does, in order:
#   1. Installs podman + a compose provider + git (if missing).
#   2. Fetches this repo (build context + config seed).
#   3. Builds the `vornik` + `vornikctl` binaries in an ephemeral golang
#      container (no host Go toolchain) and installs them to ~/.local/bin.
#   4. Builds the agent image into your rootless podman storage.
#   5. Seeds ~/.config/vornik (config.yaml + vornik.env + configs/) and
#      ~/.local/share/vornik (data) on first run.
#   6. Brings up PostgreSQL (+ scraper on Enterprise) via podman compose.
#   7. Installs + starts the `vornik` user service (schema migrates on boot).
#   8. Waits for readiness and prints how to connect.
#
# Re-running is safe (idempotent; existing config is never clobbered).
# Tunables via environment:
#   VORNIK_REPO_URL   git URL to clone            (default: https://github.com/grinco/vornik)
#   VORNIK_REF        branch/tag to check out      (default: main)
#   VORNIK_DIR        where to place the checkout  (default: $HOME/vornik)
#   VORNIK_SKIP_FETCH 1 = use VORNIK_DIR as-is, no clone/pull (offline/dev)
#   VORNIK_HTTP_PORT  host port for the UI/API     (default: 8080)
#   POSTGRES_PORT     host port for PostgreSQL      (default: 5432)
#
if [ "${VORNIK_QUICKSTART_SOURCED:-}" = 1 ]; then
  set -eu
else
  set -euo pipefail
fi

REPO_URL="${VORNIK_REPO_URL:-https://github.com/grinco/vornik}"
REF="${VORNIK_REF:-main}"
DIR="${VORNIK_DIR:-$HOME/vornik}"
HTTP_PORT="${VORNIK_HTTP_PORT:-8080}"
PG_PORT="${POSTGRES_PORT:-5432}"
GO_IMAGE="${VORNIK_GO_IMAGE:-docker.io/library/golang:1.25}"

c_blue=$'\033[1;36m'; c_yellow=$'\033[1;33m'; c_red=$'\033[1;31m'; c_green=$'\033[1;32m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue"   "$c_off" "$*"; }
ok()   { printf '%s ok%s %s\n' "$c_green"  "$c_off" "$*"; }
warn() { printf '%s !!%s %s\n' "$c_yellow" "$c_off" "$*" >&2; }
die()  { printf '%s xx%s %s\n' "$c_red"    "$c_off" "$*" >&2; exit 1; }

# --- detection / selection helpers (sourced by quickstart_test.sh) -------
# Kept here, above the install body, so a test harness can source just
# these functions with VORNIK_QUICKSTART_SOURCED=1 without triggering the
# podman/git/build steps below.

have_compose() { podman compose version >/dev/null 2>&1 || command -v podman-compose >/dev/null 2>&1; }
is_immutable() { [ -f /run/ostree-booted ] || command -v rpm-ostree >/dev/null 2>&1; }

# install_sys <pkg...> — best-effort system package install. brew first
# (immutable-friendly, no root), then the classic distro managers. Returns
# non-zero WITHOUT dying so the caller can fall back or print guidance.
install_sys() {
  if   command -v brew    >/dev/null 2>&1; then brew install "$@"
  elif command -v dnf     >/dev/null 2>&1; then sudo dnf install -y "$@"
  elif command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y "$@"
  elif command -v zypper  >/dev/null 2>&1; then sudo zypper install -y "$@"
  elif command -v pacman  >/dev/null 2>&1; then sudo pacman -Sy --noconfirm "$@"
  else return 1
  fi
}

# Compose provider: podman's `compose` subcommand just shells out to whatever
# provider is on PATH. Prefer pip --user (no root, no reboot — works on
# immutable hosts), then pipx, then a system package.
ensure_compose() {
  have_compose && return 0
  log "Setting up a podman compose provider..."
  if command -v pipx >/dev/null 2>&1 && pipx install podman-compose >/dev/null 2>&1; then
    have_compose && return 0
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 -m pip --version >/dev/null 2>&1 || python3 -m ensurepip --user >/dev/null 2>&1 || true
    if python3 -m pip install --user podman-compose >/dev/null 2>&1; then
      export PATH="$HOME/.local/bin:$PATH"
      have_compose && return 0
    fi
  fi
  install_sys podman-compose >/dev/null 2>&1 || true
  have_compose
}

# require_safe_checkout_dir <path> — quickstart may delete and re-clone the
# repo checkout, so refuse dangerous targets after resolving path traversal.
require_safe_checkout_dir() {
  dir="$1"

  [ -n "$dir" ] || die "Refusing empty VORNIK_DIR. Set VORNIK_DIR to a dedicated checkout directory."

  case "$dir" in
    "~"|"~/"*|"."|".."|*/.|*/..|*/./*|*/../*)
      die "Refusing unsafe VORNIK_DIR value: '$dir'. Set VORNIK_DIR to a dedicated checkout directory."
      ;;
  esac

  case "$dir" in
    /*) abs="$dir" ;;
    *) abs="$(pwd -P)/$dir" ;;
  esac
  while [ "$abs" != "/" ] && [ "${abs%/}" != "$abs" ]; do
    abs="${abs%/}"
  done

  parent="${abs%/*}"
  base="${abs##*/}"
  [ -n "$parent" ] || parent="/"
  [ -n "$base" ] || die "Refusing unsafe VORNIK_DIR value: '$dir'. Set VORNIK_DIR to a dedicated checkout directory."
  [ -d "$parent" ] || die "Parent directory for VORNIK_DIR does not exist: '$parent'"

  parent_real="$(cd "$parent" && pwd -P)"
  if [ "$parent_real" = "/" ]; then
    target_real="/$base"
  else
    target_real="$parent_real/$base"
  fi
  home_real="$(cd "$HOME" && pwd -P)"

  case "$target_real" in
    "/"|"$home_real"|/*)
      if [ "$target_real" = "/" ] || [ "${target_real%/*}" = "" ]; then
        die "Refusing unsafe VORNIK_DIR value: '$dir'. Set VORNIK_DIR to a dedicated checkout directory."
      fi
      ;;
  esac

  case "$home_real/" in
    "$target_real/"*)
      die "Refusing unsafe VORNIK_DIR value: '$dir'. Set VORNIK_DIR to a dedicated checkout directory."
      ;;
  esac
}

# When sourced by quickstart_test.sh, stop here — expose the helpers above
# without running the install body (which calls sudo/podman/git/build).
if [ "${VORNIK_QUICKSTART_SOURCED:-}" = 1 ]; then return 0 2>/dev/null || exit 0; fi

[ "$(uname -s)" = "Linux" ] || die "This quickstart targets Linux (rootless podman). For macOS/Windows or k8s, see deployments/podman/README.md and docs/public/getting-started.md."
[ "$(id -u)" -ne 0 ] || die "Run as a normal (non-root) user: Vornik CE installs as a rootless 'systemctl --user' service and spawns agents via your rootless podman. (The Enterprise RPM/deb is the system-service path.)"

CONFIG_DIR="$HOME/.config/vornik"
DATA_DIR="$HOME/.local/share/vornik"
BIN_DIR="$HOME/.local/bin"
UNIT_DIR="$HOME/.config/systemd/user"

# ---------------------------------------------------------------------------
# 1. Prerequisites. Works across mutable distros (dnf/apt/zypper/pacman),
#    Homebrew, and immutable/ostree hosts (Bazzite, Silverblue, Kinoite,
#    …) where podman ships in the base image and there is no dnf. We never
#    assume a single package manager: each tool is installed only if it is
#    actually missing, and the compose provider prefers a no-root /
#    no-reboot path (pip --user) so immutable hosts don't need an
#    rpm-ostree layer + reboot just to get going.
# ---------------------------------------------------------------------------
# (detection helpers have_compose / is_immutable / install_sys /
#  ensure_compose live above the source-guard, so quickstart_test.sh can
#  exercise them in isolation.)

# Homebrew may be installed but not yet on PATH under `curl | bash`.
if ! command -v brew >/dev/null 2>&1; then
  for b in /home/linuxbrew/.linuxbrew/bin/brew "$HOME/.linuxbrew/bin/brew"; do
    [ -x "$b" ] && eval "$("$b" shellenv)" && break
  done
fi
# Core tools. On Bazzite/Silverblue these are already in the base image, so
# this loop usually no-ops — we never reinstall what's present.
missing=()
for t in podman git curl; do command -v "$t" >/dev/null 2>&1 || missing+=("$t"); done
if [ "${#missing[@]}" -gt 0 ]; then
  log "Installing: ${missing[*]}"
  if ! install_sys "${missing[@]}"; then
    if is_immutable; then
      die "Immutable OS detected and these tools are missing: ${missing[*]}.
  Layer them, reboot, then re-run:
      sudo rpm-ostree install ${missing[*]} && systemctl reboot
  Or install Homebrew (https://brew.sh) — it needs no reboot — and re-run."
    fi
    die "Could not install: ${missing[*]}. Install them with your package manager and re-run."
  fi
fi
command -v podman >/dev/null 2>&1 || die "podman is required but still not available."

ensure_compose || die "No podman compose provider available. Install one without root via:
      python3 -m pip install --user podman-compose      (or: brew install podman-compose)
  then re-run."

compose=(podman compose)
podman compose version >/dev/null 2>&1 || compose=(podman-compose)
ok "Using compose provider: ${compose[*]}"

# Keep the user service running after logout so agents survive a closed SSH
# session. Not fatal if it can't be enabled (some minimal hosts lack logind).
loginctl enable-linger "$(id -un)" >/dev/null 2>&1 || warn "could not enable login lingering — the daemon may stop when you log out (run: loginctl enable-linger $(id -un))"

# ---------------------------------------------------------------------------
# 2. Fetch the build context + config seed.
# ---------------------------------------------------------------------------
if [ "${VORNIK_SKIP_FETCH:-}" = "1" ]; then
  [ -f "$DIR/deployments/podman/deps.compose.yaml" ] || die "VORNIK_SKIP_FETCH=1 but $DIR is not a Vornik checkout."
  log "Using existing checkout (no fetch): $DIR"
elif [ -d "$DIR/.git" ]; then
  log "Updating existing checkout at $DIR"
  # Hard-reset to the remote ref rather than `pull --ff-only`. The CE publish
  # rewrites grinco/vornik history, so a returning checkout can't fast-forward
  # — the old code then warned and continued on a STALE tree, and the curled
  # (latest) quickstart would reference files that tree lacks (e.g.
  # config/vornik.host.yaml) → a confusing `cp: cannot stat` later. $DIR is a
  # throwaway build/seed checkout (real config lives in ~/.config/vornik), so
  # discarding local state here is safe. Fall back to a clean re-clone.
  if ! git -C "$DIR" fetch --depth 1 origin "$REF" --quiet \
     || ! git -C "$DIR" reset --hard FETCH_HEAD --quiet; then
    warn "could not update $DIR cleanly — re-cloning"
    require_safe_checkout_dir "$DIR"
    rm -rf "$DIR"
    git clone --depth 1 --branch "$REF" "$REPO_URL" "$DIR"
  fi
else
  log "Cloning $REPO_URL ($REF) -> $DIR"
  require_safe_checkout_dir "$DIR"
  git clone --depth 1 --branch "$REF" "$REPO_URL" "$DIR"
fi

# ---------------------------------------------------------------------------
# 3. Build the daemon + CLI in an ephemeral golang container (no host Go).
#    Output to $DIR/.bin, then install to ~/.local/bin. Module + build
#    caches persist in named volumes so re-runs are fast. label=disable
#    avoids relabeling the whole checkout (same approach the daemon uses
#    for podman ops); harmless on non-SELinux hosts.
# ---------------------------------------------------------------------------
log "Building vornik + vornikctl (first run downloads modules, ~2-3 min)..."
mkdir -p "$DIR/.bin" "$BIN_DIR"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VERSION="${VORNIK_VERSION:-$(git -C "$DIR" describe --tags --always 2>/dev/null || echo dev)}"
LDFLAGS="-X main.Version=${VERSION} -X main.BuildDate=${BUILD_DATE}"
if podman run --rm \
     --security-opt label=disable \
     -v "$DIR":/src \
     -v "$DIR/.bin":/out \
     -v vornik-go-build-cache:/root/.cache/go-build \
     -v vornik-go-mod-cache:/go/pkg/mod \
     -w /src \
     -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
     "$GO_IMAGE" \
     sh -c "go build -ldflags=\"$LDFLAGS\" -o /out/vornik ./cmd/vornik && go build -ldflags=\"$LDFLAGS\" -o /out/vornikctl ./cmd/vornikctl"; then
  install -m 0755 "$DIR/.bin/vornik"    "$BIN_DIR/vornik"
  install -m 0755 "$DIR/.bin/vornikctl" "$BIN_DIR/vornikctl"
  ok "Installed vornik + vornikctl -> $BIN_DIR"
else
  die "Build failed. Retry, or build on a host with Go: (cd $DIR && go build -o ~/.local/bin/vornik ./cmd/vornik)."
fi
case ":$PATH:" in
  *":$BIN_DIR:"*) : ;;
  *) warn "$BIN_DIR is not on your PATH. Add it, then re-open your shell:"
     warn "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc" ;;
esac

# ---------------------------------------------------------------------------
# 4. Build the agent image into the host's rootless podman storage. The
#    daemon spawns each task's agent as a sibling container from here. The
#    image's internal user is built with your uid/gid so bind-mounted
#    workspaces stay writable under rootless podman. The fully-qualified
#    localhost/ ref is required: podman refuses bare short-names
#    non-interactively, so an unqualified ref fails every job at start.
# ---------------------------------------------------------------------------
log "Building the agent image localhost/vornik-agent:latest (first run ~1-2 min)..."
if podman build -f "$DIR/images/vornik-agent/Containerfile" \
     --build-arg VORNIK_UID="$(id -u)" \
     --build-arg VORNIK_GID="$(id -g)" \
     -t localhost/vornik-agent:latest "$DIR"; then
  ok "Agent image built: localhost/vornik-agent:latest"
else
  warn "Agent image build failed — jobs will fail at container start until it exists."
  warn "  retry: podman build -f $DIR/images/vornik-agent/Containerfile -t localhost/vornik-agent:latest $DIR"
fi

# ---------------------------------------------------------------------------
# 5. Seed host config (XDG) on first run. Never clobber existing files so a
#    re-run preserves operator edits and project/swarm changes.
# ---------------------------------------------------------------------------
mkdir -p "$CONFIG_DIR/configs" "$DATA_DIR/artifacts" "$DATA_DIR/workspaces"

# Guard: the seed templates must exist in the checkout. If they don't, $DIR is
# a stale/partial checkout — fail with an actionable message instead of a raw
# `cp: cannot stat`.
for f in deployments/podman/config/vornik.host.yaml deployments/podman/vornik.env.example; do
  [ -f "$DIR/$f" ] || die "Missing $f in $DIR — the checkout looks stale/incomplete. Remove it and re-run: rm -rf '$DIR' && curl -fsSL https://get.vornik.io | bash"
done

if [ ! -f "$CONFIG_DIR/config.yaml" ]; then
  cp "$DIR/deployments/podman/config/vornik.host.yaml" "$CONFIG_DIR/config.yaml"
  ok "Seeded $CONFIG_DIR/config.yaml"
fi

if [ ! -f "$CONFIG_DIR/vornik.env" ]; then
  cp "$DIR/deployments/podman/vornik.env.example" "$CONFIG_DIR/vornik.env"
  # Stamp host-specific values into the freshly seeded env only.
  sed -i \
    -e "s|^VORNIK_RUN_AS_USER=.*|VORNIK_RUN_AS_USER=$(id -u):$(id -g)|" \
    -e "s|^VORNIK_DATABASE_PORT=.*|VORNIK_DATABASE_PORT=${PG_PORT}|" \
    "$CONFIG_DIR/vornik.env"
  ok "Seeded $CONFIG_DIR/vornik.env (add your LLM key here)"
fi

# Seed the registry tree (projects/swarms/workflows/pricing) on first run.
if [ -z "$(ls -A "$CONFIG_DIR/configs" 2>/dev/null)" ]; then
  cp -r "$DIR/configs/." "$CONFIG_DIR/configs/"
  ok "Seeded $CONFIG_DIR/configs from the repo registry"
fi

# ---------------------------------------------------------------------------
# 6. Bring up dependencies (PostgreSQL; scraper on Enterprise).
# ---------------------------------------------------------------------------
cd "$DIR/deployments/podman"
[ -f .env ] || { cp .env.example .env && ok "Created .env from .env.example"; }

log "Starting PostgreSQL + pgvector..."
VORNIK_HTTP_PORT="$HTTP_PORT" POSTGRES_PORT="$PG_PORT" \
  "${compose[@]}" -f deps.compose.yaml up -d

# scraper.compose.yaml is Enterprise-only (stripped from the CE tree).
if [ -f scraper.compose.yaml ]; then
  log "Starting the scraper (Enterprise)..."
  "${compose[@]}" -f scraper.compose.yaml up -d --build || warn "scraper failed to start — research-via-browser features will be unavailable."
fi

# ---------------------------------------------------------------------------
# 7. Install + start the daemon user service.
# ---------------------------------------------------------------------------
log "Installing the vornik user service..."
mkdir -p "$UNIT_DIR"
install -m 0644 "$DIR/deployments/podman/systemd/vornik.service" "$UNIT_DIR/vornik.service"
systemctl --user daemon-reload
systemctl --user enable --now vornik.service || die "Failed to start vornik.service. Check: journalctl --user -u vornik -e"

# ---------------------------------------------------------------------------
# 8. Wait for readiness and report.
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
  warn "Vornik did not report ready within the timeout. Check logs: journalctl --user -u vornik -e"
fi

cat <<EOF

  ${c_green}Connect${c_off}
    UI       http://localhost:${HTTP_PORT}/ui
    API      http://localhost:${HTTP_PORT}
    CLI      vornikctl doctor
    Health   curl http://localhost:${HTTP_PORT}/readyz

  ${c_green}Run tasks${c_off} — connect an LLM:
    guided   open http://localhost:${HTTP_PORT}/ui — the first-run setup guide
             (/ui/setup) tests your endpoint + key and creates a first project
    manual   edit ${CONFIG_DIR}/vornik.env    # set VORNIK_CHAT_API_KEY (+ CHAT_ENDPOINT / CHAT_MODEL)
             then  systemctl --user restart vornik

  ${c_green}Control${c_off}
    check    vornikctl doctor
    list     vornikctl project list
    (If 'vornikctl' isn't found, add ~/.local/bin to your PATH — see the note above.)

  ${c_green}Manage${c_off}
    logs     journalctl --user -u vornik -f
    restart  systemctl --user restart vornik
    stop     systemctl --user stop vornik
    deps     (cd ${DIR}/deployments/podman && ${compose[*]} -f deps.compose.yaml down)   # add -v to wipe data

EOF
