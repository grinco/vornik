#!/usr/bin/env bash
# export-publish-excl-selftest.sh — prove the PUBLISH preserve-list in
# export-public-ce.sh preserves the public repo's ROOT files and nothing else.
#
# Regression: the list used unanchored basenames (--exclude='README.md'), and an
# rsync pattern with no slash matches at ANY depth. Every nested README.md /
# Makefile / LICENSE was therefore frozen in grinco/vornik at whatever the
# initial recreate wrote. It surfaced as CE CI red on
# TestCompanionPlugin_ReadmeAdvertisesEverySkill (2026-07-26): the companion
# plugin README still said "One skill" months after the bundle grew to four,
# and contrib/codex-companion/README.md had never reached the public repo at all.
#
# The list is consumed via `--print-publish-excludes` so this test exercises the
# EXACT patterns the sync uses — it cannot drift from the real script.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
EXPORT="$SCRIPT_DIR/export-public-ce.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
# Takes the message then the assertion AS A COMMAND: a bare `[ ... ]` that
# evaluated false would abort the whole script under `set -e` before it could be
# reported, which would have hidden exactly the failures this test exists to show.
chk() { local msg="$1"; shift; if "$@"; then echo "  ok   $msg"; else echo "  FAIL $msg"; fail=1; fi; }

mapfile -t EXCL < <("$EXPORT" --print-publish-excludes)
[ "${#EXCL[@]}" -gt 0 ] || { echo "--print-publish-excludes produced nothing" >&2; exit 2; }

# Every pattern must be anchored: unanchored ones match at any depth, which is
# the whole bug. Checked directly so a newly-added bare basename fails here even
# if no fixture below happens to cover that filename.
for p in "${EXCL[@]}"; do
  case "$p" in
    --exclude=/*) ;;
    *) echo "  FAIL unanchored publish exclude: $p (needs a leading '/')"; fail=1 ;;
  esac
done

# SRC = the freshly exported EE tree. DST = the public repo clone.
SRC="$TMP/src"; DST="$TMP/dst"
mkdir -p "$SRC/contrib/claude-code-companion" "$SRC/contrib/codex-companion" \
         "$SRC/deployments/podman" "$SRC/internal/architecture" \
         "$SRC/.github/workflows" "$SRC/services/scraper"
mkdir -p "$DST/contrib/claude-code-companion" "$DST/internal/architecture" \
         "$DST/.github/workflows"

# Root files: the public repo's copies must survive the sync.
for f in README.md LICENSE Makefile CLA.md CODE_OF_CONDUCT.md; do
  echo "ee-root" > "$SRC/$f"; echo "public-root" > "$DST/$f"
done
echo "ee-ci"     > "$SRC/.github/workflows/ci.yaml"
echo "public-ci" > "$DST/.github/workflows/ci.yaml"
echo "ee-law"     > "$SRC/internal/architecture/import_law_test.go"
echo "public-law" > "$DST/internal/architecture/import_law_test.go"

# Nested same-named files: these MUST sync (updated, and created when absent).
echo "four skills"   > "$SRC/contrib/claude-code-companion/README.md"
echo "One skill"     > "$DST/contrib/claude-code-companion/README.md"
echo "codex readme"  > "$SRC/contrib/codex-companion/README.md"
echo "podman readme" > "$SRC/deployments/podman/README.md"
echo "scraper make"  > "$SRC/services/scraper/Makefile"

rsync -a --delete --exclude='.git/' "${EXCL[@]}" "$SRC/" "$DST/"

for f in LICENSE Makefile CLA.md CODE_OF_CONDUCT.md; do
  chk "root $f preserved" grep -qx 'public-root' "$DST/$f"
done
# The root README is deliberately NOT preserved: it syncs from
# scripts/public-ce-templates/README.md. Preserving it is what let the public
# landing page drift behind the maintained template, including a security-relevant
# install instruction. If this assertion is ever flipped back, that drift returns.
chk "root README.md SYNCS (no longer frozen — see PUBLISH_EXCL)" \
  grep -qx 'ee-root' "$DST/README.md"
chk "public CI workflow preserved" grep -qx 'public-ci' "$DST/.github/workflows/ci.yaml"
chk "injected import-law test preserved" \
  grep -qx 'public-law' "$DST/internal/architecture/import_law_test.go"

chk "contrib/claude-code-companion/README.md syncs (the 2026-07-26 regression)" \
  grep -qx 'four skills' "$DST/contrib/claude-code-companion/README.md"
chk "contrib/codex-companion/README.md reaches a tree that lacked it" \
  test -f "$DST/contrib/codex-companion/README.md"
chk "deployments/podman/README.md syncs" test -f "$DST/deployments/podman/README.md"
chk "nested Makefile syncs" test -f "$DST/services/scraper/Makefile"

echo ">> publish preserve-list: $( [ "$fail" -eq 0 ] && echo CLEAN || echo FAILED )"
exit "$fail"
