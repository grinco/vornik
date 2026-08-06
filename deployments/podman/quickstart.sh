#!/usr/bin/env bash
#
# Vornik Community Edition — one-command quickstart.
#
# Verified install (recommended) — pin a release and check the script against
# its published checksum before running it. This catches tampering in transit
# or a compromised get.vornik.io redirect; it is NOT a signature (someone who
# controls the tag could rewrite both files together), so also skim the script:
#   REF=<release>  # a tag from github.com/grinco/vornik/releases that ships quickstart.sh.sha256
#   base="https://raw.githubusercontent.com/grinco/vornik/$REF/deployments/podman"
#   curl -fsSLO "$base/quickstart.sh" && curl -fsSLO "$base/quickstart.sh.sha256"
#   sha256sum -c quickstart.sh.sha256 && VORNIK_REF="$REF" bash quickstart.sh
#
# Convenience one-liner (trusts TLS + GitHub; no checksum step). It pins to the
# release tag baked in below, NOT to a moving branch:
#   curl -fsSL https://get.vornik.io | bash
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
#   VORNIK_REF        branch/tag to check out      (default: pinned release tag; 'main' for bleeding-edge)
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
# DEFAULT_VORNIK_REF is stamped to the release tag at release/export time
# (`make quickstart-stamp-ref REF=<tag>`), so the PUBLISHED installer pins a
# concrete release rather than a moving branch. Keep it a real, recent tag in
# the repo. Override at runtime with VORNIK_REF (e.g. VORNIK_REF=main).
DEFAULT_VORNIK_REF="2026.8.1"
REF="${VORNIK_REF:-$DEFAULT_VORNIK_REF}"
DIR="${VORNIK_DIR:-$HOME/vornik}"
HTTP_PORT="${VORNIK_HTTP_PORT:-8080}"
PG_PORT="${POSTGRES_PORT:-5432}"
GO_IMAGE="${VORNIK_GO_IMAGE:-docker.io/library/golang:1.25}"

c_blue=$'\033[1;36m'; c_yellow=$'\033[1;33m'; c_red=$'\033[1;31m'; c_green=$'\033[1;32m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue"   "$c_off" "$*"; }
ok()   { printf '%s ok%s %s\n' "$c_green"  "$c_off" "$*"; }
warn() { printf '%s !!%s %s\n' "$c_yellow" "$c_off" "$*" >&2; }
die()  { printf '%s xx%s %s\n' "$c_red"    "$c_off" "$*" >&2; exit 1; }

# --- install-failure reporting (2026-07-25) ---------------------------------
# On ANY non-zero exit, print a prefilled grinco/vornik issue URL the user can
# open + submit with their own GitHub account. The installer is the only actor
# pre-install, so it carries its own (deliberately aggressive) secret scrubber.
# It posts ONLY low-risk structured context (version/platform/exit + the scrubbed
# failing command) — NOT an arbitrary log tail — and asks the user to add detail
# after reviewing it. sourceable by quickstart_test.sh (VORNIK_QUICKSTART_SOURCED=1).

# vornik_scrub — strip likely secrets AND identifiers from stdin before they
# enter a PUBLIC issue. Mirrors the Go scrubber (internal/report) so the
# installer path is no weaker than daemon-up reporting (review-20260725-7530 #3):
# secrets first, then email / IPv4 / home-&-tilde paths (whole path, tail and
# all — a surviving tail leaks project names), base64 last (after paths collapse).
vornik_scrub() {
  sed -E \
    -e 's/(Bearer|token|secret|password|passwd|api[_-]?key|access[_-]?key)([=: ]+)[^[:space:]"'"'"']+/\1\2<redacted>/Ig' \
    -e 's/eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}/<redacted-jwt>/g' \
    -e 's/\bsk-[A-Za-z0-9_-]{12,}/<redacted-key>/g' \
    -e 's/\bAKIA[0-9A-Z]{12,}/<redacted-key>/g' \
    -e 's/([?&](token|key|api_key|secret|password|access_key)=)[^&[:space:]]+/\1<redacted>/Ig' \
    -e 's/-----BEGIN[^-]+-----[^-]*-----END[^-]+-----/<redacted-pem>/g' \
    -e 's#[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}#<email>#g' \
    -e 's#\b([0-9]{1,3}\.){3}[0-9]{1,3}\b#<ip>#g' \
    -e 's#(/var)?/home/[^[:space:]/]+(/[^[:space:],;)]*)?#<path>#g' \
    -e 's#/Users/[^[:space:]/]+(/[^[:space:],;)]*)?#<path>#g' \
    -e 's#/root(/[^[:space:],;)]*)?#<path>#g' \
    -e 's#~/[^[:space:],;)]*#<path>#g' \
    -e 's#\b[A-Za-z0-9+/]{32,}={0,2}\b#<redacted-b64>#g'
}

# vornik_urlencode — percent-encoder (no jq/python dependency; awk is POSIX
# base). Everything above the source guard must parse under /bin/sh, because
# quickstart_test.sh sources this file with dash in CI — so no C-style for,
# no `+=`, no `printf -v`. The input arrives via the environment, not `awk -v`,
# which would expand backslash escapes in a failing command line; LC_ALL=C
# makes length/substr byte-wise so multibyte UTF-8 is encoded per byte, as
# RFC 3986 requires.
vornik_urlencode() {
  VORNIK_URLENCODE_IN="$1" LC_ALL=C awk '
    BEGIN {
      for (i = 0; i < 256; i++) ord[sprintf("%c", i)] = i
      s = ENVIRON["VORNIK_URLENCODE_IN"]
      n = length(s)
      for (i = 1; i <= n; i++) {
        c = substr(s, i, 1)
        if (c ~ /^[A-Za-z0-9._~-]$/) printf "%s", c
        else printf "%%%02X", ord[c]
      }
    }'
}

# vornik_replace_all <haystack> <needle> <replacement> — literal (non-regex)
# replace-all. sed would treat the needle as a BRE, and hostnames carry dots.
vornik_replace_all() {
  vra_rest="$1" vra_needle="$2" vra_repl="$3" vra_out=''
  if [ -z "$vra_needle" ]; then printf '%s' "$vra_rest"; return 0; fi
  while :; do
    case "$vra_rest" in
      *"$vra_needle"*)
        vra_pre="${vra_rest%%"$vra_needle"*}"
        vra_out="$vra_out$vra_pre$vra_repl"
        vra_rest="${vra_rest#"$vra_pre$vra_needle"}"
        ;;
      *) printf '%s' "$vra_out$vra_rest"; return 0 ;;
    esac
  done
}

# report_install_failure <exit-code> — no-op on success; else print the URL.
report_install_failure() {
  local ec="${1:-0}" os arch cmd hn title body url
  case "$ec" in ''|0) return 0 ;; esac
  os="$(uname -s 2>/dev/null || echo unknown)"
  arch="$(uname -m 2>/dev/null || echo unknown)"
  cmd="$(printf '%s' "${BASH_COMMAND:-<unknown>}" | vornik_scrub)"
  # This machine's hostname is only known at runtime, so strip it here (after
  # the stdin scrubber): a literal replace, hostnames carry no glob chars.
  hn="$(uname -n 2>/dev/null || hostname 2>/dev/null || true)"
  if [ -n "$hn" ]; then cmd="$(vornik_replace_all "$cmd" "$hn" '<host>')"; fi
  # This installer only ever clones grinco/vornik, so an install failure is a
  # Community Edition failure by construction — nothing here can be EE. Marking it
  # (body AND title, matching `vornikctl report`) is what lets triage tell which
  # build a reporter was running (operator report 2026-08-03: a CE customer's
  # report named neither edition nor build).
  title="[CE] Install failure: exit ${ec}"
  body="vornik quickstart install failed.

- version (REF): ${REF:-unknown}
- edition: community (CE)
- platform: ${os}/${arch}
- exit code: ${ec}
- failing command: ${cmd}

<Add what you were doing and any error output here. This issue is PUBLIC — review your text for secrets, tokens, hostnames, and file paths before submitting.>"
  url="https://github.com/grinco/vornik/issues/new?labels=$(vornik_urlencode 'bug,install')&title=$(vornik_urlencode "$title")&body=$(vornik_urlencode "$body")"
  printf '\n%s xx%s installation failed (exit %s).\n' "${c_red:-}" "${c_off:-}" "$ec" >&2
  printf 'Report it — this opens a prefilled, anonymized GitHub issue you review + submit:\n  %s\n\n' "$url" >&2
}

# Fire the hook on any non-zero exit — but NOT when merely sourced by the test.
if [ "${VORNIK_QUICKSTART_SOURCED:-}" != 1 ]; then
  trap 'report_install_failure "$?"' EXIT
fi

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

# configure_anonymous_telemetry <config-path> <interactive:0|1>
# Applies the first-install choice. Enabled is the implicit default, so only
# opt-out writes YAML. Environment values make automation deterministic.
configure_anonymous_telemetry() {
  local config_path="$1" interactive="${2:-0}" choice="${VORNIK_TELEMETRY:-}" normalized
  normalized="$(printf '%s' "$choice" | tr '[:upper:]' '[:lower:]')"
  case "$normalized" in
    0|false|no|off)
      printf '\n# Anonymous lifecycle telemetry opt-out (default is enabled).\ntelemetry:\n  enabled: false\n' >>"$config_path"
      ok "Anonymous telemetry disabled in $config_path"
      return 0
      ;;
    1|true|yes|on)
      return 0
      ;;
    "")
      ;;
    *)
      warn "Invalid VORNIK_TELEMETRY value; telemetry is disabled."
      printf '\n# Anonymous lifecycle telemetry opt-out (invalid environment value; fail closed).\ntelemetry:\n  enabled: false\n' >>"$config_path"
      return 0
      ;;
  esac

  if [ "$interactive" != 1 ]; then
    printf '%s\n' "Anonymous telemetry is enabled. Set VORNIK_TELEMETRY=off now, or telemetry.enabled: false later, to disable it."
    return 0
  fi

  while :; do
    cat >/dev/tty <<'EOF'

Anonymous usage telemetry
Vornik reports successful installs and project creations. No IDs, names,
paths, prompts, config, keys, or model/provider details are sent.
Your IP is visible while telemetry.vornik.io handles the request, but is not
included in the payload and must not be retained by the service.

Send anonymous telemetry? [Y/n/s=show sample]
EOF
    IFS= read -r choice </dev/tty || choice=""
    case "$(printf '%s' "$choice" | tr '[:upper:]' '[:lower:]')" in
      ""|y|yes) return 0 ;;
      n|no)
        printf '\n# Anonymous lifecycle telemetry opt-out (default is enabled).\ntelemetry:\n  enabled: false\n' >>"$config_path"
        ok "Anonymous telemetry disabled in $config_path"
        return 0
        ;;
      s|sample)
        cat >/dev/tty <<'EOF'
Example URL:
https://telemetry.vornik.io/v1/collect.json?e=install_succeeded&sv=1&v=2026.7.4&os=linux&arch=amd64&source=quickstart
Example body:
{"schema_version":1,"event":"install_succeeded","vornik_version":"2026.7.4","platform":{"os":"linux","arch":"amd64"},"source":"quickstart"}
After install, run: vornikctl telemetry sample
EOF
        ;;
      *) warn "Please answer y, n, or s." ;;
    esac
  done
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

# print_success_footer — post-install guidance (F5): a failing first task
# should point users at diagnostics + reporting, not leave them stranded.
print_success_footer() {
  printf '\nIf a task fails, run:\n'
  printf '  vornikctl doctor   # checks agent-LLM topology, image uid, upstream key, +more\n'
  printf '  vornikctl report   # opens an anonymized issue you review + submit\n'
}

# ensure_linger — verify `loginctl enable-linger` actually took (F6). The
# unprivileged call can be silently denied on some hosts; when that happens
# linger stays off and the daemon (a `systemctl --user` unit) dies the moment
# no login session remains. Escalate to sudo before giving up.
ensure_linger() {
  local u; u="$(id -un)"
  if [ "$(loginctl show-user "$u" --property=Linger 2>/dev/null)" = "Linger=yes" ]; then
    return 0
  fi
  loginctl enable-linger "$u" >/dev/null 2>&1 || true
  if [ "$(loginctl show-user "$u" --property=Linger 2>/dev/null)" != "Linger=yes" ]; then
    # Unprivileged enable was denied on this host; escalate.
    sudo loginctl enable-linger "$u" >/dev/null 2>&1 || true
  fi
  if [ "$(loginctl show-user "$u" --property=Linger 2>/dev/null)" != "Linger=yes" ]; then
    warn "could not enable login lingering — the daemon and Postgres will stop when you log out. Run: sudo loginctl enable-linger $u"
  fi
}

# enable_podman_restart — Postgres autostarts after a reboot (F7). Rootless
# `restart: always` (deps.compose.yaml) recovers crashes but NOT host
# reboots; the user's podman-restart.service (with linger from
# ensure_linger) restarts restart-policy containers at boot. Guard on the
# unit existing so hosts without it warn instead of hard-failing.
enable_podman_restart() {
  if systemctl --user list-unit-files podman-restart.service >/dev/null 2>&1; then
    systemctl --user enable podman-restart.service >/dev/null 2>&1 \
      || warn "could not enable podman-restart.service — Postgres may not autostart after a reboot (run: systemctl --user enable podman-restart.service)"
  else
    warn "podman-restart.service not found — Postgres may not autostart after a reboot."
  fi
}

# When sourced by quickstart_test.sh, stop here — expose the helpers above
# without running the install body (which calls sudo/podman/git/build).
if [ "${VORNIK_QUICKSTART_SOURCED:-}" = 1 ]; then return 0 2>/dev/null || exit 0; fi

# macOS handoff: this script is the get.vornik.io entry point for BOTH OSes
# (the redirect serves it for every path; the OS is differentiated HERE, not by
# URL). A native-mac daemon can't preserve zero-egress, so macOS runs the whole
# stack in a Lima Linux VM — hand off to that installer, which provisions the VM
# and runs THIS quickstart inside it (where uname=Linux, so the guard below is a
# no-op and the Linux install proceeds — no loop). See
# https://docs.vornik.io
if [ "$(uname -s)" = "Darwin" ]; then
  mac_base="${VORNIK_INSTALL_BASE:-https://raw.githubusercontent.com/grinco/vornik/${REF}/deployments}"
  echo "vornik: macOS detected → handing off to the Lima-VM installer (${mac_base}/macos/install.sh)"
  curl -fsSL "${mac_base}/macos/install.sh" | VORNIK_REF="${REF}" bash
  exit "$?"
fi
[ "$(uname -s)" = "Linux" ] || die "This quickstart targets Linux or macOS (rootless podman / Lima VM). For Windows or k8s, see deployments/podman/README.md and docs/public/getting-started.md."
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
# session. Linger is load-bearing (without it nothing autostarts), so verify
# it actually took and escalate to sudo if the unprivileged call was denied.
ensure_linger

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
    git clone --depth 1 --branch "$REF" "$REPO_URL" "$DIR" \
    || die "Could not check out '$REF' from $REPO_URL. If a release was just cut, its tag may not be published yet — retry in a moment, or set VORNIK_REF=main to install from latest source."
  fi
else
  log "Cloning $REPO_URL ($REF) -> $DIR"
  require_safe_checkout_dir "$DIR"
  git clone --depth 1 --branch "$REF" "$REPO_URL" "$DIR" \
    || die "Could not check out '$REF' from $REPO_URL. If a release was just cut, its tag may not be published yet — retry in a moment, or set VORNIK_REF=main to install from latest source."
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

# Operator shell fragment (T-1089). vornikctl runs the daemon's full config load
# and validation but resolves paths + secrets from the INVOKING SHELL, not from
# the systemd unit — so in a plain shell commands like `vornikctl backup` fail on
# unrelated rules purely for want of the daemon's environment. Generate the
# fragment; contains only non-secret paths plus a loop sourcing the 0600 secret
# files. We ADVISE rather than edit any rc: this script is curl|bash'd, and
# silently rewriting a user's shell rc is not ours to do.
if [ -x "$DIR/scripts/gen-shell-env.sh" ] || [ -r "$DIR/scripts/gen-shell-env.sh" ]; then
  if bash "$DIR/scripts/gen-shell-env.sh" "$CONFIG_DIR" "$DATA_DIR" "$CONFIG_DIR/shell-env.sh" >/dev/null 2>&1; then
    ok "Wrote $CONFIG_DIR/shell-env.sh (makes vornikctl resolve the same config as the daemon)"
    log "  To use it in every shell, add this line to your shell rc:"
    log "    [ -r \"$CONFIG_DIR/shell-env.sh\" ] && . \"$CONFIG_DIR/shell-env.sh\""
  else
    warn "Could not generate $CONFIG_DIR/shell-env.sh — vornikctl may need VORNIK_CONFIG set by hand."
  fi
fi

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

new_install=""
if [ ! -f "$CONFIG_DIR/config.yaml" ]; then
  cp "$DIR/deployments/podman/config/vornik.host.yaml" "$CONFIG_DIR/config.yaml"
  new_install=1
  ok "Seeded $CONFIG_DIR/config.yaml"
  telemetry_interactive=0
  if [ -t 1 ] && [ -c /dev/tty ] && [ -r /dev/tty ] && [ -w /dev/tty ]; then
    telemetry_interactive=1
  fi
  configure_anonymous_telemetry "$CONFIG_DIR/config.yaml" "$telemetry_interactive"
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

enable_podman_restart

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
  if [ -n "$new_install" ]; then
    "$BIN_DIR/vornikctl" telemetry emit-install --source quickstart >/dev/null 2>&1 || true
  fi
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

print_success_footer
