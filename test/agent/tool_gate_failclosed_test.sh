#!/usr/bin/env bash
# Fail-closed tool gates — agent runtime contract §7.1.
#
# Sources images/vornik-agent/entrypoint.sh and drives the two gates directly:
# exec_tool() for execution, tool_definitions() for advertisement.
#
# The invariant under test: a tool is permitted iff it is on the effective
# allowlist, OR registered in contractreg.UngatedByDesign (tool_search,
# tool_result_read), OR matches a prefix in contractreg.UngatedPrefixesByDesign
# (mcp__, gated daemon-side). Anything else is refused — INCLUDING a name that
# appears in no registry at all, which is the case both gates used to let
# through (2026.8.1 bypass, backlog P0 2026-08-19).

set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
if [ ! -f "$ENTRYPOINT" ]; then
    echo "FAIL: entrypoint.sh not found at $ENTRYPOINT" >&2
    exit 1
fi

# The gate consults its registries through jq. If jq were missing, every permit
# check would fail and every assertion below would pass — for the wrong reason,
# since "denied because the gate said no" and "denied because nothing could be
# parsed" are the same observation from outside. Refuse to run rather than
# report a green that means nothing.
if ! command -v jq >/dev/null 2>&1; then
    echo "FAIL: jq is required — without it these assertions pass vacuously" >&2
    exit 1
fi

PASS=0
FAIL=0
FAILURES=()

assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then
        PASS=$((PASS+1)); echo "PASS: $name"
    else
        FAIL=$((FAIL+1)); FAILURES+=("$name: expected to contain '$needle', got: ${haystack:0:200}")
        echo "FAIL: $name"
    fi
}

assert_not_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" != *"$needle"* ]]; then
        PASS=$((PASS+1)); echo "PASS: $name"
    else
        FAIL=$((FAIL+1)); FAILURES+=("$name: unexpectedly contains '$needle'")
        echo "FAIL: $name"
    fi
}

ORIG_PATH="$PATH"

# setup ALLOWED_TOOLS_JSON — a role whose allowlist is exactly that list.
setup() {
    TMP="$(mktemp -d)"
    export PATH="$ORIG_PATH"
    mkdir -p "$TMP/workspace" "$TMP/input"
    printf '{"config":{"permissions":{"allowedTools":%s}}}' "$1" > "$TMP/input/task.json"
    export WORKSPACE="$TMP/workspace"
    export INPUT_FILE="$TMP/input/task.json"
    export OUTPUT_FILE="$TMP/output.json"
    set +u
    # shellcheck disable=SC1090
    source "$ENTRYPOINT"
    trap - EXIT
    set -e +u
    set +e
}

teardown() {
    rm -rf "$TMP"
    unset WORKSPACE INPUT_FILE OUTPUT_FILE TMP
    export PATH="$ORIG_PATH"
}

# ---------- execution gate ----------

setup '["file_read","current_time"]'

# The substance of the fail-closed flip: a name in NO registry. Before the flip
# `is_builtin_tool` returned 1 for it, which made the gate's first conjunct
# false and skipped the allowlist entirely.
out=$(exec_tool totally_unregistered_tool '{}')
assert_contains "unregistered name is refused" "$out" "ERROR"
assert_not_contains "unregistered name does not reach a handler" "$out" "OK:"

# The 2026-08-20 malformed identity: a model leaked a reasoning-block terminator
# into the tool name and it was accepted and persisted. It begins with a real
# tool id, so a prefix-tolerant gate would run file_write.
out=$(exec_tool 'file_write</think>allowed_paths' '{"path":"x.txt","content":"pwned"}')
assert_contains "malformed tool name is refused" "$out" "ERROR"
if [ -f "$TMP/workspace/x.txt" ]; then
    FAIL=$((FAIL+1)); FAILURES+=("malformed tool name must not write a file")
    echo "FAIL: malformed tool name must not write a file"
else
    PASS=$((PASS+1)); echo "PASS: malformed tool name must not write a file"
fi

# A registered builtin the role was not granted.
out=$(exec_tool run_shell '{"command":"echo hi"}')
assert_contains "ungranted builtin is refused" "$out" "not allowed for this role"

# THE 2026.8.1 BYPASS, reproduced. The four tools of that incident had a
# dispatch case and were missing from is_builtin_tool(), so the gate's first
# conjunct was false and the allowlist was never consulted. contractreg now
# prevents that drift at build time, which is exactly why this has to be
# simulated: shadow is_builtin_tool so it reports "not a builtin" for
# everything, then call an ungranted tool that still has a dispatch case.
#
# Pre-flip this EXECUTES the command. Post-flip the allowlist is the only
# authority, so registry drift can no longer widen anything.
real_is_builtin_tool=$(declare -f is_builtin_tool)
is_builtin_tool() { return 1; }
out=$(exec_tool run_shell '{"command":"echo BYPASSED"}')
assert_not_contains "registry drift cannot bypass the allowlist" "$out" "BYPASSED"
assert_contains "registry drift still refuses" "$out" "ERROR"
eval "$real_is_builtin_tool"

# The name reaches jq as --arg data, never as program text. These are the
# shapes that would matter if it did not: jq string syntax, jq interpolation,
# and shell command substitution.
for hostile in 'foo"bar' 'foo\\(1+1)' 'foo$(touch '"$TMP"'/pwned)' 'foo`touch '"$TMP"'/pwned2`' '.[]|halt_error'; do
    out=$(exec_tool "$hostile" '{}')
    assert_contains "hostile name refused: ${hostile:0:14}" "$out" "ERROR"
done
if [ -f "$TMP/pwned" ] || [ -f "$TMP/pwned2" ]; then
    FAIL=$((FAIL+1)); FAILURES+=("a tool name executed shell command substitution")
    echo "FAIL: a tool name executed shell command substitution"
else
    PASS=$((PASS+1)); echo "PASS: a tool name cannot execute shell command substitution"
fi

# A registered builtin the role WAS granted still runs.
printf 'hello\n' > "$TMP/workspace/greeting.txt"
out=$(exec_tool file_read '{"path":"greeting.txt"}')
assert_contains "granted builtin still runs" "$out" "hello"

# UngatedByDesign — declared by 0 of 45 live roles, so gating either would
# strand deferred MCP discovery and result pagination.
out=$(exec_tool tool_search '{"query":"anything"}')
assert_not_contains "tool_search is exempt from the allowlist" "$out" "not allowed for this role"
out=$(exec_tool tool_result_read '{"tool_call_id":"nope"}')
assert_not_contains "tool_result_read is exempt from the allowlist" "$out" "not allowed for this role"

# UngatedPrefixesByDesign — mcp__ is gated daemon-side (roleAllowsMCPTool), and
# the container cannot reproduce that decision (§7.1 division of labour).
out=$(exec_tool mcp__broker__get_positions '{}')
assert_not_contains "mcp__ is not refused by the container gate" "$out" "not allowed for this role"

# The refusal messages must stay distinguishable: a registered-but-ungranted
# tool is an operator allowlist question, an unregistered one is a bug or a
# hallucinated name.
out_known=$(exec_tool git_status '{}')
out_unknown=$(exec_tool no_such_tool_at_all '{}')
assert_contains "ungranted builtin says not-allowed" "$out_known" "not allowed for this role"
assert_contains "unregistered name says unknown" "$out_unknown" "unknown tool"
teardown

# ---------- advertisement gate ----------

setup '["file_read","current_time"]'
tools=$(tool_definitions)

assert_contains "granted builtin is advertised" "$tools" '"name": "file_read"'
assert_not_contains "ungranted builtin is NOT advertised" "$tools" '"name": "run_shell"'
assert_not_contains "ungranted builtin is NOT advertised (git_status)" "$tools" '"name": "git_status"'

# Fail-closed in the other direction too: a definition whose name is absent from
# BUILTIN_TOOL_NAMES_JSON must not be advertised. Before the flip, absence from
# that list SATISFIED the filter and the tool went to every role's model.
BUILTIN_TOOL_NAMES_JSON='["current_time"]'
tools_narrowed=$(tool_definitions)
assert_not_contains "unregistered definition is NOT advertised" "$tools_narrowed" '"name": "file_read"'
teardown

# A role granted nothing beyond the daemon's baseline still sees current_time,
# which allowed_builtin_tools_json unions in unconditionally.
setup '["file_read"]'
tools=$(tool_definitions)
assert_contains "current_time is always advertised" "$tools" '"name": "current_time"'
teardown

# ---------- the third advertisement path (2026-08-22) ----------
#
# tool_definitions() appended $extras_ungated UNCONDITIONALLY, bypassing the
# advertisement filter entirely — a third path beside the builtin definition
# list and $extras_gated. skill_fetch rode it, commented "Ungated (like
# memory_search)"; memory_search had been re-gated on 2026-08-16 precisely so
# advertisement would follow execution, and skill_fetch was left behind.
#
# It was described as harmless because agenttools.alwaysGranted puts skill_fetch
# on every role's effective allowlist. That holds only for input the daemon
# built. allowed_builtin_tools_json falls back to
# ["file_read","file_write","run_shell","current_time"] when
# .config.permissions.allowedTools is absent, and that fallback has no
# skill_fetch — so in exactly that state the tool was ADVERTISED and REFUSED,
# which is the see-but-cannot-call the fail-closed flip exists to prevent.

# skill_fetch is advertised when the role's effective allowlist carries it,
# which on the daemon path it always does (withAlwaysGrantedTools).
setup '["file_read","skill_fetch","current_time"]'
export VORNIK_API_URL="http://daemon.invalid"
tools=$(tool_definitions)
assert_contains "skill_fetch advertised when granted" "$tools" '"name": "skill_fetch"'
teardown

# ...and NOT advertised when it is absent from the allowlist, which is the state
# the container's own fallback produces. Advertisement must follow execution.
setup '["file_read","current_time"]'
export VORNIK_API_URL="http://daemon.invalid"
tools=$(tool_definitions)
assert_not_contains "skill_fetch NOT advertised when ungranted" "$tools" '"name": "skill_fetch"'
teardown

# The ungated append is filtered against the registry. An extra that is not in
# UNGATED_TOOL_NAMES_JSON must not reach the model just because it was appended
# to extras_ungated — that is the bypass, independent of which tool rides it.
setup '["file_read","current_time"]'
export VORNIK_API_URL="http://daemon.invalid"
UNGATED_TOOL_NAMES_JSON='["tool_search"]'
tools=$(tool_definitions)
assert_not_contains "unregistered ungated extra is NOT advertised" "$tools" '"name": "tool_result_read"'
teardown

# ---------- summary ----------
echo ""
echo "================================"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "Failures:"
    for f in "${FAILURES[@]}"; do
        echo "  - $f"
    done
    exit 1
fi
exit 0
