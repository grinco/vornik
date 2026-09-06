#!/usr/bin/env bash
# Fail before installing/building when CI, ref, signing or API state is unsafe.
set -euo pipefail
[[ "${GITHUB_REF:-}" == refs/tags/* ]] || { echo '::error::Dispatch at a release tag'; exit 1; }
[[ "${GITHUB_REF_NAME:-}" =~ ^v?20[0-9]{2}\.[0-9]+\.[0-9]+$ ]] || { echo '::error::Expected a calendar release tag'; exit 1; }
test -n "${GPG_PRIVATE_KEY:-}" || { echo '::error::GPG_PRIVATE_KEY is required'; exit 1; }

# A 4xx is an ANSWER. A 5xx, a 429 or a dropped connection is not.
#
# Both directions of getting this wrong are real, and they are not symmetric.
# Treating a transient failure as an answer is the dangerous one: the
# releases/tags probe below would read "no such release" out of a 503 and the
# run would rebuild and re-publish over a release that already exists. Treating
# it as fatal is merely expensive — but it strands a release mid-flight with no
# way forward but a human re-run, which is the reliability gap this closes.
#
# So: retry the responses that carry no information, and fail fast on the ones
# that do. A 403 is a permissions answer, not a hiccup — except GitHub's rate
# limiter, which also speaks 403 and says so in the body, so that one case is
# read out of the message rather than the status.
GH_API_ATTEMPTS="${GH_API_ATTEMPTS:-5}"
gh_api_retry() {
  local out="$1"; shift
  local err="$RUNNER_TEMP/release-api-error"
  local attempt delay=2 status
  for (( attempt = 1; attempt <= GH_API_ATTEMPTS; attempt++ )); do
    if gh api "$@" > "$out" 2> "$err"; then
      return 0
    fi
    # gh writes "gh: Not Found (HTTP 404)"; match the code with or without the
    # parentheses so a message-format change cannot silently reclassify every
    # answer as a retryable network error.
    status="$(sed -n 's/.*HTTP \([0-9]\{3\}\).*/\1/p' "$err" | head -1)"
    case "$status" in
      429|5[0-9][0-9]) ;;                                        # transient
      403) grep -qi 'rate limit' "$err" "$out" || return 1 ;;    # 403 = answer, unless throttled
      '')  ;;                                                    # no status at all = network/DNS
      *)   return 1 ;;                                           # 404 and friends are answers
    esac
    [[ "$attempt" -lt "$GH_API_ATTEMPTS" ]] || return 1
    echo "::warning::GitHub API gave ${status:-no HTTP status}; retrying in ${delay}s (attempt ${attempt}/${GH_API_ATTEMPTS})"
    sleep "$delay"
    delay=$(( delay * 2 ))
  done
  return 1
}

sha=$(git rev-parse HEAD)
runs="$RUNNER_TEMP/release-ci-runs"
gh_api_retry "$runs" --paginate "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yaml/runs?head_sha=$sha&status=success&per_page=100" \
  --jq '.workflow_runs[] | select(.event == "push" or .event == "workflow_dispatch") | .id' \
  || { echo '::error::Cannot reach the GitHub API to verify CI for this commit; the release is NOT known to be unverified, only unverifiable. Re-run.'; exit 1; }
test -s "$runs" || { echo '::error::Full CI has not passed for this commit. Wait then rerun; RELEASE.md documents local gates when quota is exhausted.'; exit 1; }
response="$RUNNER_TEMP/release-existing.json"
if gh_api_retry "$response" "repos/$GITHUB_REPOSITORY/releases/tags/$GITHUB_REF_NAME"; then
  if jq -e '.draft == false' "$response" >/dev/null; then
    jq -e '[.assets[].name] as $a | ($a | index("checksums.txt.asc")) != null and ($a | index("checksums.txt")) != null and ($a | index("vornik-release.asc")) != null and any($a[]; endswith(".rpm")) and any($a[]; endswith(".deb"))' "$response" >/dev/null || { echo '::error::Published release is incomplete; inspect it before retrying'; exit 1; }
    echo 'published=true' >> "$GITHUB_OUTPUT"
    exit 0
  fi
else
  # A denied/rate-limited API is not evidence that the release does not exist.
  jq -e '.status == "404" or .status == 404' "$response" >/dev/null || { echo '::error::Cannot determine existing release state'; exit 1; }
fi
echo 'published=false' >> "$GITHUB_OUTPUT"
