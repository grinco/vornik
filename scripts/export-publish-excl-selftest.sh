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

# --- 2026-09-23: a PRESERVED path that the destination does NOT have ---------
#
# THE INCIDENT. PUBLISH_EXCL is a PRESERVE-list: with `rsync --delete`, an
# --exclude means "do not sync this path AND do not delete it there", so an
# operator edit in the public repo survives. That is correct — and it assumes
# the destination ALREADY HAS the file.
#
# .github/workflows/codeql.yml and .github/codeql/codeql-config.yml were added
# to the templates on 2026-08-15 to move the public repo to CodeQL advanced
# setup and suppress 63 verified-sanitised path-injection alerts. They are on
# the preserve-list, and grinco/vornik never had them — so every sync since has
# faithfully preserved their ABSENCE. Default setup was switched off the same
# day, the advanced workflow never arrived, and the public repo went 38 days
# with NO code scanning while 66 stale alerts stayed open.
#
# The export's structural check asserted both files exist — in the EXPORT tree,
# which is not where they have to be. Its own comment predicted the outcome:
# "the config without the workflow would be a file nothing reads while scanning
# silently stops."
#
# So a preserved path missing at the destination must be CREATED, not preserved.
echo "ee-codeql-workflow" > "$SRC/.github/workflows/codeql.yml"
mkdir -p "$SRC/.github/codeql"
echo "ee-codeql-config"   > "$SRC/.github/codeql/codeql-config.yml"
# DST deliberately has neither.
rm -f "$DST/.github/workflows/codeql.yml" "$DST/.github/codeql/codeql-config.yml"

rsync -a --delete --exclude='.git/' "${EXCL[@]}" "$SRC/" "$DST/"
# The create-half, invoked through the export script's own entry point so this
# test cannot drift from what the sync runs.
"$EXPORT" --restore-absent-preserved "$SRC" "$DST" >/dev/null

for f in .github/workflows/codeql.yml .github/codeql/codeql-config.yml; do
  if [ ! -f "$DST/$f" ]; then
    echo "  FAIL preserved-but-absent path was never created at the destination: $f"
    echo "       (this is the 2026-08-15 CodeQL gap: scanning stopped for 38 days)"
    fail=1
  else
    echo "  ok   preserved-but-absent path is created at the destination: $f"
  fi
done

# And an operator edit to one of them must still survive a later sync.
echo "operator-edited" > "$DST/.github/codeql/codeql-config.yml"
rsync -a --delete --exclude='.git/' "${EXCL[@]}" "$SRC/" "$DST/"
"$EXPORT" --restore-absent-preserved "$SRC" "$DST" >/dev/null
if [ "$(cat "$DST/.github/codeql/codeql-config.yml")" = "operator-edited" ]; then
  echo "  ok   an operator edit at the destination still survives the sync"
else
  echo "  FAIL preserve broke: the operator's codeql-config.yml edit was overwritten"
  fail=1
fi

exit "$fail"
