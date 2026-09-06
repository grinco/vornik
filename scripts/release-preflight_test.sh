#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[ -f "$ROOT/.goreleaser.enterprise.yaml" ] || exit 0
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/bin"
cat > "$TMP/bin/gh" <<'GH'
#!/usr/bin/env bash
case "$*" in
  *actions/workflows*) [ "$SCENARIO" != no-ci ] && echo 123 || true ;;
  *releases/tags*)
    case "$SCENARIO" in
      published) echo '{"draft":false,"assets":[{"name":"checksums.txt.asc"},{"name":"checksums.txt"},{"name":"vornik-release.asc"},{"name":"a.rpm"},{"name":"a.deb"}]}' ;;
      partial) echo '{"draft":false,"assets":[]}' ;;
      forbidden) echo 'HTTP 403' >&2; exit 1 ;;
      *) echo '{"message":"Not Found","status":"404"}'; echo 'HTTP 404' >&2; exit 1 ;;
    esac ;;
  *) exit 2 ;;
esac
GH
chmod +x "$TMP/bin/gh"
export PATH="$TMP/bin:$PATH" GITHUB_REF=refs/tags/2026.9.3 GITHUB_REF_NAME=2026.9.3 GITHUB_REPOSITORY=grinco/vornik-enterprise GPG_PRIVATE_KEY=test RUNNER_TEMP="$TMP" GITHUB_OUTPUT="$TMP/out" SCENARIO=new
bash "$ROOT/scripts/release-preflight.sh"
grep -q 'published=false' "$GITHUB_OUTPUT"
SCENARIO=published bash "$ROOT/scripts/release-preflight.sh"
grep -q 'published=true' "$GITHUB_OUTPUT"
for SCENARIO in no-ci partial forbidden; do
 export SCENARIO
 if bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then echo "unexpected pass: $SCENARIO"; exit 1; fi
done
export SCENARIO=new
if GITHUB_REF=refs/heads/main bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi
if GPG_PRIVATE_KEY='' bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi
echo 'release preflight: PASS'
