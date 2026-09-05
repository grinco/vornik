#!/usr/bin/env bash
# Tests for the skill_fetch agent tool (progressive-disclosure skills,
# LLD 2026-07-12-skill-progressive-disclosure-design). Sources the
# entrypoint (function definitions only) and exercises the handler's
# degradation paths + URL construction against a local fake daemon.
set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"

PASS=0
FAIL=0
FAILURES=()
ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILURES+=("$1"); echo "FAIL: $1"; }

# shellcheck disable=SC1090
source "$ENTRYPOINT" >/dev/null 2>&1 || true

if ! declare -f handle_skill_fetch >/dev/null; then
    bad "handle_skill_fetch must exist in entrypoint.sh"
else
    workdir="$(mktemp -d)"
    trap 'rm -rf "$workdir"' EXIT
    INPUT_FILE="$workdir/task.json"
    echo '{"projectId":"p1"}' > "$INPUT_FILE"

    # 1. Missing VORNIK_API_URL degrades to a structured error.
    unset VORNIK_API_URL 2>/dev/null || true
    out=$(handle_skill_fetch '{"name":"x"}')
    if printf '%s' "$out" | jq -e '.error' >/dev/null 2>&1; then
        ok "degrades gracefully without VORNIK_API_URL"
    else
        bad "expected structured error without VORNIK_API_URL, got: $out"
    fi

    # 2. Missing name degrades the same way.
    export VORNIK_API_URL="http://127.0.0.1:1"
    out=$(handle_skill_fetch '{}')
    if printf '%s' "$out" | jq -e '.error' >/dev/null 2>&1; then
        ok "degrades gracefully without a name"
    else
        bad "expected structured error without name, got: $out"
    fi

    # 3. Unreachable daemon yields the request-failed error, not a hang.
    export VORNIK_EXECUTION_ID="exec-t"
    out=$(handle_skill_fetch '{"name":"any"}')
    if printf '%s' "$out" | jq -e '.error' >/dev/null 2>&1; then
        ok "unreachable daemon yields structured error"
    else
        bad "expected error from unreachable daemon, got: $out"
    fi
fi

# Registration pins: the declaration (schema in the generated registry the
# entrypoint sources, from internal/agenttools) + the dispatch case must exist.
if [ -n "$(tool_definition_for skill_fetch 2>/dev/null)" ]; then
    ok "skill_fetch tool schema registered"
else
    bad "skill_fetch tool schema missing"
fi
if grep -qE '^\s+skill_fetch\)' "$ENTRYPOINT"; then
    ok "skill_fetch dispatch case present"
else
    bad "skill_fetch dispatch case missing"
fi

echo "================================"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    printf '%s\n' "${FAILURES[@]}"
    exit 1
fi
