#!/usr/bin/env bash
# Only an exact clean source tree may claim the prior publication's identity.
set -euo pipefail
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
  echo 'ERROR: commit source changes before publishing docs' >&2
  exit 2
fi
# Ignored prose can still be picked up by MkDocs; reject that hidden input.
if [ -n "$(git ls-files --others --ignored --exclude-standard docs/public)" ]; then
  echo 'ERROR: ignored files under docs/public could change the published site' >&2
  exit 2
fi
fingerprint=$(git ls-tree -r -z HEAD | sha256sum | cut -d ' ' -f1)
if [ "${1:-}" = --matches ]; then
  [ -f "${2:?marker path required}" ] && [ "$(cat "$2")" = "$fingerprint" ]
else
  printf '%s\n' "$fingerprint"
fi
