#!/usr/bin/env bash
# Fail before installing/building when CI, ref, signing or API state is unsafe.
set -euo pipefail
[[ "${GITHUB_REF:-}" == refs/tags/* ]] || { echo '::error::Dispatch at a release tag'; exit 1; }
[[ "${GITHUB_REF_NAME:-}" =~ ^(ent-v|v)?20[0-9]{2}\.[0-9]+\.[0-9]+$ ]] || { echo '::error::Expected a calendar release tag'; exit 1; }
test -n "${GPG_PRIVATE_KEY:-}" || { echo '::error::GPG_PRIVATE_KEY is required'; exit 1; }
sha=$(git rev-parse HEAD)
gh api --paginate "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yaml/runs?head_sha=$sha&status=success&per_page=100" \
  --jq '.workflow_runs[] | select(.event == "push" or .event == "workflow_dispatch") | .id' > "$RUNNER_TEMP/release-ci-runs"
test -s "$RUNNER_TEMP/release-ci-runs" || { echo '::error::Full CI has not passed for this commit. Wait then rerun; RELEASE.md documents local gates when quota is exhausted.'; exit 1; }
response="$RUNNER_TEMP/release-existing.json"
if gh api "repos/$GITHUB_REPOSITORY/releases/tags/$GITHUB_REF_NAME" > "$response" 2> "$RUNNER_TEMP/release-api-error"; then
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
