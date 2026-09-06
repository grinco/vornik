#!/usr/bin/env bash
#
# vornik-update.sh — safe in-place upgrade of a Community-Edition Vornik
# install created by the quickstart (deployments/podman/quickstart.sh).
#
# It mirrors the quickstart's topology exactly, so it is turnkey on a
# greenfield CE host:
#   - daemon runs ON THE HOST as a rootless `systemctl --user` service;
#   - binaries live in ~/.local/bin (vornik, vornikctl);
#   - PostgreSQL+pgvector runs in a rootless podman container (vornik-postgres);
#   - the daemon + CLI are rebuilt in an EPHEMERAL golang container (no host
#     Go toolchain), the same way the quickstart builds them.
#
# What it does, in order:
#   1. Backs up the DB (pg_dump), the current binaries, and config.yaml.
#   2. Fetches + checks out the target ref in the source checkout (~/vornik).
#   3. Rebuilds vornik + vornikctl in the golang container (version-stamped).
#   4. Smoke-checks the new binary (-version) before touching the service.
#   5. Stops the service, installs the new binaries, starts it. Because the
#      unit is Type=notify, systemd only reports "ready" AFTER the DB
#      migrations applied and health checks passed.
#   6. Verifies /readyz, prints the DB migration version bump, runs doctor.
#
# Every Vornik migration to date is additive (CREATE/ADD ... IF NOT EXISTS),
# so an upgrade does not drop data. A full DB dump is still taken every run;
# rollback commands are printed at the end.
#
# Usage:
#   ./vornik-update.sh                    # upgrade to the newest fetched tag (confirm prompt)
#   ./vornik-update.sh --ref 2026.8.0     # upgrade to a specific tag or commit
#   ./vornik-update.sh --ref origin/main  # track the tip of main instead of the newest tag
#   ./vornik-update.sh --yes              # skip the confirmation prompt (automation)
#   ./vornik-update.sh --no-build         # reuse binaries already in <repo>/.bin
#   ./vornik-update.sh --no-rebuild-images  # skip the image rebuild (see below)
#   ./vornik-update.sh --no-recreate-sidecars  # rebuild images but leave the
#                                              # scraper/broker containers on
#                                              # the old ones (see below)
#   ./vornik-update.sh --force            # rebuild+reinstall even if the checkout already matches
#   ./vornik-update.sh --check            # only report current vs. available version, then exit
#
# CONTAINER IMAGES ARE REBUILT BY DEFAULT.
#   Agent-side product code ships INSIDE the agent image — cmd/mcp-bridge,
#   cmd/agent-helper and images/vornik-agent/entrypoint.sh are baked in — so an
#   update that swaps only the daemon binary delivers half of any release that
#   changed both. That is not hypothetical: commit 356e74cd ("four agent tools
#   bypassed the per-role allowlist") changed internal/agenttools AND the agent
#   entrypoint, and every install updated with the old opt-in default got the
#   daemon half only, leaving the bypass reachable.
#
#   SINCE 2026-09-06 AN IMAGE IS PULLED WHERE THE RELEASE PUBLISHED ONE, and
#   built locally only when it did not, or when the registry cannot be reached.
#   `vornik-images -obtain` makes that decision and records which happened; this
#   script builds what it is handed. An update across a release that touched no
#   image inputs costs a digest comparison per image rather than a build.
#
#   Air-gapped hosts are unaffected: with no reachable registry they build
#   locally exactly as before. A host that already PULLED an image and later
#   loses network keeps that image rather than rebuilding over it — see
#   2026-08-28-packaged-image-provenance-design.md §S2.3.
#
#   --no-rebuild-images is honoured, but while images are stale `vornikctl
#   doctor` will report a WARNING on every run. That warning confirms the pin is
#   intentional; it is not a fault to be silenced.
#
# Tunables (env) — defaults match the quickstart:
#   VORNIK_DIR           source checkout            (default: $HOME/vornik)
#   VORNIK_BIN_DIR       install dir                (default: $HOME/.local/bin)
#   VORNIK_CONFIG        config file                (default: $HOME/.config/vornik/config.yaml)
#   VORNIK_SERVICE       user unit                  (default: vornik.service)
#   VORNIK_HTTP_PORT     UI/API port                (default: 8080)
#   VORNIK_PG_CONTAINER  postgres container         (default: vornik-postgres)
#   VORNIK_PG_DB/USER    database name / role       (default: vornik / vornik)
#   VORNIK_GO_IMAGE      build image                (default: docker.io/library/golang:1.25)
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Run from a private copy — THIS SCRIPT CHECKS OUT A NEW VERSION OF ITSELF.
#
# Step 2 runs `git -C "$REPO_DIR" checkout "$TARGET_REF"`, and this file lives
# inside $REPO_DIR, so that line rewrites the file bash is reading. Bash reads a
# script lazily and seeks back to a saved byte offset after every command; once
# the file underneath changes, it resumes at that offset inside the NEW file and
# executes whatever byte lands there.
#
# Not theoretical (2026-09-06). Every CE upgrade whose vornik-update.sh changed
# size died at the checkout — 2026.8.x (13023 B) -> 2026.9.x (17612 B) and
# 2026.9.1 -> 2026.9.2 (20487 B) both did — with a nonsense error and exit 127.
# Nothing after the checkout ran: no binary build, no image rebuild, no sidecar
# recreate, no cutover. And because the checkout HAD moved, the operator's retry
# printed "Checkout already at target commit. Nothing to do." and exited 0 — an
# install that was never updated, reporting success. That is worse than the
# half-applied state contract C3 forbids, because nothing reports it.
# (2026-08-25-image-freshness-and-rebuild-on-update-design.md §13.)
#
# The copy is what makes the file bash reads immutable for the whole run. It is
# taken BEFORE anything else so no code path can reach the checkout without it.
if [[ -z "${VORNIK_UPDATE_REEXEC:-}" && -f "${BASH_SOURCE[0]}" ]]; then
  _copy_dir="$(mktemp -d "${TMPDIR:-/tmp}/vornik-update.XXXXXX")"
  cat "${BASH_SOURCE[0]}" > "$_copy_dir/vornik-update.sh"
  chmod +x "$_copy_dir/vornik-update.sh"
  # The copy carries the full comment header, so --help still works from it.
  export VORNIK_UPDATE_REEXEC="${BASH_SOURCE[0]}" VORNIK_UPDATE_COPY_DIR="$_copy_dir"
  exec bash "$_copy_dir/vornik-update.sh" "$@"
fi
# Every later exit must take the copy with it. The one other EXIT trap in this
# script (the rebuilt-rows tempfile) re-states this cleanup rather than
# replacing it — bash keeps a single EXIT trap, so a second `trap ... EXIT`
# silently drops the first.
if [[ -n "${VORNIK_UPDATE_COPY_DIR:-}" ]]; then
  trap 'rm -rf "$VORNIK_UPDATE_COPY_DIR"' EXIT
fi

# ---------------------------------------------------------------------------
# Settings (env-overridable; defaults match deployments/podman/quickstart.sh)
# ---------------------------------------------------------------------------
REPO_DIR="${VORNIK_DIR:-$HOME/vornik}"
BIN_DIR="${VORNIK_BIN_DIR:-$HOME/.local/bin}"
CONFIG="${VORNIK_CONFIG:-$HOME/.config/vornik/config.yaml}"
SERVICE="${VORNIK_SERVICE:-vornik.service}"           # rootless *user* unit
HTTP_PORT="${VORNIK_HTTP_PORT:-8080}"
READYZ_URL="${VORNIK_READYZ_URL:-http://localhost:${HTTP_PORT}/readyz}"
GO_IMAGE="${VORNIK_GO_IMAGE:-docker.io/library/golang:1.25}"

PG_CONTAINER="${VORNIK_PG_CONTAINER:-vornik-postgres}" # rootless podman container
PG_DB="${VORNIK_PG_DB:-vornik}"
PG_USER="${VORNIK_PG_USER:-vornik}"

# ---------------------------------------------------------------------------
# Args
# ---------------------------------------------------------------------------
TARGET_REF=""
ASSUME_YES=0
DO_BUILD=1
CHECK_ONLY=0
FORCE=0
# Rebuilding images is what this script DOES, not an extra it can be asked for.
# The previous default (REBUILD_AGENT=0, opt-in via --rebuild-agent) is the
# defect this inversion fixes.
REBUILD_IMAGES=1
# A rebuilt image changes nothing already running: the scraper and broker
# sidecars keep the image they were created from until recreated (design §12,
# C5). Recreating them is part of the update; --no-recreate-sidecars opts out
# and says what is left stale.
RECREATE_SIDECARS=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ref)          TARGET_REF="${2:-}"; shift 2 ;;
    --ref=*)        TARGET_REF="${1#*=}"; shift ;;
    --yes|-y)       ASSUME_YES=1; shift ;;
    --no-build)     DO_BUILD=0; shift ;;
    --check)        CHECK_ONLY=1; shift ;;
    --force)        FORCE=1; shift ;;
    --no-rebuild-images) REBUILD_IMAGES=0; shift ;;
    --no-recreate-sidecars) RECREATE_SIDECARS=0; shift ;;
    # Retained so cron wrappers and timers carrying the old flag keep working.
    # It is now the default, so the flag is a no-op with a nudge.
    --rebuild-agent) REBUILD_AGENT_DEPRECATED=1; shift ;;
    -h|--help)      grep -E '^#( |$)' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

REBUILD_AGENT_DEPRECATED="${REBUILD_AGENT_DEPRECATED:-0}"

c_blue=$'\033[1;36m'; c_yellow=$'\033[1;33m'; c_red=$'\033[1;31m'; c_off=$'\033[0m'
log()  { printf '%s==>%s %s\n' "$c_blue"   "$c_off" "$*"; }
warn() { printf '%s !!%s %s\n' "$c_yellow" "$c_off" "$*" >&2; }
die()  { printf '%sERROR:%s %s\n' "$c_red" "$c_off" "$*" >&2; exit 1; }

pg() { podman exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
log "Preflight checks"
for t in git curl systemctl podman install; do
  command -v "$t" >/dev/null 2>&1 || die "required tool not found: $t"
done
[[ -d "$REPO_DIR/.git" ]] || die "source checkout not found at $REPO_DIR (set VORNIK_DIR, or install via the quickstart first)"
[[ -f "$CONFIG" ]]        || die "config not found at $CONFIG"
systemctl --user cat "$SERVICE" >/dev/null 2>&1 || die "user unit $SERVICE not found"
podman ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER" \
  || die "postgres container '$PG_CONTAINER' is not running (set VORNIK_PG_CONTAINER, or bring deps up: (cd $REPO_DIR/deployments/podman && podman compose -f deps.compose.yaml up -d))"

CURRENT_COMMIT="$(git -C "$REPO_DIR" rev-parse --short HEAD)"
CURRENT_DBVER="$(pg 'SELECT COALESCE(max(version),0) FROM migrations;')"

log "Fetching tags/commits from origin (non-destructive)"
git -C "$REPO_DIR" fetch --tags --prune origin >/dev/null 2>&1 || die "git fetch failed in $REPO_DIR"

# Default target = newest tag by creation date.
if [[ -z "$TARGET_REF" ]]; then
  TARGET_REF="$(git -C "$REPO_DIR" tag -l --sort=-creatordate | head -1)"
  [[ -n "$TARGET_REF" ]] || die "no tags found; pass --ref <tag-or-commit> (e.g. --ref origin/main)"
fi
git -C "$REPO_DIR" rev-parse --verify "$TARGET_REF^{commit}" >/dev/null 2>&1 \
  || die "target ref '$TARGET_REF' does not resolve to a commit"
TARGET_COMMIT="$(git -C "$REPO_DIR" rev-parse --short "$TARGET_REF^{commit}")"

echo
echo "  current : commit $CURRENT_COMMIT   (DB migration v${CURRENT_DBVER:-?})"
echo "  target  : $TARGET_REF (commit $TARGET_COMMIT)"
echo

if [[ "$CHECK_ONLY" == 1 ]]; then
  [[ "$CURRENT_COMMIT" == "$TARGET_COMMIT" ]] && log "Checkout is already at the target commit."
  exit 0
fi
if [[ "$CURRENT_COMMIT" == "$TARGET_COMMIT" && "$FORCE" != 1 ]]; then
  log "Checkout already at target commit. Nothing to do."
  warn "This compares the git checkout, not the installed binary. If you moved HEAD"
  warn "manually (e.g. 'git pull'), pass --force to rebuild + reinstall anyway."
  exit 0
fi

# Best-effort in-flight-task warning: a restart interrupts running agents.
INFLIGHT="$(pg "SELECT count(*) FROM tasks WHERE status IN ('RUNNING','LEASED');" || true)"
if [[ -n "${INFLIGHT:-}" && "$INFLIGHT" != "0" ]]; then
  warn "$INFLIGHT task(s) are RUNNING/LEASED right now — the cutover restart will interrupt them."
fi
if [[ "$ASSUME_YES" != 1 ]]; then
  read -r -p "Proceed with in-place upgrade to $TARGET_REF? [y/N] " ans
  [[ "$ans" =~ ^[Yy]$ ]] || { echo "aborted."; exit 0; }
fi

# ---------------------------------------------------------------------------
# 1. Backup (DB dump + binaries + config)
# ---------------------------------------------------------------------------
BK="$HOME/vornik-upgrade-backup-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BK"
log "Backing up to $BK"
DUMP="vornik-${PG_DB}-$(date -u +%Y%m%d).dump"
podman exec "$PG_CONTAINER" pg_dump -U "$PG_USER" -d "$PG_DB" --format=custom --file="/tmp/$DUMP"
podman cp "$PG_CONTAINER:/tmp/$DUMP" "$BK/$DUMP"
podman exec "$PG_CONTAINER" rm -f "/tmp/$DUMP"
[[ -f "$BIN_DIR/vornik" ]]    && cp -a "$BIN_DIR/vornik"    "$BK/vornik.prev"
[[ -f "$BIN_DIR/vornikctl" ]] && cp -a "$BIN_DIR/vornikctl" "$BK/vornikctl.prev"
cp -a "$CONFIG" "$BK/config.yaml"
cat > "$BK/STATE.txt" <<EOF
pre_upgrade_commit=$CURRENT_COMMIT
pre_upgrade_db_version=$CURRENT_DBVER
target_ref=$TARGET_REF
target_commit=$TARGET_COMMIT
EOF
log "Backup complete ($(du -h "$BK/$DUMP" | cut -f1) DB dump + binaries + config)"

# ---------------------------------------------------------------------------
# 2 & 3. Checkout target and rebuild in the golang container (mirrors the
#        quickstart's build exactly: same image, mounts, caches, ldflags).
# ---------------------------------------------------------------------------
git -C "$REPO_DIR" checkout --quiet "$TARGET_REF"
# Resolved OUTSIDE the DO_BUILD branch: step 3b stamps VORNIK_VERSION on every
# image it rebuilds, and rebuilding images is not conditional on rebuilding
# binaries. While this sat inside the branch, `--no-build` died at the first
# image with "VERSION: unbound variable" (set -u) — invisible until the
# re-exec fix let any run reach step 3b at all.
VERSION="$(git -C "$REPO_DIR" describe --tags --always 2>/dev/null || echo dev)"
if [[ "$DO_BUILD" == 1 ]]; then
  log "Rebuilding vornik + vornikctl in $GO_IMAGE (first run downloads modules, ~2-3 min)"
  mkdir -p "$REPO_DIR/.bin"
  BUILD_DATE="$(git -C "$REPO_DIR" log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
  LDFLAGS="-X main.Version=${VERSION} -X main.BuildDate=${BUILD_DATE}"
  podman run --rm \
    --security-opt label=disable \
    -v "$REPO_DIR":/src \
    -v "$REPO_DIR/.bin":/out \
    -v vornik-go-build-cache:/root/.cache/go-build \
    -v vornik-go-mod-cache:/go/pkg/mod \
    -w /src \
    -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
    "$GO_IMAGE" \
    sh -c "go build -ldflags=\"$LDFLAGS\" -o /out/vornik ./cmd/vornik && go build -ldflags=\"$LDFLAGS\" -o /out/vornikctl ./cmd/vornikctl && go build -o /out/vornik-images ./cmd/vornik-images" \
    || die "build failed (nothing swapped). Retry, or build on a host with Go: (cd $REPO_DIR && go build -o $BIN_DIR/vornik ./cmd/vornik)"
else
  log "--no-build: reusing existing binaries in $REPO_DIR/.bin"
  [[ -x "$REPO_DIR/.bin/vornik" && -x "$REPO_DIR/.bin/vornikctl" ]] \
    || die "$REPO_DIR/.bin/vornik{,ctl} missing; drop --no-build"
fi

# ---------------------------------------------------------------------------
# 3b. Rebuild container images that drifted from the target commit.
#
#     BEFORE the cutover, and FATAL on failure. The original script rebuilt the
#     agent AFTER installing the new binaries and merely warn()ed, so a failed
#     rebuild left a NEW daemon running an OLD image — manufacturing exactly the
#     half-applied state a rebuild is meant to prevent. Everything that can fail
#     must fail while the old install is still intact.
#
#     Which images get rebuilt comes from the manifest (internal/imagemanifest,
#     emitted by cmd/vornik-images), never a list maintained here: a hardcoded
#     list is how the cluster tags ended up with no builder at all, and how the
#     scraper and broker images ended up covered by no update path.
# ---------------------------------------------------------------------------
# Since 2026-09-06 the DECISION is Go's, not this script's.
#
# `vornik-images -obtain` resolves each deployable image, pulls by digest what
# the release published, records what this host obtained (contract C5), and
# prints only the rows that still need a LOCAL BUILD — in the same five columns
# this loop already read. What used to live here was a second implementation of
# the skip rule, and it was the WRONG one after Stage 2: it compared an image's
# revision label against `git rev-parse HEAD`, and a pulled image carries the CE
# commit it was built from, which an EE HEAD never equals. See
# 2026-08-28-packaged-image-provenance-design.md §S2.3.
rebuild_images() {
  local emitter="$REPO_DIR/.bin/vornik-images"
  [[ -x "$emitter" ]] || die "image manifest emitter missing at $emitter (re-run without --no-build)"

  local target_rev built=0
  target_rev="$(git -C "$REPO_DIR" rev-parse HEAD)"

  local rows
  rows="$(cd "$REPO_DIR" && "$emitter" -obtain)" \
    || die "obtaining container images failed — nothing swapped, the running install is untouched"

  local tag containerfile target context condition
  while IFS=$'\t' read -r tag containerfile target context condition; do
    [[ -n "$tag" ]] || continue

    log "  $tag — building (condition: $condition)"
    local -a build_args=(
      build -f "$REPO_DIR/$containerfile"
      --build-arg "VORNIK_REVISION=$target_rev"
      --build-arg "VORNIK_VERSION=$VERSION"
      --build-arg "VORNIK_UID=$(id -u)"
      --build-arg "VORNIK_GID=$(id -g)"
      -t "$tag"
    )
    # The emitter prints "-" for a single-stage image so that no column is
    # ever empty — bash's tab-IFS read collapses an empty column and shifts
    # everything after it (design §12 C8). Anything else is a real --target.
    [[ -n "$target" && "$target" != "-" ]] && build_args+=(--target "$target")
    build_args+=("$REPO_DIR/$context")

    podman "${build_args[@]}" \
      || die "image build failed for $tag — nothing swapped, the running install is untouched"
    built=$((built + 1))
    # Remember the row: the containers created from this tag are recreated
    # in step 3c, and only those.
    #
    # A PULLED image is deliberately NOT here. The only registry-pinned image is
    # the agent, whose containers are the warm pool — drained by the cutover
    # restart (design §5). The sidecars that step 3c exists for are host-built,
    # so they arrive through this loop as they always did.
    printf '%s\t%s\t%s\t%s\t%s\n' "$tag" "$containerfile" "$target" "$context" "$condition" >> "$REBUILT_ROWS"
  done <<< "$rows"

  log "Images: $built built locally (obtain decisions above)"
}
REBUILT_ROWS="$(mktemp)"
# Re-states the private-copy cleanup: bash keeps ONE EXIT trap, so this
# replaces the one set at the top rather than adding to it.
trap 'rm -f "$REBUILT_ROWS"; rm -rf "${VORNIK_UPDATE_COPY_DIR:-/nonexistent}"' EXIT

if [[ "$REBUILD_IMAGES" == 1 ]]; then
  log "Rebuilding container images that drifted from $TARGET_REF"
  if [[ "$REBUILD_AGENT_DEPRECATED" == 1 ]]; then
    warn "--rebuild-agent is deprecated: rebuilding is now the default. Drop the"
    warn "  flag from wrapper scripts at your next maintenance window."
  fi
  rebuild_images
else
  warn "--no-rebuild-images: container images are NOT being rebuilt."
  warn "  Agent-side code (mcp-bridge, agent-helper, the agent entrypoint) ships"
  warn "  INSIDE those images, so this install may end up running a different"
  warn "  release than its daemon. While they are stale, 'vornikctl doctor'"
  warn "  reports a WARNING on every run — that confirms the pin is intentional,"
  warn "  it is not a fault to be silenced."
fi

# ---------------------------------------------------------------------------
# 3c. Recreate the sidecar containers created from the images just rebuilt
#     (design §12, C5/C6). A rebuilt tag alters no existing container; the
#     scraper and broker sidecars would keep running the previous image with
#     nothing to report it — the agent pool drains at the cutover, they do
#     not. Before the cutover, because the daemon's MCP client to a recreated
#     broker reconnects only when the daemon restarts, which step 5 does; and
#     fatal, so a failure leaves old daemon + old containers + new images
#     (which image_freshness reports) rather than new daemon + old sidecars
#     (which nothing does). Recreating is not graceful — a tool call in
#     flight against that sidecar fails — but the cutover interrupts every
#     running task anyway and the in-flight warning above covers both.
# ---------------------------------------------------------------------------
if [[ "$REBUILD_IMAGES" == 1 && -s "$REBUILT_ROWS" ]]; then
  if [[ "$RECREATE_SIDECARS" == 1 ]]; then
    log "Recreating sidecar containers created from the rebuilt images"
    "$REPO_DIR/deployments/podman/recreate-sidecars.sh" < "$REBUILT_ROWS" \
      || die "sidecar recreate failed — nothing swapped; the daemon and its sidecars still run the previous release, and the rebuilt images wait (vornikctl doctor reports them)"
  else
    warn "--no-recreate-sidecars: containers created from the rebuilt images are NOT being recreated."
    warn "  They keep running the previous image until you recreate them; this is what would run:"
    "$REPO_DIR/deployments/podman/recreate-sidecars.sh" --dry-run < "$REBUILT_ROWS" || true
  fi
fi

# ---------------------------------------------------------------------------
# 4. Smoke-check the freshly built binary BEFORE touching the service.
#    A static binary that can't even print its version is broken (bad build,
#    arch mismatch); catching it here means we never swap it in.
# ---------------------------------------------------------------------------
NEW_VER_LINE="$("$REPO_DIR/.bin/vornik" -version 2>&1 | head -1 || true)"
grep -qi vornik <<<"$NEW_VER_LINE" || die "new binary failed its -version smoke check: '$NEW_VER_LINE' (nothing swapped)"
log "New binary: $NEW_VER_LINE"

# ---------------------------------------------------------------------------
# 5. Cutover: stop, install, start (Type=notify -> migrations apply on boot).
# ---------------------------------------------------------------------------
log "Stopping $SERVICE"
systemctl --user stop "$SERVICE"
log "Installing new binaries into $BIN_DIR"
install -m 0755 "$REPO_DIR/.bin/vornik"    "$BIN_DIR/vornik"
install -m 0755 "$REPO_DIR/.bin/vornikctl" "$BIN_DIR/vornikctl"

log "Starting $SERVICE (auto-applies additive migrations)"
systemctl --user start "$SERVICE"

# ---------------------------------------------------------------------------
# 6. Verify
# ---------------------------------------------------------------------------
log "Waiting for readiness"
ready=0
for _ in $(seq 1 60); do
  if curl -fsS "$READYZ_URL" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [[ "$ready" != 1 ]]; then
  warn "Service did not report ready within 60s. Recent logs:"
  journalctl --user -u "$SERVICE" -n 30 --no-pager || true
  echo
  warn "ROLLBACK:"
  warn "  systemctl --user stop $SERVICE"
  warn "  install -m0755 $BK/vornik.prev $BIN_DIR/vornik"
  warn "  install -m0755 $BK/vornikctl.prev $BIN_DIR/vornikctl"
  warn "  git -C $REPO_DIR checkout $CURRENT_COMMIT"
  warn "  systemctl --user start $SERVICE"
  die "readiness check failed"
fi

NEW_DBVER="$(pg 'SELECT max(version) FROM migrations;')"
echo
log "Upgrade complete."
echo "  /readyz : ready"
echo "  version : $("$BIN_DIR/vornikctl" version 2>&1 | head -1)"
echo "  DB migr : v${CURRENT_DBVER:-?} -> v${NEW_DBVER:-?}"
echo "  backup  : $BK"
echo
echo "Rollback if needed:"
echo "  systemctl --user stop $SERVICE"
echo "  install -m0755 $BK/vornik.prev $BIN_DIR/vornik"
echo "  install -m0755 $BK/vornikctl.prev $BIN_DIR/vornikctl"
echo "  git -C $REPO_DIR checkout $CURRENT_COMMIT"
echo "  systemctl --user start $SERVICE"
echo "  # full DB restore (only if a migration ever misbehaves):"
echo "  #   podman exec -i $PG_CONTAINER pg_restore -U $PG_USER -d $PG_DB --clean < $BK/$DUMP"
