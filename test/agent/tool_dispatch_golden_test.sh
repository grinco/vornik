#!/usr/bin/env bash
# tool_dispatch_golden_test.sh — what the eleven filesystem/git tools print, pinned.
#
# Design: https://docs.vornik.io §5.
# The fixtures under fixtures/tool_dispatch/ were RECORDED against the bash
# implementations of file_read, file_write, file_edit, read_many_files, grep,
# glob, git_status, git_diff, git_log and git_show in entrypoint.sh, before any
# of them moved to Go (commit named in the fixtures' README). This test builds
# the same deterministic workspace, calls exec_tool for every case, and
# compares stdout byte-for-byte after one normalisation: the temporary
# workspace path is replaced by $WS so outputs that echo a resolved path are
# stable across runs.
#
# current_time is deliberately not here (it is time); the Go unit test covers
# it with an injected clock.
#
# The four divergences the design names (D1 file_edit on invalid UTF-8, D2 the
# regex dialect, D3 ** glob semantics, D4 truncation by bytes) each have a
# case below whose fixture is re-recorded in the commit that introduces the
# divergence — and only then. A re-record that accompanies a refactor is the
# refactor changing behaviour.
#
# Usage: test/agent/tool_dispatch_golden_test.sh [--record]
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
FIXTURES="$HERE/fixtures/tool_dispatch"
RECORD=0
[ "${1:-}" = "--record" ] && RECORD=1

command -v jq >/dev/null 2>&1 || { echo "FAIL: jq is required" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "FAIL: git is required" >&2; exit 1; }
[ -f "$ENTRYPOINT" ] || { echo "FAIL: entrypoint not found at $ENTRYPOINT" >&2; exit 1; }

# The helper binary is on PATH in the image; outside it the Go test wrapper
# builds it into VORNIK_HELPER_DIR. When neither is present the bash cases
# still run (that is how the fixtures were recorded).
if [ -n "${VORNIK_HELPER_DIR:-}" ]; then export PATH="$VORNIK_HELPER_DIR:$PATH"; fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# WORKSPACE is a plain shell variable here, NOT exported — that is how the
# container sees it: the executor sets no WORKSPACE in the container env and
# the entrypoint assigns the default itself. The helper is a subprocess, so it
# only sees the variable if the entrypoint exports it. This harness exported it
# until 2026-09-05, which hid that the entrypoint did not: in production every
# helper-dispatched tool failed with "path escapes workspace" (easeit-companion
# ingest task_20260905101846, three attempts to the tool cap).
WORKSPACE="$TMP/ws"
OUTSIDE="$TMP/outside"
mkdir -p "$WORKSPACE" "$OUTSIDE" "$TMP/input"
printf '{"config":{"permissions":{"allowedTools":["file_read","file_write","run_shell","current_time","file_edit","read_many_files","grep","glob","git_status","git_diff","git_log","git_show","test_run","lint_run","typecheck_run"]}}}' > "$TMP/input/task.json"
export INPUT_FILE="$TMP/input/task.json" OUTPUT_FILE="$TMP/output.json"

# ---- the fixture workspace, deterministic to the byte and the mtime ----------
build_workspace() {
    local ws="$WORKSPACE"
    printf 'hello\nworld\n' > "$ws/notes.txt"
    head -c 40000 /dev/zero | tr '\0' 'a' > "$ws/big.bin"
    mkdir -p "$ws/sub/deep" "$ws/real/sub" "$ws/.tool_results" "$OUTSIDE/dir"
    printf 'needle one\nhay\nNeedle two\n' > "$ws/sub/one.txt"
    printf '# two\nneedle in markdown\n' > "$ws/sub/two.md"
    printf 'deep needle\n' > "$ws/sub/deep/three.txt"
    printf 'aa\nab\n' > "$ws/sub/back.txt"
    printf 'from the real dir\n' > "$ws/real/sub/file.txt"
    printf 'spilled\n' > "$ws/.tool_results/spill.txt"
    printf 'outside\n' > "$OUTSIDE/dir/x.txt"
    printf 'caf\xc3\xa9 and bad byte \xff here\n' > "$ws/utf8.txt"
    ln -s notes.txt "$ws/link.txt"
    ln -s real "$ws/linkdir"
    ln -s "$OUTSIDE/dir" "$ws/out"
    # Deterministic mtimes so glob's mtime-descending order is stable.
    local i=0
    for f in notes.txt big.bin sub/one.txt sub/two.md sub/deep/three.txt sub/back.txt real/sub/file.txt utf8.txt; do
        i=$((i+1))
        touch -d "2026-09-01T00:00:0${i}Z" "$ws/$f"
    done
    # A git repository with two commits, one unstaged edit and one staged edit;
    # fixed identity and dates make the hashes reproducible.
    export GIT_AUTHOR_NAME=Golden GIT_AUTHOR_EMAIL=golden@example.test GIT_COMMITTER_NAME=Golden GIT_COMMITTER_EMAIL=golden@example.test
    export GIT_AUTHOR_DATE="2026-09-01T10:00:00Z" GIT_COMMITTER_DATE="2026-09-01T10:00:00Z"
    local repo="$ws/project"
    mkdir -p "$repo"
    git -C "$repo" init -q -b main
    git -C "$repo" config commit.gpgsign false
    printf 'first\n' > "$repo/a.txt"
    printf 'unchanged\n' > "$repo/keep.txt"
    head -c 20000 /dev/zero | tr '\0' 'x' | sed 's/x/é/g' > "$repo/wide.txt"
    git -C "$repo" add . && git -C "$repo" commit -q -m "one"
    export GIT_AUTHOR_DATE="2026-09-01T11:00:00Z" GIT_COMMITTER_DATE="2026-09-01T11:00:00Z"
    printf 'first\nsecond\n' > "$repo/a.txt"
    git -C "$repo" add a.txt && git -C "$repo" commit -q -m "two: extend a.txt"
    printf 'first\nsecond\nthird (unstaged)\n' > "$repo/a.txt"
    printf 'staged\n' > "$repo/s.txt"
    git -C "$repo" add s.txt
    head -c 20000 /dev/zero | tr '\0' 'y' | sed 's/y/ü/g' > "$repo/wide.txt"
}
build_workspace

# ---- the cases: name | tool | arguments ---------------------------------------
CASES=(
  "file_read-basic|file_read|{\"path\":\"notes.txt\"}"
  "file_read-nested|file_read|{\"path\":\"sub/one.txt\"}"
  "file_read-missing|file_read|{\"path\":\"missing.txt\"}"
  "file_read-no-path|file_read|{}"
  "file_read-truncated|file_read|{\"path\":\"big.bin\"}"
  "file_read-tool-results-refused|file_read|{\"path\":\".tool_results/spill.txt\"}"
  "file_read-leaf-symlink|file_read|{\"path\":\"link.txt\"}"
  "file_read-through-dir-symlink|file_read|{\"path\":\"linkdir/sub/file.txt\"}"
  "file_read-escaping-symlink|file_read|{\"path\":\"out/x.txt\"}"
  "file_read-dotdot-escape|file_read|{\"path\":\"../outside/dir/x.txt\"}"
  "file_read-absolute-inside|file_read|{\"path\":\"$WORKSPACE/notes.txt\"}"
  "file_read-absolute-outside-rerooted|file_read|{\"path\":\"/notes.txt\"}"
  "file_write-new|file_write|{\"path\":\"written.txt\",\"content\":\"alpha\\nbeta\"}"
  "file_write-nested-dirs|file_write|{\"path\":\"made/up/dir/w.txt\",\"content\":\"x\"}"
  "file_write-no-content|file_write|{\"path\":\"w.txt\"}"
  "file_write-no-path|file_write|{\"content\":\"x\"}"
  "file_write-through-nonexistent-under-symlink|file_write|{\"path\":\"linkdir/new/file.txt\",\"content\":\"made\"}"
  "file_write-escape|file_write|{\"path\":\"../outside/w.txt\",\"content\":\"x\"}"
  "file_read-after-write|file_read|{\"path\":\"written.txt\"}"
  "file_edit-single|file_edit|{\"path\":\"notes.txt\",\"old_string\":\"hello\",\"new_string\":\"HELLO\"}"
  "file_edit-ambiguous|file_edit|{\"path\":\"sub/back.txt\",\"old_string\":\"a\",\"new_string\":\"z\"}"
  "file_edit-replace-all|file_edit|{\"path\":\"sub/back.txt\",\"old_string\":\"a\",\"new_string\":\"z\",\"replace_all\":true}"
  "file_edit-not-found|file_edit|{\"path\":\"notes.txt\",\"old_string\":\"nope\",\"new_string\":\"x\"}"
  "file_edit-missing-file|file_edit|{\"path\":\"missing.txt\",\"old_string\":\"x\",\"new_string\":\"y\"}"
  "file_edit-empty-old|file_edit|{\"path\":\"notes.txt\",\"old_string\":\"\",\"new_string\":\"y\"}"
  "file_edit-no-path|file_edit|{\"old_string\":\"x\",\"new_string\":\"y\"}"
  "file_edit-D1-invalid-utf8|file_edit|{\"path\":\"utf8.txt\",\"old_string\":\"and\",\"new_string\":\"AND\"}"
  "file_read-after-D1|file_read|{\"path\":\"utf8.txt\"}"
  "read_many-two|read_many_files|{\"paths\":[\"sub/one.txt\",\"sub/two.md\"]}"
  "read_many-missing-and-present|read_many_files|{\"paths\":[\"nope.txt\",\"sub/two.md\"]}"
  "read_many-empty|read_many_files|{\"paths\":[]}"
  "read_many-truncated|read_many_files|{\"paths\":[\"big.bin\",\"notes.txt\"]}"
  "read_many-escape|read_many_files|{\"paths\":[\"../outside/dir/x.txt\",\"notes.txt\"]}"
  "grep-files|grep|{\"pattern\":\"needle\"}"
  "grep-content|grep|{\"pattern\":\"needle\",\"output_mode\":\"content\"}"
  "grep-count|grep|{\"pattern\":\"needle\",\"output_mode\":\"count\"}"
  "grep-ignore-case|grep|{\"pattern\":\"needle\",\"ignore_case\":true,\"output_mode\":\"content\"}"
  "grep-glob-md|grep|{\"pattern\":\"needle\",\"glob\":\"*.md\"}"
  "grep-glob-doublestar|grep|{\"pattern\":\"needle\",\"glob\":\"**/*.txt\"}"
  "grep-head-limit|grep|{\"pattern\":\"needle\",\"output_mode\":\"content\",\"head_limit\":1}"
  "grep-path|grep|{\"pattern\":\"needle\",\"path\":\"sub/deep\"}"
  "grep-no-match|grep|{\"pattern\":\"zzz-not-here\"}"
  "grep-invalid-regex|grep|{\"pattern\":\"(\"}"
  "grep-no-pattern|grep|{}"
  "grep-escape-path|grep|{\"pattern\":\"x\",\"path\":\"../outside\"}"
  "grep-D2-backreference|grep|{\"pattern\":\"(a)\\\\1\",\"output_mode\":\"content\"}"
  "glob-txt|glob|{\"pattern\":\"*.txt\"}"
  "glob-D3-doublestar|glob|{\"pattern\":\"**/*.txt\"}"
  "glob-sub|glob|{\"pattern\":\"*\",\"path\":\"sub\"}"
  "glob-no-match|glob|{\"pattern\":\"*.nope\"}"
  "glob-no-pattern|glob|{}"
  "glob-escape-path|glob|{\"pattern\":\"*\",\"path\":\"../outside\"}"
  "git_status-basic|git_status|{}"
  "git_status-not-a-repo|git_status|{\"path\":\"sub\"}"
  "git_diff-unstaged|git_diff|{\"paths\":[\"a.txt\"]}"
  "git_diff-staged|git_diff|{\"staged\":true}"
  "git_diff-revision|git_diff|{\"revision\":\"HEAD~1\",\"paths\":[\"a.txt\"]}"
  "git_diff-D4-wide-truncated|git_diff|{\"paths\":[\"wide.txt\"]}"
  "git_log-default|git_log|{}"
  "git_log-max-1|git_log|{\"max\":1}"
  "git_log-paths|git_log|{\"paths\":[\"keep.txt\"]}"
  "git_log-bad-revision|git_log|{\"revision\":\"nope\"}"
  "git_show-head|git_show|{\"paths\":[\"a.txt\"]}"
  "git_show-revision|git_show|{\"revision\":\"HEAD~1\",\"paths\":[\"a.txt\"]}"
)

normalise() { sed -e "s#$WORKSPACE#\$WS#g" -e "s#$OUTSIDE#\$OUTSIDE#g"; }

run_case() {
    local tool="$1" args="$2"
    (
        set +u
        # shellcheck disable=SC1090
        source "$ENTRYPOINT"
        trap - EXIT
        set +e
        cd "$WORKSPACE" || exit 1
        exec_tool "$tool" "$args"
    ) 2>/dev/null | normalise
}

pass=0; fail=0
for spec in "${CASES[@]}"; do
    name="${spec%%|*}"; rest="${spec#*|}"; tool="${rest%%|*}"; args="${rest#*|}"
    out="$(run_case "$tool" "$args")"
    if [ "$RECORD" -eq 1 ]; then
        printf '%s\n' "$out" > "$FIXTURES/$name.out"
        echo "recorded $name"
        continue
    fi
    if [ ! -f "$FIXTURES/$name.out" ]; then
        echo "FAIL: $name: no fixture (run with --record against the bash implementation)"; fail=$((fail+1)); continue
    fi
    if diff -u "$FIXTURES/$name.out" <(printf '%s\n' "$out") >/dev/null; then
        pass=$((pass+1))
    else
        echo "FAIL: $name differs from its fixture:"; diff -u "$FIXTURES/$name.out" <(printf '%s\n' "$out") | head -30; fail=$((fail+1))
    fi
done

if [ "$RECORD" -eq 1 ]; then echo "recorded ${#CASES[@]} fixtures"; exit 0; fi
echo "tool_dispatch golden: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
