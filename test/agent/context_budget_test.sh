#!/usr/bin/env bash
# Regression tests for the 2026-07-12 context-overflow incident
# (task_20260712145902_18667395d2826b72): glm-5 rejected a 194561-token
# prompt because keep-tail compaction preserved ~256KB tool results verbatim
# and the byte→token estimate reserved no output budget. Covers:
#   1. clamp_tool_contents — bounds every tool message and marks truncation.
#   2. the hardened threshold formula — reserves max_tokens and uses
#      3 bytes/token (grep-pinned so a revert to the old `* 4 * 80` math or
#      a dropped max_tokens reservation fails loudly).
set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
if [ ! -f "$ENTRYPOINT" ]; then
    echo "FAIL: entrypoint.sh not found at $ENTRYPOINT" >&2
    exit 1
fi

PASS=0
FAIL=0
FAILURES=()

ok()   { PASS=$((PASS+1)); echo "PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); FAILURES+=("$1"); echo "FAIL: $1"; }

# Source only the function definitions (entrypoint guards its main flow the
# same way tools_test.sh relies on).
# shellcheck disable=SC1090
source "$ENTRYPOINT" >/dev/null 2>&1 || true

if ! declare -f clamp_tool_contents >/dev/null; then
    bad "clamp_tool_contents function must exist in entrypoint.sh"
else
    workdir="$(mktemp -d)"
    trap 'rm -rf "$workdir"' EXIT
    msgs="$workdir/msgs.json"

    fat="$(printf 'X%.0s' $(seq 1 50000))"
    jq -n --arg fat "$fat" '[
        {"role":"system","content":"sys"},
        {"role":"user","content":"task"},
        {"role":"assistant","tool_calls":[{"id":"a"}]},
        {"role":"tool","tool_call_id":"a","content":$fat},
        {"role":"assistant","content":"small"}
    ]' > "$msgs"

    before=$(wc -c < "$msgs")
    clamp_tool_contents "$msgs" 5000
    after=$(wc -c < "$msgs")

    if [ "$after" -lt 7000 ] && [ "$before" -gt 50000 ]; then
        ok "clamp bounds a 50KB tool result under the cap"
    else
        bad "clamp did not shrink the file (before=$before after=$after)"
    fi
    if jq -e '.[3].content | contains("[tool result truncated")' "$msgs" >/dev/null; then
        ok "truncation marker present on the clamped tool message"
    else
        bad "clamped tool message missing the truncation marker"
    fi
    if [ "$(jq -r '.[4].content' "$msgs")" = "small" ] && [ "$(jq -r '.[1].content' "$msgs")" = "task" ]; then
        ok "non-tool messages are untouched"
    else
        bad "clamp modified non-tool messages"
    fi
    if jq -e 'length == 5' "$msgs" >/dev/null; then
        ok "message structure preserved (no drops)"
    else
        bad "clamp changed the message count"
    fi
fi

# Threshold-formula pins (textual): the budget must reserve the output
# tokens and use the 3-bytes/token estimate.
if grep -q 'LLM_CONTEXT_SIZE - ${LLM_MAX_TOKENS' "$ENTRYPOINT"; then
    ok "threshold reserves the output-token budget"
else
    bad "threshold no longer reserves max_tokens (2026-07-12 regression)"
fi
if grep -qE 'budget_tokens \* 3 \* 80 / 100' "$ENTRYPOINT"; then
    ok "threshold uses the 3-bytes/token estimate"
else
    bad "threshold byte/token estimate changed from 3 (2026-07-12 regression)"
fi
if grep -q 'CONTEXT_OVERFLOW' "$ENTRYPOINT"; then
    ok "agent handles the proxy's CONTEXT_OVERFLOW code"
else
    bad "agent no longer reacts to CONTEXT_OVERFLOW"
fi

echo "================================"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    printf '%s\n' "${FAILURES[@]}"
    exit 1
fi
