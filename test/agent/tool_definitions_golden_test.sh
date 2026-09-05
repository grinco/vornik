#!/usr/bin/env bash
# tool_definitions_golden_test.sh — the tools array a role's model sees, pinned.
#
# Design: https://docs.vornik.io §6.
# The fixtures under fixtures/tool_definitions/ were RECORDED against the
# entrypoint as it stood before the tool registries became generated views of
# internal/agenttools (commit named in the fixtures' README). This test asserts
# tool_definitions() still produces byte-identical JSON (canonicalised with
# `jq -S`) for every cell of the matrix, so a refactor of how definitions are
# assembled cannot change what a model is offered — a description, a parameter,
# an inclusion rule — without failing a named cell.
#
# Environment axis: one state per advertise token, isolating it, plus both ends.
#   bare        nothing set (result hygiene is ON by default: VORNIK_TOOL_RESULT_HYGIENE=1)
#   nohygiene   VORNIK_TOOL_RESULT_HYGIENE=0 — the negative case for WhenResultHygiene
#   mem         VORNIK_MEM_URL only
#   api         VORNIK_API_URL only (skill_fetch, query_api, list_apis; NOT the lifecycle pair)
#   task        VORNIK_API_URL + VORNIK_TASK_ID
#   all         every token true
# Allowlist axis: the shapes allowed_builtin_tools_json() distinguishes, named
# for what task.json contains.
#   no-task-json          the file is absent (the `|| printf` fallback)
#   no-allowedtools-key   config.permissions has no allowedTools (the `//` fallback —
#                         the 2026-08-22 state in which skill_fetch was advertised then refused)
#   default-four          allowedTools is exactly what the daemon substitutes for a role that declared none
#   full                  every declared name
#   file-read-only        ["file_read"]
# all × file-read-only is the narrowing-only proof: every token true, and the
# output is file_read, current_time (the container's unconditional grant, LLD 09
# §7.1 rule 1) and the exempt tools — nothing else.
#
# Usage: test/agent/tool_definitions_golden_test.sh [--record]
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
FIXTURES="$HERE/fixtures/tool_definitions"
RECORD=0
[ "${1:-}" = "--record" ] && RECORD=1

command -v jq >/dev/null 2>&1 || { echo "FAIL: jq is required" >&2; exit 1; }
[ -f "$ENTRYPOINT" ] || { echo "FAIL: entrypoint not found at $ENTRYPOINT" >&2; exit 1; }

FULL='["file_read","file_write","file_edit","run_shell","current_time","read_many_files","grep","glob","git_status","git_diff","git_log","git_show","test_run","lint_run","typecheck_run","memory_search","tool_result_read","query_api","list_apis","backlog_deposit","skill_fetch","get_conversation_window","summarize_thread","tool_search"]'

ENVS=(bare nohygiene mem api task all)
ALLOWS=(no-task-json no-allowedtools-key default-four full file-read-only)

# render <env> <allow> — prints the canonical tools array for one cell.
render() {
    local envname="$1" allow="$2"
    local tmp
    tmp="$(mktemp -d)"
    mkdir -p "$tmp/workspace" "$tmp/input"
    case "$allow" in
        no-task-json) ;;
        no-allowedtools-key) printf '{"config":{"permissions":{}}}' > "$tmp/input/task.json" ;;
        default-four) printf '{"config":{"permissions":{"allowedTools":["file_read","file_write","run_shell","current_time"]}}}' > "$tmp/input/task.json" ;;
        full) printf '{"config":{"permissions":{"allowedTools":%s}}}' "$FULL" > "$tmp/input/task.json" ;;
        file-read-only) printf '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$tmp/input/task.json" ;;
    esac
    (
        unset VORNIK_MEM_URL VORNIK_API_URL VORNIK_TASK_ID VORNIK_TOOL_RESULT_HYGIENE
        case "$envname" in
            bare) ;;
            nohygiene) export VORNIK_TOOL_RESULT_HYGIENE=0 ;;
            mem) export VORNIK_MEM_URL=http://mem.test ;;
            api) export VORNIK_API_URL=http://api.test ;;
            task) export VORNIK_API_URL=http://api.test VORNIK_TASK_ID=task_golden ;;
            all) export VORNIK_MEM_URL=http://mem.test VORNIK_API_URL=http://api.test VORNIK_TASK_ID=task_golden VORNIK_TOOL_RESULT_HYGIENE=1 ;;
        esac
        export WORKSPACE="$tmp/workspace" INPUT_FILE="$tmp/input/task.json" OUTPUT_FILE="$tmp/output.json"
        set +u
        # shellcheck disable=SC1090
        source "$ENTRYPOINT"
        trap - EXIT
        set +e
        tool_definitions | jq -S .
    )
    local rc=$?
    rm -rf "$tmp"
    return $rc
}

pass=0; fail=0
for e in "${ENVS[@]}"; do
    for a in "${ALLOWS[@]}"; do
        cell="$e-$a"
        out="$(render "$e" "$a")" || { echo "FAIL: $cell: tool_definitions failed"; fail=$((fail+1)); continue; }
        if [ "$RECORD" -eq 1 ]; then
            printf '%s\n' "$out" > "$FIXTURES/$cell.json"
            echo "recorded $cell ($(printf '%s' "$out" | jq 'length') tools)"
            continue
        fi
        if [ ! -f "$FIXTURES/$cell.json" ]; then
            echo "FAIL: $cell: no fixture (run with --record against the pre-refactor entrypoint)"; fail=$((fail+1)); continue
        fi
        if diff -u "$FIXTURES/$cell.json" <(printf '%s\n' "$out") >/dev/null; then
            pass=$((pass+1))
        else
            echo "FAIL: $cell differs from its fixture:"; diff -u "$FIXTURES/$cell.json" <(printf '%s\n' "$out") | head -40; fail=$((fail+1))
        fi
    done
done

if [ "$RECORD" -eq 1 ]; then echo "recorded $((${#ENVS[@]} * ${#ALLOWS[@]})) fixtures"; exit 0; fi

# The narrowing-only proof, stated as its own assertion so a fixture regenerated
# by mistake cannot quietly weaken it.
names="$(jq -r '[.[].function.name] | sort | join(",")' "$FIXTURES/all-file-read-only.json")"
if [ "$names" = "current_time,file_read,tool_result_read" ]; then
    pass=$((pass+1))
else
    echo "FAIL: all × file-read-only advertised more than file_read + current_time + the exempt tools: $names"; fail=$((fail+1))
fi

echo "tool_definitions golden: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
