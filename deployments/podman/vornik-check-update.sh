#!/usr/bin/env bash
#
# vornik-check-update.sh — quiet "is a newer Vornik tag available?" check.
#
# Designed to run unattended from a systemd user timer. It is SILENT and
# changes nothing when already up to date. When a newer tag exists it:
#   - prints a one-line notice (captured by journald under the timer),
#   - writes a marker file ($VORNIK_UPDATE_MARKER) a login hint can surface,
#   - logs a NOTICE to the journal via `logger`.
# It never builds, stops, or upgrades anything — that is vornik-update.sh's job.
#
set -euo pipefail

REPO_DIR="${VORNIK_DIR:-$HOME/vornik}"
MARKER="${VORNIK_UPDATE_MARKER:-$HOME/.cache/vornik/update-available}"

mkdir -p "$(dirname "$MARKER")"
[[ -d "$REPO_DIR/.git" ]] || { echo "vornik repo not found at $REPO_DIR (set VORNIK_DIR)" >&2; exit 1; }

git -C "$REPO_DIR" fetch --tags --prune origin >/dev/null 2>&1 \
  || { echo "vornik-check-update: git fetch failed" >&2; exit 1; }

CURRENT="$(git -C "$REPO_DIR" rev-parse HEAD)"
LATEST_TAG="$(git -C "$REPO_DIR" tag -l --sort=-creatordate | head -1)"
[[ -n "$LATEST_TAG" ]] || { echo "vornik-check-update: no tags found" >&2; exit 1; }
LATEST_COMMIT="$(git -C "$REPO_DIR" rev-parse "${LATEST_TAG}^{commit}")"

if [[ "$CURRENT" == "$LATEST_COMMIT" ]]; then
  rm -f "$MARKER"
  exit 0
fi

MSG="Vornik update available: ${LATEST_TAG} (you are on $(git -C "$REPO_DIR" rev-parse --short HEAD)). Run: $REPO_DIR/deployments/podman/vornik-update.sh"
printf '%s\n' "$MSG"
printf '%s\n' "$MSG" > "$MARKER"
command -v logger >/dev/null 2>&1 && logger -t vornik-update "$MSG" || true
