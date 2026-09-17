#!/usr/bin/env bash
# Regression guard: the missing-prerequisite failure must not blame an upstream
# role for a path the DAEMON malformed.
#
# 2026-09-16. task_20260916163809_c16163e002783786 /
# exec_20260916164618_4c53e9deafd4e8e6 died in 8s on
#
#   Missing prerequisite: file_read of
#   "/app/workspace/app/workspace/artifacts/in/2026-09-16-retrieval-recency-design.md"
#   returned not-found twice. This usually means an upstream role (researcher,
#   planner) did not produce the expected artifact — check that step's outcome.
#
# No upstream role was involved. rewriteInputPathsInPrompt (executor/plan.go)
# was not idempotent and prepended the container prefix twice; the path is
# visibly doubled in the message that then sends the reader to the producing
# step. The rewrite is fixed, but the message has to be able to say which of
# the two things happened — a control that cannot tell them apart reports the
# one it can name.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

export WORKSPACE="$tmp"
export INPUT_FILE="$tmp/task.json"
export OUTPUT_FILE="$tmp/result.json"
export VORNIK_LLM_MODEL="test-model"
printf '{"context":{"prompt":"x"}}' > "$INPUT_FILE"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

doubled="/app/workspace/app/workspace/artifacts/in/design.md"
slashed="/app/workspace//app/workspace/artifacts/in/design.md"
ok_path="/app/workspace/artifacts/in/design.md"
elsewhere="/tmp/upload/design.md"

# --- the predicate: two occurrences of the container prefix, however joined.
for p in "$doubled" "$slashed"; do
  if path_repeats_container_prefix "$p"; then
    pass "doubled prefix recognised: $p"
  else
    fail "doubled prefix NOT recognised: $p"
  fi
done
for p in "$ok_path" "$elsewhere" "/app/workspace" ""; do
  if path_repeats_container_prefix "$p"; then
    fail "well-formed path reported as doubled: '$p'"
  else
    pass "well-formed path left alone: '$p'"
  fi
done

# --- the message for a malformed path names the real subsystem and does NOT
#     send the reader at the producing step.
msg="$(missing_prerequisite_message "$doubled")"
case "$msg" in
  *"$doubled"*) pass "malformed message quotes the path it actually tried" ;;
  *) fail "malformed message does not quote the path: $msg" ;;
esac
case "$msg" in
  *upstream*|*researcher*|*planner*|*"that step"*)
    fail "malformed message still blames an upstream role: $msg" ;;
  *) pass "malformed message does not blame an upstream role" ;;
esac
case "$msg" in
  *MALFORMED*) pass "malformed message says the path is malformed" ;;
  *) fail "malformed message does not name the defect: $msg" ;;
esac
case "$msg" in
  *"$ok_path"*) pass "malformed message names the path that was meant" ;;
  *) fail "malformed message does not name the intended path: $msg" ;;
esac

# --- the ordinary case keeps its existing, correct diagnosis. This guard is
#     right for a genuinely absent upstream artifact and must not be weakened.
msg="$(missing_prerequisite_message "$ok_path")"
case "$msg" in
  *"upstream role"*) pass "well-formed path keeps the upstream-producer diagnosis" ;;
  *) fail "well-formed path lost its diagnosis: $msg" ;;
esac
case "$msg" in
  *MALFORMED*) fail "well-formed path wrongly reported as malformed: $msg" ;;
  *) pass "well-formed path is not called malformed" ;;
esac
case "$msg" in
  *"not-found twice"*) pass "well-formed path keeps the repeat-miss phrasing" ;;
  *) fail "well-formed path lost the repeat-miss phrasing: $msg" ;;
esac

echo
[ "$fails" -eq 0 ] || { echo "$fails case(s) failed" >&2; exit 1; }
echo "all cases passed"
