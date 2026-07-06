#!/usr/bin/env bash
# Provision PageDrop from upstream at a pinned ref. Idempotent; re-run on
# rebuild to re-pull and rebuild the host-service image. No source or
# Dockerfile is vendored into this repo — this script is the only link to
# upstream. Bump the version by editing PAGEDROP_REF and re-running.
set -euo pipefail

REPO_URL="https://github.com/grinco/PageDrop"
CHECKOUT="${PAGEDROP_CHECKOUT:-/opt/vornik/pagedrop}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REF="$(tr -d '[:space:]' < "${SCRIPT_DIR}/PAGEDROP_REF")"

if [ -z "$REF" ]; then echo "PAGEDROP_REF is empty" >&2; exit 1; fi

echo ">> PageDrop sync: ref=${REF} checkout=${CHECKOUT}"

if [ ! -d "${CHECKOUT}/.git" ]; then
  git clone "${REPO_URL}" "${CHECKOUT}"
fi

git -C "${CHECKOUT}" fetch --tags origin
git -C "${CHECKOUT}" checkout --detach "${REF}"
git -C "${CHECKOUT}" reset --hard "${REF}"
git -C "${CHECKOUT}" clean -fdx -e node_modules

# Deps for both the host image build context and the daemon-spawned MCP
# server (tsx is a devDependency → installed by npm ci).
( cd "${CHECKOUT}" && npm ci )

SHORT="$(git -C "${CHECKOUT}" rev-parse --short HEAD)"
echo ">> building localhost/pagedrop-host:${SHORT} from upstream Dockerfile"
podman build -t "localhost/pagedrop-host:${SHORT}" \
             -t "localhost/pagedrop-host:current" \
             -f "${CHECKOUT}/Dockerfile" "${CHECKOUT}"

echo ">> PageDrop sync complete: image localhost/pagedrop-host:current (${SHORT})"
