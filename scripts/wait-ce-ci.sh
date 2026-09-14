#!/usr/bin/env bash
# wait-ce-ci.sh — did the CE export's own CI go green on the public repo?
#
# RELEASE.md step 5 said "confirm the CE `ci` run goes green". It was a
# sentence, not a mechanism — the same shape the CE tag had before 2026-09-06,
# and with the same result. Public `main` was red from the 2026-09-07 export
# through SIX further exports on 2026-09-08/09 (a test reading a pruned compose
# file; another passing only on hosts with a user config), and nobody looked
# until a Dependabot PR's checks were read for an unrelated reason.
#
# The export now gates `go test ./deployments/...` structurally, which covers
# the first of those two shapes. It does not cover a test that is green in EE
# and red in CE for any OTHER reason — which is the general case, and the one
# this script watches.
#
# NO CREDENTIAL. grinco/vornik is public and a public repository's workflow
# runs are readable anonymously, so this needs no token, no deploy key and no
# new secret — which is what makes it cheap enough to be worth doing at all.
#
# THREE OUTCOMES, deliberately distinct (exit codes):
#   0  the run for this SHA concluded SUCCESS
#   1  the run concluded FAILURE/TIMED_OUT/CANCELLED — the case this exists for
#   2  UNDETERMINED: no run appeared, or it was still going at the deadline.
#      NOT a pass. The caller decides what to do with it, and must say
#      "not checked" rather than "clean" — a control that cannot tell those
#      apart reports the first and means the second.
#
# Usage: wait-ce-ci.sh <sha> [repo] [workflow-file]
#   CE_CI_TIMEOUT   total seconds to wait (default 1800)
#   CE_CI_INTERVAL  seconds between polls (default 30)
#   CE_CI_API       API base (default https://api.github.com)
#   CE_CI_FETCH     command that takes a URL and prints the response body
#                   (default: curl). The seam the self-test drives, so every
#                   branch below is exercised without a network or a token.
set -uo pipefail

sha="${1:-}"
repo="${2:-grinco/vornik}"
workflow="${3:-ci.yaml}"
api="${CE_CI_API:-https://api.github.com}"
timeout="${CE_CI_TIMEOUT:-1800}"
interval="${CE_CI_INTERVAL:-30}"

if [ -z "$sha" ]; then
  echo "usage: wait-ce-ci.sh <sha> [repo] [workflow-file]" >&2
  exit 2
fi

command -v jq >/dev/null 2>&1 || { echo "wait-ce-ci: jq is required" >&2; exit 2; }

# fetch prints the response body for a URL, or nothing on any failure. Split
# out so the self-test can substitute a stub: a gate whose only untested part
# is "does curl work" is a gate whose LOGIC is tested, which is the half that
# can be wrong in an interesting way.
fetch() {
  if [ -n "${CE_CI_FETCH:-}" ]; then
    "$CE_CI_FETCH" "$1" 2>/dev/null || true
    return
  fi
  curl -fsSL -H 'Accept: application/vnd.github+json' "$1" 2>/dev/null || true
}

deadline=$(( $(date +%s) + timeout ))
url="$api/repos/$repo/actions/workflows/$workflow/runs?head_sha=$sha&per_page=10"

while :; do
  body="$(fetch "$url")"

  if [ -n "$body" ]; then
    # Newest first is the API's own order; take the newest run for this SHA.
    # A re-run replaces the verdict, which is what an operator would read too.
    status="$(printf '%s' "$body" | jq -r '.workflow_runs[0].status // empty' 2>/dev/null || true)"
    conclusion="$(printf '%s' "$body" | jq -r '.workflow_runs[0].conclusion // empty' 2>/dev/null || true)"
    run_url="$(printf '%s' "$body" | jq -r '.workflow_runs[0].html_url // empty' 2>/dev/null || true)"

    if [ "$status" = "completed" ]; then
      case "$conclusion" in
        success)
          echo "wait-ce-ci: $repo $workflow is GREEN on $sha ($run_url)"
          exit 0
          ;;
        skipped|neutral)
          # Nothing ran, so nothing was verified. Undetermined, not green.
          echo "wait-ce-ci: $repo $workflow concluded '$conclusion' on $sha — NOT CHECKED ($run_url)"
          exit 2
          ;;
        "")
          # completed with no conclusion should not happen; treat as unknown
          # rather than inventing a verdict.
          echo "wait-ce-ci: $repo $workflow completed with no conclusion on $sha — NOT CHECKED" >&2
          exit 2
          ;;
        *)
          echo "wait-ce-ci: $repo $workflow concluded '$conclusion' on $sha ($run_url)" >&2
          exit 1
          ;;
      esac
    fi
  fi

  now=$(date +%s)
  if [ "$now" -ge "$deadline" ]; then
    if [ -n "${status:-}" ]; then
      echo "wait-ce-ci: $repo $workflow still '$status' on $sha after ${timeout}s — NOT CHECKED" >&2
    else
      echo "wait-ce-ci: no $workflow run found for $sha on $repo after ${timeout}s — NOT CHECKED" >&2
    fi
    exit 2
  fi
  sleep "$interval"
done
