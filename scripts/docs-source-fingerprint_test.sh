#!/usr/bin/env bash
# Full tracked source identity must change with gates as well as public prose.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if [ ! -f "$ROOT/.github/workflows/docs.yaml" ]; then
  echo 'skip: docs publishing is EE-only'
  exit 0
fi
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
git init -q "$TMP/source"
cd "$TMP/source"
git config user.name test
git config user.email test@example.invalid
mkdir -p docs/public scripts
printf 'hello\n' > docs/public/index.md
printf 'guard-v1\n' > scripts/guard.sh
git add .
git commit -qm initial
first=$(bash "$ROOT/scripts/docs-source-fingerprint.sh")
[[ "$first" =~ ^[a-f0-9]{64}$ ]]
printf '%s\n' "$first" > "$TMP/marker"
bash "$ROOT/scripts/docs-source-fingerprint.sh" --matches "$TMP/marker"
! bash "$ROOT/scripts/docs-source-fingerprint.sh" --matches "$TMP/missing"
printf 'guard-v2\n' > scripts/guard.sh
! bash "$ROOT/scripts/docs-source-fingerprint.sh" >/dev/null 2>&1
git add .
git commit -qm changed-guard
second=$(bash "$ROOT/scripts/docs-source-fingerprint.sh")
[[ "$first" != "$second" ]]
! bash "$ROOT/scripts/docs-source-fingerprint.sh" --matches "$TMP/marker"
printf 'new untracked prose\n' > docs/public/new.md
! bash "$ROOT/scripts/docs-source-fingerprint.sh" >/dev/null 2>&1
rm docs/public/new.md
printf 'ignored.md\n' > .gitignore
git add .gitignore
git commit -qm ignore
printf 'ignored prose\n' > docs/public/ignored.md
! bash "$ROOT/scripts/docs-source-fingerprint.sh" >/dev/null 2>&1
printf 'docs source fingerprint: PASS\n'
