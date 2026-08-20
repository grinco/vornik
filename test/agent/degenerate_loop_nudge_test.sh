#!/usr/bin/env bash
# Regression tests for the 2026-08-16 long-horizon arm's second-largest failure
# cause: degenerate loops, 23 of 73 failed steps (31.5%), spread across coder
# (12), analyst (8) and tester (3) — a harness defect, not role tuning.
#
# The detector ended the step without ever telling the model it was repeating
# itself, and its third call was wasted work whose result was already known.
#
# The safety argument rests entirely on the detector being CONSECUTIVE-only:
# any different call resets repeat_count, so two counted repeats are adjacent
# and nothing can have mutated between them. That is why suppressing the second
# call is safe, and why no run_shell result cache may be added — a cache keyed
# on anything broader than adjacency would tell an agent its edit changed
# nothing.
#
# These are structural assertions against entrypoint.sh. The loop body itself
# needs a live LLM and container, so what is pinned here is the invariants a
# future edit could silently break.
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

ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILURES+=("$1"); echo "FAIL: $1"; }

# 1. The kill threshold moved to 4, leaving room for a nudge at 2 and 3.
if grep -qE '^[[:space:]]*MAX_REPEATS=4[[:space:]]*$' "$ENTRYPOINT"; then
    ok "kill threshold is 4"
else
    bad "MAX_REPEATS must be 4 — at 3 there is no room to nudge before killing"
fi

# 2. The nudge fires strictly before the kill.
NUDGE_AT="$(grep -oE '^[[:space:]]*DEGENERATE_NUDGE_AT=[0-9]+' "$ENTRYPOINT" | grep -oE '[0-9]+$' | head -1)"
MAXR="$(grep -oE '^[[:space:]]*MAX_REPEATS=[0-9]+' "$ENTRYPOINT" | grep -oE '[0-9]+$' | head -1)"
if [ -n "$NUDGE_AT" ] && [ -n "$MAXR" ] && [ "$NUDGE_AT" -lt "$MAXR" ]; then
    ok "nudge threshold ($NUDGE_AT) is below the kill threshold ($MAXR)"
else
    bad "DEGENERATE_NUDGE_AT ($NUDGE_AT) must be below MAX_REPEATS ($MAXR), or the model is killed without ever being warned"
fi

# 3. The repeated call is SUPPRESSED, not re-executed: the nudge path sets
#    tc_cache_hit=1, which is what makes the exec_tool block skip it.
if grep -A 8 'repeat_count" -ge "\$DEGENERATE_NUDGE_AT' "$ENTRYPOINT" | grep -q 'tc_cache_hit=1'; then
    ok "an adjacent repeat is suppressed rather than re-run"
else
    bad "the nudge must set tc_cache_hit=1 — otherwise the identical call still executes and nothing is saved"
fi

# 4. The nudge carries the PREVIOUS result, so the model is not left blind.
if grep -A 8 'repeat_count" -ge "\$DEGENERATE_NUDGE_AT' "$ENTRYPOINT" | grep -q 'last_tool_result'; then
    ok "the nudge returns the previous result alongside the warning"
else
    bad "the nudge must return last_tool_result — replacing a tool result with a bare warning loses information the model needs"
fi

# 5. last_tool_result is captured from a real execution.
#    Checked by ORDER, not by proximity. This was a `grep -A 8` window until
#    2026-08-20, when it went red without any behaviour changing: the advisory
#    near-repeat block and its comments grew between the exec_tool call and the
#    assignment, pushing them 13 lines apart. A test that fails because correct
#    code got longer teaches the next person to widen the window, which is how
#    an assertion quietly stops asserting. What actually matters is that the
#    capture happens AFTER the call it captures.
exec_line=$(grep -n 'tool_result=\$(exec_tool' "$ENTRYPOINT" | head -1 | cut -d: -f1)
capture_line=$(grep -n 'last_tool_result="\$tool_result"' "$ENTRYPOINT" | head -1 | cut -d: -f1)
if [ -n "$exec_line" ] && [ -n "$capture_line" ] && [ "$capture_line" -gt "$exec_line" ]; then
    ok "last_tool_result is populated after each executed tool call"
else
    bad "last_tool_result must be set after exec_tool, or the nudge has nothing to return"
fi

# 6. THE SAFETY INVARIANT. The detector must stay consecutive-only: a different
#    call resets the counter. If this reset is ever removed, an edit-then-retest
#    cycle becomes suppressible and the nudge turns into a correctness bug.
if grep -A 4 'last_tool_sig="\$tool_sig"' "$ENTRYPOINT" | grep -q 'repeat_count=1'; then
    ok "a different tool call resets the repeat counter (adjacency preserved)"
else
    bad "repeat_count must reset when the signature changes — without it, file_edit followed by an identical test command could be suppressed"
fi

# 7. run_shell must NOT be added to the read-only tool cache. That cache is
#    keyed on (tool, args) for the whole step, with no adjacency requirement,
#    so caching a mutating command there would survive an intervening edit.
if declare -f tool_is_cacheable_read >/dev/null 2>&1 || grep -q 'tool_is_cacheable_read()' "$ENTRYPOINT"; then
    if grep -A 12 'tool_is_cacheable_read()' "$ENTRYPOINT" | grep -qE '(^|[|[:space:]])run_shell([|)\\]|$)'; then
        bad "run_shell must never be in tool_is_cacheable_read — that cache ignores adjacency and would suppress a legitimate re-run after an edit"
    else
        ok "run_shell is not in the read-only tool cache"
    fi
else
    bad "tool_is_cacheable_read not found — the default-deny cache allow-list must exist"
fi

echo
echo "passed: $PASS, failed: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    printf '  - %s\n' "${FAILURES[@]}"
    exit 1
fi
