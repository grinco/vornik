#!/usr/bin/env bash
# Regression tests for the 2026-08-18 root-cause finding: the fleet's dominant
# benchmark failure was a READ-ONLY GIT MOUNT that nothing told the agents about.
#
# internal/runtime/manager.go bind-mounts the project's main .git read-only while
# the worktree is rw. A worktree keeps its index/HEAD/logs under that main .git,
# so `git add` / `git commit` / `git stash` cannot land. Measured over the
# agentbench ledger: of 108 executions killed by the degenerate-loop detector,
# the final repeated command was git in 96 (89%) — 64 reads whose output never
# changes, 32 writes that can never succeed. Agents invented `.git-local`, a
# second git dir absent from all source, and later agents reuse it.
#
# Covers:
#   1. run_shell names the cause when a command fails on the read-only git, so
#      the tool result CHANGES — which is what breaks a retry loop.
#   2. the near-repeat detector exists, is looser than the exact one, and warns
#      before it kills.
#   3. the near-repeat advisory does NOT suppress the call (arguments genuinely
#      differ, so the result may too).
set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
if [ ! -f "$ENTRYPOINT" ]; then
    echo "entrypoint not found: $ENTRYPOINT"
    exit 1
fi

PASS=0
FAIL=0
FAILURES=()
ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILURES+=("$1"); echo "FAIL: $1"; }

# 1. run_shell annotates a read-only-git failure.
if grep -q 'Read-only file system' "$ENTRYPOINT"; then
    ok "run_shell detects the read-only-git failure signature"
else
    bad "run_shell must detect 'Read-only file system' — the bare git error reads as a transient glitch and agents retried it"
fi
if grep -q 'the harness commits everything left in the workspace' "$ENTRYPOINT"; then
    ok "the annotation states the harness commits for the agent"
else
    bad "the annotation must say the harness commits — telling an agent it CANNOT commit without saying who does is an unanswered question, and it will retry"
fi

# 2. Near-repeat thresholds exist and are ordered.
NEAR_MAX="$(grep -oE '^[[:space:]]*NEAR_MAX_REPEATS=[0-9]+' "$ENTRYPOINT" | grep -oE '[0-9]+$' | head -1)"
NEAR_NUDGE="$(grep -oE '^[[:space:]]*NEAR_REPEAT_NUDGE_AT=[0-9]+' "$ENTRYPOINT" | grep -oE '[0-9]+$' | head -1)"
EXACT_MAX="$(grep -oE '^[[:space:]]*MAX_REPEATS=[0-9]+' "$ENTRYPOINT" | grep -oE '[0-9]+$' | head -1)"
if [ -n "$NEAR_MAX" ] && [ -n "$NEAR_NUDGE" ] && [ "$NEAR_NUDGE" -lt "$NEAR_MAX" ]; then
    ok "near-repeat nudge ($NEAR_NUDGE) is below the near kill ($NEAR_MAX)"
else
    bad "NEAR_REPEAT_NUDGE_AT ($NEAR_NUDGE) must be below NEAR_MAX_REPEATS ($NEAR_MAX)"
fi
if [ -n "$NEAR_MAX" ] && [ -n "$EXACT_MAX" ] && [ "$NEAR_MAX" -gt "$EXACT_MAX" ]; then
    ok "the near kill ($NEAR_MAX) is looser than the exact kill ($EXACT_MAX)"
else
    bad "NEAR_MAX_REPEATS ($NEAR_MAX) must exceed MAX_REPEATS ($EXACT_MAX) — a repeating SHAPE can be legitimate work (paging a file), a byte-identical repeat never is"
fi

# 3. The near signature collapses digits — that is the whole mechanism.
if grep -q "tr -s '0-9' '#'" "$ENTRYPOINT"; then
    ok "the near signature normalises digit runs"
else
    bad "the near signature must collapse digits, or a sliding window ('175,270p' then '176,280p') evades the detector exactly as it did on 2026-08-18"
fi

# 4. The advisory must NOT suppress execution: unlike an exact repeat, the
#    arguments differ, so the result may legitimately differ too.
if grep -A 6 'near_repeat_warn:-0' "$ENTRYPOINT" | grep -q 'tc_cache_hit=1'; then
    bad "the near-repeat advisory must not set tc_cache_hit — the call's arguments differ, so its result cannot be assumed identical"
else
    ok "the near-repeat advisory annotates without suppressing the call"
fi

# 5. near_repeat_warn resets per call, or one near-repeat brands every later result.
if grep -q 'local near_repeat_warn=0' "$ENTRYPOINT"; then
    ok "near_repeat_warn is reset per tool call"
else
    bad "near_repeat_warn must be declared per call — a step-scoped flag would mark every subsequent result"
fi

echo
echo "passed: $PASS, failed: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    printf '  - %s\n' "${FAILURES[@]}"
    exit 1
fi
