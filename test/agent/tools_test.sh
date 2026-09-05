#!/usr/bin/env bash
# Smoke tests for the built-in agent tools added in 2026.4.14.
# Sources images/vornik-agent/entrypoint.sh and invokes exec_tool() against
# a temp workspace. Reports pass/fail per case to stdout; exits non-zero on
# any failure.

set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
# The Go helper implements the filesystem/git tools; outside the image the Go
# test wrapper (or make) builds it into VORNIK_HELPER_DIR.
if [ -n "${VORNIK_HELPER_DIR:-}" ]; then export PATH="$VORNIK_HELPER_DIR:$PATH"; fi

if [ ! -f "$ENTRYPOINT" ]; then
    echo "FAIL: entrypoint.sh not found at $ENTRYPOINT" >&2
    exit 1
fi

PASS=0
FAIL=0
FAILURES=()

assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then
        PASS=$((PASS+1))
        echo "PASS: $name"
    else
        FAIL=$((FAIL+1))
        FAILURES+=("$name: expected to contain '$needle', got: $(printf '%q' "$haystack" | head -c 200)")
        echo "FAIL: $name"
    fi
}

assert_not_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" != *"$needle"* ]]; then
        PASS=$((PASS+1))
        echo "PASS: $name"
    else
        FAIL=$((FAIL+1))
        FAILURES+=("$name: unexpectedly contains '$needle'")
        echo "FAIL: $name"
    fi
}

ORIG_PATH="$PATH"

setup() {
    TMP="$(mktemp -d)"
    export PATH="$ORIG_PATH"
    mkdir -p "$TMP/workspace" "$TMP/workspace/project" "$TMP/input"
    # Allow every builtin tool so the gate doesn't mask bugs in handlers.
    printf '{"config":{"permissions":{"allowedTools":["file_read","file_write","run_shell","current_time","file_edit","read_many_files","grep","glob","git_status","git_diff","git_log","git_show","test_run","lint_run","typecheck_run"]}}}' > "$TMP/input/task.json"
    export WORKSPACE="$TMP/workspace"
    export INPUT_FILE="$TMP/input/task.json"
    export OUTPUT_FILE="$TMP/output.json"
    # Source in a subshell-safe way. entrypoint.sh's top-level `set -eu` and
    # EXIT trap need to be relaxed for testing.
    set +u
    # shellcheck disable=SC1090
    source "$ENTRYPOINT"
    trap - EXIT
    set -e +u  # keep -e off so assertions don't abort
    set +e
}

teardown() {
    rm -rf "$TMP"
    unset WORKSPACE INPUT_FILE OUTPUT_FILE TMP
    export PATH="$ORIG_PATH"
}

# ---------- file_edit ----------
setup
printf 'line one\nline two\nline three\nline two\n' > "$TMP/workspace/sample.txt"

out=$(exec_tool file_edit '{"path":"sample.txt","old_string":"line one","new_string":"line ONE"}')
assert_contains "file_edit: single match replaces" "$out" "OK: replaced 1"
assert_contains "file_edit: file content updated" "$(cat "$TMP/workspace/sample.txt")" "line ONE"

out=$(exec_tool file_edit '{"path":"sample.txt","old_string":"line two","new_string":"line TWO"}')
assert_contains "file_edit: ambiguous match errors" "$out" "matches 2 times"

out=$(exec_tool file_edit '{"path":"sample.txt","old_string":"line two","new_string":"line TWO","replace_all":true}')
assert_contains "file_edit: replace_all succeeds" "$out" "OK: replaced 2"

out=$(exec_tool file_edit '{"path":"sample.txt","old_string":"nonexistent","new_string":"x"}')
assert_contains "file_edit: not-found errors" "$out" "not found in file"

out=$(exec_tool file_edit '{"path":"missing.txt","old_string":"x","new_string":"y"}')
assert_contains "file_edit: missing file errors" "$out" "file not found"

out=$(exec_tool file_edit '{"old_string":"x","new_string":"y"}')
assert_contains "file_edit: missing path errors" "$out" "path is required"
teardown

# ---------- current_time ----------
setup
out=$(exec_tool current_time '{"timezone":"UTC"}')
assert_contains "current_time: returns timezone" "$out" '"timezone": "UTC"'
assert_contains "current_time: returns date field" "$out" '"date":'
assert_contains "current_time: returns utc field" "$out" '"utc":'
assert_contains "current_time: returns offset" "$out" '"utc_offset": "+00:00"'

out=$(exec_tool current_time '{"timezone":"Europe/Prague"}')
assert_contains "current_time: handles Europe/Prague" "$out" '"timezone": "Europe/Prague"'
assert_contains "current_time: returns weekday" "$out" '"weekday":'

out=$(exec_tool current_time '{"timezone":"Not/AZone"}')
assert_contains "current_time: invalid timezone errors" "$out" "invalid timezone"

printf '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$TMP/input/task.json"
out=$(exec_tool current_time '{"timezone":"UTC"}')
assert_contains "current_time: available with restrictive allowlist" "$out" '"timezone": "UTC"'
teardown

# ---------- read_many_files ----------
setup
printf 'alpha\n' > "$TMP/workspace/a.txt"
printf 'beta\n' > "$TMP/workspace/b.txt"
out=$(exec_tool read_many_files '{"paths":["a.txt","b.txt"]}')
assert_contains "read_many_files: emits a.txt header" "$out" "===== FILE: a.txt ====="
assert_contains "read_many_files: emits b.txt header" "$out" "===== FILE: b.txt ====="
assert_contains "read_many_files: emits a.txt content" "$out" "alpha"
assert_contains "read_many_files: emits b.txt content" "$out" "beta"

out=$(exec_tool read_many_files '{"paths":["nope.txt"]}')
assert_contains "read_many_files: missing file reported" "$out" "ERROR: file not found"

out=$(exec_tool read_many_files '{"paths":[]}')
assert_contains "read_many_files: empty paths errors" "$out" "paths array is required"
teardown

# ---------- grep ----------
setup
mkdir -p "$TMP/workspace/src"
printf 'hello world\ngoodbye world\n' > "$TMP/workspace/src/a.txt"
printf 'other file\n' > "$TMP/workspace/src/b.txt"

out=$(exec_tool grep '{"pattern":"world"}')
assert_contains "grep: files_with_matches finds a.txt" "$out" "src/a.txt"
assert_not_contains "grep: files_with_matches excludes b.txt" "$out" "src/b.txt"

out=$(exec_tool grep '{"pattern":"world","output_mode":"content"}')
assert_contains "grep: content mode emits line numbers" "$out" "src/a.txt:1:hello world"

out=$(exec_tool grep '{"pattern":"WORLD","ignore_case":true}')
assert_contains "grep: case-insensitive matches" "$out" "src/a.txt"

out=$(exec_tool grep '{"pattern":"nothingmatchesthis"}')
assert_contains "grep: no matches reported" "$out" "(no matches)"

out=$(exec_tool grep '{}')
assert_contains "grep: missing pattern errors" "$out" "pattern is required"
teardown

# ---------- glob ----------
setup
mkdir -p "$TMP/workspace/nested/deep"
printf 'x' > "$TMP/workspace/top.go"
printf 'x' > "$TMP/workspace/nested/mid.go"
printf 'x' > "$TMP/workspace/nested/deep/leaf.go"
printf 'x' > "$TMP/workspace/other.py"

out=$(exec_tool glob '{"pattern":"*.go"}')
assert_contains "glob: non-recursive matches top-level" "$out" "top.go"
assert_not_contains "glob: non-recursive excludes nested" "$out" "nested/mid.go"

out=$(exec_tool glob '{"pattern":"**/*.go"}')
assert_contains "glob: recursive matches nested" "$out" "nested/mid.go"
assert_contains "glob: recursive matches deeply nested" "$out" "nested/deep/leaf.go"

out=$(exec_tool glob '{"pattern":"*.rs"}')
assert_contains "glob: no matches reported" "$out" "(no matches)"
teardown

# ---------- git_status / git_log / git_diff / git_show ----------
setup
(cd "$TMP/workspace/project" && \
    git init -q && \
    git config user.email "t@t" && git config user.name "t" && \
    printf 'package main\n' > main.go && \
    printf 'module x\ngo 1.25\n' > go.mod && \
    git add -A && git commit -q -m "initial" && \
    printf 'package main\n\n// change\n' > main.go)

out=$(exec_tool git_status '{}')
assert_contains "git_status: reports branch" "$out" '"branch":'
assert_contains "git_status: lists modified main.go" "$out" "main.go"

out=$(exec_tool git_log '{"max":5}')
assert_contains "git_log: returns JSON array" "$out" '"subject": "initial"'
assert_contains "git_log: includes short_sha" "$out" '"short_sha":'

out=$(exec_tool git_diff '{}')
assert_contains "git_diff: emits unified diff header" "$out" "+++ b/main.go"
assert_contains "git_diff: shows change line" "$out" "+// change"

out=$(exec_tool git_show '{"revision":"HEAD"}')
assert_contains "git_show: shows initial commit subject" "$out" "initial"

out=$(exec_tool git_status '{"path":"does_not_exist"}')
assert_contains "git_status: missing path errors cleanly" "$out" "ERROR"
teardown

# ---------- test_run / lint_run / typecheck_run (graceful degradation) ----------
setup
# No language markers — expect "could not detect language" error
out=$(exec_tool test_run '{"path":"."}')
assert_contains "test_run: detects missing language" "$out" "could not detect language"

# Set up a minimal Go project, which the agent image has the toolchain for
# on the CI host (not in the production agent container, but for this test
# we use the host's toolchain via the sourced script).
mkdir -p "$TMP/workspace/project"
(cd "$TMP/workspace/project" && \
    printf 'package main\n\nfunc main() {}\n' > main.go && \
    printf 'module testproj\ngo 1.25\n' > go.mod)
out=$(exec_tool test_run '{}')
# May or may not have go toolchain in CI — accept either success or
# a graceful "not available" marker. The key assertion is that the JSON
# parses and has a "language" field.
assert_contains "test_run: reports language=go" "$out" '"language": "go"'

out=$(exec_tool lint_run '{}')
assert_contains "lint_run: reports language=go" "$out" '"language": "go"'

out=$(exec_tool typecheck_run '{}')
assert_contains "typecheck_run: reports language=go" "$out" '"language": "go"'
teardown

# ---------- backlog_deposit: allowedTools gating ----------
setup
# Default setup() task.json omits backlog_deposit from allowedTools.
out=$(tool_definitions)
assert_not_contains "backlog_deposit: not advertised when absent from allowedTools" "$out" '"backlog_deposit"'
teardown

setup
printf '{"config":{"permissions":{"allowedTools":["backlog_deposit"]}}}' > "$TMP/input/task.json"
out=$(tool_definitions)
assert_contains "backlog_deposit: advertised when present in allowedTools" "$out" '"backlog_deposit"'
teardown

setup
printf '{"config":{"permissions":{"allowedTools":["backlog_deposit"]}}}' > "$TMP/input/task.json"
out=$(exec_tool backlog_deposit '{"kind":"bug","title":"x","detail":"y"}')
assert_not_contains "backlog_deposit: not gate-blocked when present in allowedTools" "$out" "not allowed for this role"
teardown

setup
printf '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$TMP/input/task.json"
out=$(exec_tool backlog_deposit '{"kind":"bug","title":"x","detail":"y"}')
assert_contains "backlog_deposit: exec_tool blocked when absent from allowedTools" "$out" "not allowed for this role"
teardown

# ---------- backlog_deposit: handler POST + response passthrough ----------
mock_curl_setup() {
    MOCK_BIN="$TMP/mockbin"
    mkdir -p "$MOCK_BIN"
    CURL_ARGS_FILE="$TMP/curl_args"
    CURL_RESPONSE_FILE="$TMP/curl_response"
    CURL_EXIT_FILE="$TMP/curl_exit"
    printf '{"status":"accepted"}' > "$CURL_RESPONSE_FILE"
    printf '0' > "$CURL_EXIT_FILE"
    cat > "$MOCK_BIN/curl" <<CURL_EOF
#!/usr/bin/env bash
printf '%s\n' "\$@" > "$CURL_ARGS_FILE"
cat "$CURL_RESPONSE_FILE"
exit "\$(cat "$CURL_EXIT_FILE")"
CURL_EOF
    chmod +x "$MOCK_BIN/curl"
    export PATH="$MOCK_BIN:$PATH"
    export CURL_ARGS_FILE CURL_RESPONSE_FILE CURL_EXIT_FILE
}

setup
printf '{"projectId":"proj-1","config":{"permissions":{"allowedTools":["backlog_deposit"]}}}' > "$TMP/input/task.json"
mock_curl_setup
export VORNIK_API_URL="http://daemon.local"
export VORNIK_API_KEY="secret-key"
export VORNIK_TASK_ID="task-1"
export VORNIK_EXECUTION_ID="exec-1"
STEP_ID="step-1"

out=$(exec_tool backlog_deposit '{"kind":"bug","title":"parseConfig crashes on empty file","detail":"Empty config.yaml causes a nil pointer panic.","evidence":"config.go:42"}')
args="$(cat "$CURL_ARGS_FILE")"
assert_contains "backlog_deposit: POSTs to internal endpoint" "$args" "http://daemon.local/api/v1/internal/backlog-deposit"
assert_contains "backlog_deposit: sends X-API-Key header" "$args" "X-API-Key: secret-key"
assert_contains "backlog_deposit: body has project_id" "$args" '"project_id": "proj-1"'
assert_contains "backlog_deposit: body has task_id" "$args" '"task_id": "task-1"'
assert_contains "backlog_deposit: body has execution_id" "$args" '"execution_id": "exec-1"'
assert_contains "backlog_deposit: body has step_id" "$args" '"step_id": "step-1"'
assert_contains "backlog_deposit: body has kind" "$args" '"kind": "bug"'
assert_contains "backlog_deposit: body has title" "$args" '"title": "parseConfig crashes on empty file"'
assert_contains "backlog_deposit: response returned verbatim" "$out" '{"status":"accepted"}'
unset VORNIK_API_URL VORNIK_API_KEY VORNIK_TASK_ID VORNIK_EXECUTION_ID
teardown

setup
printf '{"projectId":"proj-1","config":{"permissions":{"allowedTools":["backlog_deposit"]}}}' > "$TMP/input/task.json"
mock_curl_setup
printf '{"status":"rejected","reason":"duplicate of an open item"}' > "$CURL_RESPONSE_FILE"
export VORNIK_API_URL="http://daemon.local"
export VORNIK_API_KEY="secret-key"
export VORNIK_TASK_ID="task-1"
export VORNIK_EXECUTION_ID="exec-1"

out=$(exec_tool backlog_deposit '{"kind":"bug","title":"dup finding","detail":"already reported"}')
assert_contains "backlog_deposit: daemon rejection returned verbatim" "$out" '{"status":"rejected","reason":"duplicate of an open item"}'
unset VORNIK_API_URL VORNIK_API_KEY VORNIK_TASK_ID VORNIK_EXECUTION_ID
teardown

setup
printf '{"projectId":"proj-1","config":{"permissions":{"allowedTools":["backlog_deposit"]}}}' > "$TMP/input/task.json"
mock_curl_setup
printf '1' > "$CURL_EXIT_FILE"
printf '' > "$CURL_RESPONSE_FILE"
export VORNIK_API_URL="http://daemon.local"
export VORNIK_API_KEY="secret-key"
export VORNIK_TASK_ID="task-1"
export VORNIK_EXECUTION_ID="exec-1"

out=$(exec_tool backlog_deposit '{"kind":"bug","title":"x","detail":"y"}')
assert_contains "backlog_deposit: curl failure returns error JSON" "$out" '{"error":"request failed"}'
unset VORNIK_API_URL VORNIK_API_KEY VORNIK_TASK_ID VORNIK_EXECUTION_ID
teardown

# ---------- GH_TOKEN git credential wiring ----------
setup
STUB_BIN="$TMP/stubbin"
mkdir -p "$STUB_BIN"
GH_CALLS_FILE="$TMP/gh_calls"
: > "$GH_CALLS_FILE"
cat > "$STUB_BIN/gh" <<GH_EOF
#!/usr/bin/env bash
echo "\$@" >> "$GH_CALLS_FILE"
exit 0
GH_EOF
chmod +x "$STUB_BIN/gh"
export PATH="$STUB_BIN:$PATH"
export GH_TOKEN="ghp_faketoken"

setup_gh_git_credentials
call_count=$(grep -c "^auth setup-git$" "$GH_CALLS_FILE" || true)
if [ "$call_count" = "1" ]; then
    PASS=$((PASS+1)); echo "PASS: GH_TOKEN + gh present: setup-git invoked exactly once"
else
    FAIL=$((FAIL+1)); FAILURES+=("GH_TOKEN + gh present: expected exactly 1 call, got $call_count"); echo "FAIL: GH_TOKEN + gh present: setup-git invoked exactly once"
fi
unset GH_TOKEN
teardown

setup
export GH_TOKEN="ghp_faketoken"
# Restrict PATH to exclude any `gh` binary (e.g. linuxbrew on dev hosts)
# while keeping the core utils entrypoint.sh needs (bash, jq, curl, python3).
export PATH="/usr/bin:/bin"
if command -v gh >/dev/null 2>&1; then
    echo "SKIP: gh unexpectedly on restricted PATH ($(command -v gh)); skipping no-gh case"
else
    out=$(setup_gh_git_credentials)
    assert_contains "GH_TOKEN without gh: warning logged" "$out" "GH_TOKEN set but gh missing"
fi
unset GH_TOKEN
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
