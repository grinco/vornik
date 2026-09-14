#!/usr/bin/env bash
# Self-test for wait-ce-ci.sh — the gate that reads whether the CE export's own
# CI went green on the public repo.
#
# It exists because RELEASE.md's "confirm the CE ci run goes green" was a
# sentence and not a mechanism: public main was red across SIX consecutive
# exports (2026-09-07..09) and nobody looked. The three outcomes it
# distinguishes are the whole point, so each gets a case here — in particular
# UNDETERMINED must never be reported as green, which is the failure class this
# codebase keeps retiring.
#
# Drives the script through the CE_CI_FETCH seam with a stub that prints canned
# API responses, so it needs no network and no token. What the stub does NOT
# cover is whether curl itself works; what it does cover is every branch of the
# verdict logic, which is the half that can be wrong in an interesting way. The
# stub also records the URL it was asked for, so the query cannot silently stop
# naming the SHA.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/wait-ce-ci.sh"
failures=0
SHA=abc1234

command -v jq >/dev/null 2>&1 || { echo "FAIL: jq is required for this test" >&2; exit 1; }

# stub_fetch writes a fetcher that prints the given JSON for any URL and logs
# the URL it was given.
stub_fetch() {
  local json="$1" dir
  dir="$(mktemp -d)"
  printf '%s' "$json" > "$dir/body.json"
  cat > "$dir/fetch" <<STUB
#!/usr/bin/env bash
printf '%s\\n' "\$1" >> "$dir/urls"
cat "$dir/body.json"
STUB
  chmod +x "$dir/fetch"
  printf '%s' "$dir"
}

LAST_URLS=""
run_case() {
  local name="$1" json="$2" want="$3" want_text="${4:-}" dir out code
  dir="$(stub_fetch "$json")"
  out="$(CE_CI_FETCH="$dir/fetch" CE_CI_TIMEOUT=1 CE_CI_INTERVAL=1 bash "$script" "$SHA" 2>&1)"
  code=$?
  LAST_URLS="$dir/urls"
  if [ "$code" -ne "$want" ]; then
    echo "FAIL: $name — exit $code, want $want; output: $out"
    failures=$((failures+1))
    return
  fi
  if [ -n "$want_text" ] && [[ "$out" != *"$want_text"* ]]; then
    echo "FAIL: $name — output missing '$want_text'; got: $out"
    failures=$((failures+1))
    return
  fi
  echo "PASS: $name"
}

run_case "green run exits 0" \
  '{"workflow_runs":[{"status":"completed","conclusion":"success","html_url":"https://example/run/1"}]}' \
  0 "GREEN"

# THE CASE THIS SCRIPT EXISTS FOR.
run_case "failed run exits 1" \
  '{"workflow_runs":[{"status":"completed","conclusion":"failure","html_url":"https://example/run/2"}]}' \
  1 "failure"

run_case "cancelled run exits 1" \
  '{"workflow_runs":[{"status":"completed","conclusion":"cancelled","html_url":"https://example/run/3"}]}' \
  1 "cancelled"

# A run that never finishes is NOT CHECKED. Reporting it as green would make
# this gate a control that cannot tell "examined and clean" from "never
# examined" — which reports the first and means the second.
run_case "still running at the deadline is undetermined" \
  '{"workflow_runs":[{"status":"in_progress","conclusion":null,"html_url":"https://example/run/4"}]}' \
  2 "NOT CHECKED"

run_case "no run for the sha is undetermined" \
  '{"workflow_runs":[]}' \
  2 "NOT CHECKED"

# `skipped` means the workflow decided not to run. Nothing was verified, so it
# is undetermined rather than green — the distinction a path filter or a
# concurrency cancel would otherwise erase.
run_case "skipped conclusion is undetermined, not green" \
  '{"workflow_runs":[{"status":"completed","conclusion":"skipped","html_url":"https://example/run/5"}]}' \
  2 "NOT CHECKED"

# The query must name the SHA, or the script would read whatever run happens to
# be newest on the repo and call it this export's.
if [ -f "$LAST_URLS" ] && grep -q "head_sha=$SHA" "$LAST_URLS"; then
  echo "PASS: the request names the sha"
else
  echo "FAIL: the request does not filter by head_sha ($LAST_URLS)"
  failures=$((failures+1))
fi

# An unreachable API is undetermined too — never a pass.
out="$(CE_CI_FETCH=/nonexistent-fetch-$$ CE_CI_TIMEOUT=1 CE_CI_INTERVAL=1 bash "$script" "$SHA" 2>&1)"
if [ $? -eq 2 ] && [[ "$out" == *"NOT CHECKED"* ]]; then
  echo "PASS: unreachable API is undetermined"
else
  echo "FAIL: unreachable API — got: $out"
  failures=$((failures+1))
fi

# A missing SHA is a usage error, not a pass.
if bash "$script" >/dev/null 2>&1; then
  echo "FAIL: no sha argument was accepted"
  failures=$((failures+1))
else
  echo "PASS: missing sha is refused"
fi

if [ "$failures" -ne 0 ]; then
  echo "wait-ce-ci_test: $failures failure(s)"
  exit 1
fi
echo "wait-ce-ci_test: all cases passed"
