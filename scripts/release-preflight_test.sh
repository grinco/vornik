#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[ -f "$ROOT/.goreleaser.enterprise.yaml" ] || exit 0
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/bin"
cat > "$TMP/bin/gh" <<'GH'
#!/usr/bin/env bash
# Count every call so a test can assert a transient failure was retried and a
# permanent one was NOT. A retry loop that spins on a 403 is as wrong as one
# that gives up on a 503.
echo x >> "$CALLS"
case "$*" in
  *actions/workflows*) [ "$SCENARIO" != no-ci ] && echo 123 || true ;;
  *releases/tags*)
    case "$SCENARIO" in
      published) echo '{"draft":false,"assets":[{"name":"checksums.txt.asc"},{"name":"checksums.txt"},{"name":"vornik-release.asc"},{"name":"a.rpm"},{"name":"a.deb"}]}' ;;
      partial) echo '{"draft":false,"assets":[]}' ;;
      forbidden) echo 'gh: Forbidden (HTTP 403)' >&2; exit 1 ;;
      ratelimited) echo 'gh: API rate limit exceeded (HTTP 403)' >&2; exit 1 ;;
      flaky)
        # Two 503s, then the honest 404 of a release that does not exist yet.
        # (Call 1 was the workflow-runs probe, so the 503s are calls 2 and 3.)
        if [ "$(wc -l < "$CALLS")" -lt 4 ]; then
          echo 'gh: Server Error (HTTP 503)' >&2; exit 1
        fi
        # Deliberately WITHOUT the parentheses, so the status parser is proven
        # tolerant of both message shapes: a parser that only matched "(HTTP
        # 404)" would read this as a network error and retry an answer.
        echo '{"message":"Not Found","status":"404"}'; echo 'HTTP 404' >&2; exit 1 ;;
      unreachable) echo 'gh: connection refused' >&2; exit 1 ;;
      always5xx) echo 'gh: Server Error (HTTP 503)' >&2; exit 1 ;;
      *) echo '{"message":"Not Found","status":"404"}'; echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
    esac ;;
  *) exit 2 ;;
esac
GH
chmod +x "$TMP/bin/gh"
# The retry backoff must not make the suite sleep for real.
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/sleep"
chmod +x "$TMP/bin/sleep"
export PATH="$TMP/bin:$PATH" GITHUB_REF=refs/tags/2026.9.3 GITHUB_REF_NAME=2026.9.3 GITHUB_REPOSITORY=grinco/vornik-enterprise GPG_PRIVATE_KEY=test RUNNER_TEMP="$TMP" GITHUB_OUTPUT="$TMP/out" SCENARIO=new CALLS="$TMP/calls"
: > "$CALLS"
bash "$ROOT/scripts/release-preflight.sh"
grep -q 'published=false' "$GITHUB_OUTPUT"
SCENARIO=published bash "$ROOT/scripts/release-preflight.sh"
grep -q 'published=true' "$GITHUB_OUTPUT"
for SCENARIO in no-ci partial forbidden; do
 export SCENARIO
 if bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then echo "unexpected pass: $SCENARIO"; exit 1; fi
done
export SCENARIO=new
# GoReleaser v2.18.1 rejects ent-v tags as invalid semantic versions.
if GITHUB_REF_NAME=ent-v2026.9.3 bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi
if GITHUB_REF=refs/heads/main bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi
if GPG_PRIVATE_KEY='' bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi

# --- transient API failures are retried, permanent ones are not -------------
#
# A 5xx is not an answer. Reading "no such release" out of one would let the
# run rebuild and re-publish over a release that already exists; failing on it
# with no retry strands the release on a hiccup. Both are regressions, so both
# directions are pinned here.

# Two 503s then a real 404: the run must recover and report an unpublished tag.
: > "$CALLS"; : > "$GITHUB_OUTPUT"
SCENARIO=flaky bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1 \
  || { echo 'transient 5xx was not retried'; exit 1; }
grep -q 'published=false' "$GITHUB_OUTPUT" || { echo 'flaky run did not reach the 404 answer'; exit 1; }
[ "$(wc -l < "$CALLS")" -ge 4 ] || { echo 'flaky run did not actually retry'; exit 1; }

# A 5xx that never clears must still fail — retrying is not the same as
# pretending the API answered.
: > "$CALLS"
if SCENARIO=always5xx bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then
  echo 'a permanently failing API must not be treated as an answer'; exit 1
fi
[ "$(wc -l < "$CALLS")" -ge 5 ] || { echo 'always-5xx did not exhaust its attempts'; exit 1; }

# A throttled 403 says so in its body and is retried.
: > "$CALLS"
if SCENARIO=ratelimited GH_API_ATTEMPTS=3 bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then
  echo 'rate-limited run should still fail once attempts are exhausted'; exit 1
fi
[ "$(wc -l < "$CALLS")" -ge 3 ] || { echo 'a rate-limit 403 must be retried'; exit 1; }

# A plain 403 is a permissions ANSWER. Spinning on it wastes the release's
# time and hides a misconfigured token behind a timeout.
: > "$CALLS"
if SCENARIO=forbidden bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi
[ "$(wc -l < "$CALLS")" -eq 2 ] || { echo "a permanent 403 must not be retried (calls: $(wc -l < "$CALLS"))"; exit 1; }

# A connection error carries no HTTP status at all, and is transient.
: > "$CALLS"
if SCENARIO=unreachable GH_API_ATTEMPTS=3 bash "$ROOT/scripts/release-preflight.sh" >/dev/null 2>&1; then exit 1; fi
[ "$(wc -l < "$CALLS")" -ge 3 ] || { echo 'a connection error must be retried'; exit 1; }

echo 'release preflight: PASS'
