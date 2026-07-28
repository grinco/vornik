#!/usr/bin/env bash
#
# Vornik macOS installer — provisions a Lima Linux VM and runs the
# UNMODIFIED Linux quickstart.sh inside it, preserving Vornik's zero-egress
# agent-isolation invariant end-to-end across the mac<->VM boundary. See
# https://docs.vornik.io for the full
# design and the "why not a native-mac daemon" reasoning (§2).
#
# Usage:
#   bash deployments/macos/install.sh
# Re-running is idempotent: an existing 'vornik' Lima VM is reused
# (vm_exists), and the in-VM quickstart.sh is itself idempotent.
#
# Tunables via environment:
#   VORNIK_VM_CPUS     VM vCPU count                      (default: 4)
#   VORNIK_VM_MEM      VM memory, Lima --memory syntax    (default: 8GiB)
#   VORNIK_VM_DISK     VM disk, Lima --disk syntax        (default: 60GiB)
#   VORNIK_HTTP_PORT   host port for the daemon UI/API    (default: 8080)
#                        NOTE: must also match the hostPort/guestPort pinned
#                        in deployments/lima/vornik.yaml's portForwards —
#                        Lima has no CLI override for port forwards (unlike
#                        --cpus/--memory/--disk/--vm-type/--arch).
#   VORNIK_QUICKSTART_URL  override the quickstart.sh URL fetched inside the
#                        VM (default: the VORNIK_REF release's copy)
#   VORNIK_QUICKSTART_SHA256_URL  checksum URL (default: <quickstart URL>.sha256)
#   VORNIK_REF           release tag used by the nested quickstart fetch
#   VORNIK_SHIM_BIN_DIR    where to install the vornikctl shim on the mac
#                        (default: $HOME/.local/bin)
set -euo pipefail

c_blue=$'\033[1;36m'; c_yellow=$'\033[1;33m'; c_red=$'\033[1;31m'; c_green=$'\033[1;32m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue"   "$c_off" "$*"; }
ok()   { printf '%s ok%s %s\n' "$c_green"  "$c_off" "$*"; }
warn() { printf '%s !!%s %s\n' "$c_yellow" "$c_off" "$*" >&2; }
die()  { printf '%s xx%s %s\n' "$c_red"    "$c_off" "$*" >&2; exit 1; }

# --- detection / selection helpers (factored for install_test.go) ---------
# For a one-liner installer, idempotency IS the product (design §6), so
# these are plain functions rather than inlined so a test harness can
# exercise the decision each one makes.

# vm_exists <name> — true (exit 0) if Lima already knows about a VM with
# this name, in any state. Re-running install.sh must reuse it rather than
# create a duplicate.
vm_exists() {
  limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$1"
}

# pick_arch — map the mac host's `uname -m` to the guest image arch. Guest
# arch matches the host arch (no emulation tax): arm64 on Apple Silicon
# (primary/supported), amd64 on Intel (best-effort). Design §3.1.
pick_arch() {
  case "$(uname -m)" in
    arm64|aarch64) echo "arm64" ;;
    x86_64|amd64)  echo "amd64" ;;
    *) die "Unsupported mac architecture: $(uname -m) (supported: arm64, amd64)" ;;
  esac
}

# pick_backend — probe `sw_vers` for the vz macOS-13 floor (Apple
# Virtualization needs macOS >= 13); fall back to qemu on older macOS (or
# when sw_vers is unavailable/unparseable) so a pre-13 machine still
# installs, just slower. Design §3.1.
pick_backend() {
  if ! command -v sw_vers >/dev/null 2>&1; then
    echo "qemu"
    return
  fi
  local major
  major="$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)"
  case "$major" in
    ''|*[!0-9]*) echo "qemu" ;;
    *)
      if [ "$major" -ge 13 ]; then echo "vz"; else echo "qemu"; fi
      ;;
  esac
}

# --- env-default resolution -------------------------------------------------
# The VM name is fixed (not env-tunable): the vornikctl shim and
# vornik.yaml's provisioning both assume a single VM literally named
# 'vornik' (design §3.3).
VM_NAME="vornik"
# The pinned VM user (must match vornik.yaml's user: name: block) — not
# env-tunable, same reasoning as VM_NAME.
PINNED_VM_USER="ubuntu"
HTTP_PORT="${VORNIK_HTTP_PORT:-8080}"
VM_CPUS="${VORNIK_VM_CPUS:-4}"
VM_MEM="${VORNIK_VM_MEM:-8GiB}"
VM_DISK="${VORNIK_VM_DISK:-60GiB}"
REF="${VORNIK_REF:-2026.7.6}"
QUICKSTART_URL="${VORNIK_QUICKSTART_URL:-https://raw.githubusercontent.com/grinco/vornik/${REF}/deployments/podman/quickstart.sh}"
QUICKSTART_SHA256_URL="${VORNIK_QUICKSTART_SHA256_URL:-${QUICKSTART_URL}.sha256}"
SHIM_BIN_DIR="${VORNIK_SHIM_BIN_DIR:-$HOME/.local/bin}"

case "$HTTP_PORT" in
  ''|*[!0-9]*) die "VORNIK_HTTP_PORT must be a decimal port number" ;;
esac
if [ "$HTTP_PORT" -lt 1 ] || [ "$HTTP_PORT" -gt 65535 ]; then
  die "VORNIK_HTTP_PORT must be between 1 and 65535"
fi
case "$QUICKSTART_URL" in
  https://*) ;;
  *) die "VORNIK_QUICKSTART_URL must use https://" ;;
esac
case "$QUICKSTART_SHA256_URL" in
  https://*) ;;
  *) die "VORNIK_QUICKSTART_SHA256_URL must use https://" ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="$SCRIPT_DIR/../lima/vornik.yaml"

# ---------------------------------------------------------------------------
# 0. Preconditions: this installer only targets macOS. The Linux quickstart
#    stays Linux-only and macOS-unaware (design §3.2) — it never gets a
#    `uname` branch bolted in; instead the branch lives here.
# ---------------------------------------------------------------------------
[ "$(uname -s)" = "Darwin" ] || die \
  "This installer targets macOS (uname -s = Darwin). For Linux, run deployments/podman/quickstart.sh directly, or: curl -fsSL https://get.vornik.io | bash"

log "Vornik macOS installer starting (VM: $VM_NAME)"

# ---------------------------------------------------------------------------
# 1. Deps: Homebrew + Lima.
# ---------------------------------------------------------------------------
command -v brew >/dev/null 2>&1 || die "Homebrew is required (https://brew.sh) — install it, then re-run."

if ! command -v limactl >/dev/null 2>&1; then
  log "Installing Lima via Homebrew..."
  brew install lima
fi
command -v limactl >/dev/null 2>&1 || die "limactl still not on PATH after 'brew install lima'."
ok "Lima: $(limactl --version 2>/dev/null || echo present)"

# ---------------------------------------------------------------------------
# 2. Provision the VM from deployments/lima/vornik.yaml (backend + arch
#    pinned at launch, per design §3.1 — the template's own vmType is only
#    the fallback default for a direct `limactl start <template>`).
# ---------------------------------------------------------------------------
ARCH="$(pick_arch)"
BACKEND="$(pick_backend)"
ok "arch=$ARCH backend=$BACKEND (vz needs macOS >= 13; falls back to qemu otherwise)"

if vm_exists "$VM_NAME"; then
  log "Reusing existing Lima VM '$VM_NAME' (idempotent re-run) — not recreating."
  limactl start "$VM_NAME" >/dev/null 2>&1 || true
else
  [ -f "$TEMPLATE" ] || die "Lima template not found: $TEMPLATE"
  log "Provisioning Lima VM '$VM_NAME' ($BACKEND/$ARCH; ${VM_CPUS} cpus, ${VM_MEM} mem, ${VM_DISK} disk)..."
  limactl start --name vornik \
    --vm-type="$BACKEND" --arch="$ARCH" \
    --cpus="$VM_CPUS" --memory="$VM_MEM" --disk="$VM_DISK" \
    --tty=false \
    "$TEMPLATE"
  ok "Lima VM '$VM_NAME' is up."
fi

# ---------------------------------------------------------------------------
# 3. Run the unmodified Linux quickstart inside the VM, as the pinned
#    lingered VM user — NEVER root (design §3.2 step 3: the `systemd --user`
#    service quickstart installs must land under the lingered user manager).
# ---------------------------------------------------------------------------
VM_USER="$(limactl shell "$VM_NAME" -- id -un)"
[ "$VM_USER" = "$PINNED_VM_USER" ] || die "limactl shell landed as '$VM_USER' inside '$VM_NAME', not the pinned VM user '$PINNED_VM_USER' — check vornik.yaml's user: block."
ok "in-VM user: $VM_USER (matches the pinned VM user)"

log "Running the Linux quickstart inside the VM (first run builds binaries + the agent image; several minutes)..."
# The single quotes below are LOAD-BEARING and deliberate. The URLs and port
# reach the guest as `env` NAME=VALUE arguments and are expanded by the guest's
# own shell; they are never interpolated into the program text by this host
# shell. Switching to double quotes would re-introduce the command injection
# fixed in the 2026-07-25 audit (a single quote in VORNIK_QUICKSTART_URL then
# closes the quoted program and executes inside the Lima VM).
# shellcheck disable=SC2016 # intentional: expansion must happen in the guest
limactl shell "$VM_NAME" -- env \
  VORNIK_QUICKSTART_URL="$QUICKSTART_URL" \
  VORNIK_QUICKSTART_SHA256_URL="$QUICKSTART_SHA256_URL" \
  VORNIK_HTTP_PORT="$HTTP_PORT" \
  bash -c 'curl -fsSL -o /tmp/quickstart.sh -- "$VORNIK_QUICKSTART_URL" &&
    curl -fsSL -o /tmp/quickstart.sh.sha256 -- "$VORNIK_QUICKSTART_SHA256_URL" &&
    (cd /tmp && sha256sum -c quickstart.sh.sha256) &&
    chmod +x /tmp/quickstart.sh &&
    /tmp/quickstart.sh'

# ---------------------------------------------------------------------------
# 4. Install the mac-host vornikctl shim.
# ---------------------------------------------------------------------------
mkdir -p "$SHIM_BIN_DIR"
install -m 0755 "$SCRIPT_DIR/vornikctl" "$SHIM_BIN_DIR/vornikctl"
ok "Installed the vornikctl shim -> $SHIM_BIN_DIR/vornikctl"

case ":$PATH:" in
  *":$SHIM_BIN_DIR:"*) : ;;
  *)
    warn "$SHIM_BIN_DIR is not on your PATH. Add it, then re-open your shell:"
    warn "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc"
    ;;
esac

# ---------------------------------------------------------------------------
# 5. Report.
# ---------------------------------------------------------------------------
echo
cat <<EOF
  ${c_green}Connect${c_off}
    UI       http://127.0.0.1:${HTTP_PORT}/ui
    API      http://127.0.0.1:${HTTP_PORT}
    Health   curl http://127.0.0.1:${HTTP_PORT}/readyz

  ${c_green}Control${c_off}
    vornikctl status
    vornikctl logs

  ${c_green}VM lifecycle${c_off} (Lima version skew: 'limactl stop ${VM_NAME}' before 'brew upgrade lima')
    stop     limactl stop ${VM_NAME}
    start    limactl start ${VM_NAME}
    backup   vornikctl backup <path>     # do this BEFORE 'vornikctl delete --force'
    delete   vornikctl delete --force    # destroys ALL in-VM data (Postgres included)

EOF
