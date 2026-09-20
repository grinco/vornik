#!/usr/bin/env bash
# test-entrypoint-step-prompt.sh — the step's first model input is written
# beside result.json, from the same files the request is built from, before
# the request is built (step-prompt persistence design §3, §9).
#
# Sources the entrypoint and drives write_step_prompt_file and
# build_llm_request_file directly, no agent boot.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$HERE/entrypoint.sh"
command -v jq >/dev/null 2>&1 || { echo "FAIL: jq is required" >&2; exit 1; }

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/out" "$tmp/ws"
export WORKSPACE="$tmp/ws" INPUT_FILE="$tmp/task.json" OUTPUT_FILE="$tmp/out/result.json"
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}},"projectId":"p","workflow":{"executionId":"e","stepId":"plan"}}' > "$INPUT_FILE"
set +u
# shellcheck disable=SC1090
source "$ep"
trap - EXIT
set +e

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }
# want <actual> <expected> <ok-msg> <bad-msg> — equality assertion.
# Written as if/else rather than `[ ... ] && ok || bad`, which reports a
# failure when the ok arm itself fails (SC2015).
want_eq() {
    if [ "$1" = "$2" ]; then ok "$3"; else bad "$4 (got '$1', want '$2')"; fi
}
want_ne() {
    if [ "$1" != "$2" ]; then ok "$3"; else bad "$4"; fi
}

# The two inputs build_llm_request_file reads: the messages file and the tools file.
msgs="$tmp/ws/.messages.json"; tools="$tmp/ws/.tools.json"
jq -n --arg sys "You are the planner. It's 'quoted' — and ünïcode." --arg usr "Plan the task: build a thing" \
   '[{"role":"system","content":$sys},{"role":"user","content":$usr}]' > "$msgs"
tool_definitions > "$tools"

# --- 1. Written BEFORE the request exists: nothing else has run yet. ---
[ -e "$tmp/out/step_prompt.json" ] && bad "prompt file exists before anything was written"
write_step_prompt_file "$msgs" "$tools"
if [ -s "$tmp/out/step_prompt.json" ] && [ ! -e "$tmp/ws/.request.json" ]; then
    ok "prompt file written before any request is built"
else
    bad "prompt file missing, or a request already existed"
fi

# --- 2. Structural: in the loop, the write precedes the build. ---
# The patterns match the entrypoint's SOURCE TEXT, so the `$name` in them is a
# literal to find, not a variable to expand — single quotes are deliberate.
# shellcheck disable=SC2016
w=$(grep -n '^        write_step_prompt_file "\$msgs_file"' "$ep" | head -1 | cut -d: -f1)
# shellcheck disable=SC2016
b=$(grep -n '^        build_llm_request_file "\$req_file" "\$msgs_file" "\$step_tools_file"' "$ep" | head -1 | cut -d: -f1)
if [ -n "$w" ] && [ -n "$b" ] && [ "$w" -lt "$b" ]; then
    ok "write_step_prompt_file precedes build_llm_request_file in the loop"
else
    bad "the loop does not write the prompt file before building the request (w=$w b=$b)"
fi

# --- 3. Parts equal the request's first iteration, byte for byte. ---
req="$tmp/ws/.request.json"
build_llm_request_file "$req" "$msgs" "$tools" "planner_result" "" "null" ""
f="$tmp/out/step_prompt.json"
want_eq "$(jq -r '.system.body' "$f")" "$(jq -r '.messages[0].content' "$req")" \
    "system part equals the request's system message" "system part differs from the request"
want_eq "$(jq -r '.user.body' "$f")" "$(jq -r '.messages[1].content' "$req")" \
    "user part equals the request's user message" "user part differs from the request"
want_eq "$(jq -r '.tools.body' "$f" | jq -S .)" "$(jq -S '.tools' "$req")" \
    "tools part equals the request's tools array" "tools part differs from the request"

# --- 4. Hashes are sha256 of the parts. ---
for part in system user tools; do
    body=$(jq -r ".$part.body" "$f"); want=$(printf '%s' "$body" | sha256sum | cut -d' ' -f1)
    want_eq "$(jq -r ".$part.sha256" "$f")" "$want" \
        "$part sha256 matches its body" "$part sha256 does not match its body"
done

# --- 5. Once per step: a later iteration must not overwrite the first request's record. ---
jq -n '[{"role":"system","content":"CHANGED"},{"role":"user","content":"CHANGED"}]' > "$msgs"
write_step_prompt_file "$msgs" "$tools"
want_ne "$(jq -r '.system.body' "$f")" "CHANGED" \
    "second call does not overwrite the first request's prompt" "later iteration overwrote the prompt file"

# --- 6. An unwritable output dir is a log line, never a failure. ---
STEP_PROMPT_WRITTEN=0 OUTPUT_FILE="/nonexistent/dir/result.json" write_step_prompt_file "$msgs" "$tools"; rc=$?
want_eq "$rc" "0" \
    "an unwritable output dir does not fail the step" "write_step_prompt_file failed on an unwritable dir"

# --- 7. A part at or above MAX_ARG_STRLEN is still persisted. ---
# Incident 2026-09-18: every part was passed to jq as a command-line ARGUMENT
# (--arg). Linux caps a SINGLE argument at MAX_ARG_STRLEN = 131072 bytes,
# independently of the much larger total ARG_MAX, so any part at or above that
# failed the jq call with E2BIG and the prompt was silently never recorded.
# Measured to the byte: 131071 persisted, 131072 was lost. It hit the roles
# whose prompts carry a document — 173 of 286 companion reviewer steps and 259
# of 501 ingestor steps had a prompt, the rest were this.
#
# The fixture is built with --rawfile precisely because --arg cannot carry a
# value this size: a fixture using --arg would fail before reaching the code
# under test and report a false pass.
big="$tmp/big.txt"
head -c 131072 /dev/zero | tr '\0' 'x' > "$big"
big_bytes=$(wc -c < "$big" | tr -d ' ')
want_eq "$big_bytes" "131072" "fixture is exactly MAX_ARG_STRLEN bytes" "fixture is the wrong size"
jq -n --rawfile u "$big" '[{"role":"system","content":"sys"},{"role":"user","content":$u}]' > "$msgs"
bigout="$tmp/bigout"; mkdir -p "$bigout"
STEP_PROMPT_WRITTEN=0 OUTPUT_FILE="$bigout/result.json" write_step_prompt_file "$msgs" "$tools"
if [ -s "$bigout/step_prompt.json" ]; then
    ok "a prompt part at MAX_ARG_STRLEN is persisted"
    # Round-trip: the stored body must be the bytes that were sent, or the
    # sha256 the daemon stores stops describing what the model was told.
    got_len=$(jq -r '.user.body' "$bigout/step_prompt.json" | head -c 200000 | wc -c | tr -d ' ')
    want_eq "$got_len" "$((big_bytes + 1))" \
        "the large part round-trips byte for byte (plus jq's trailing newline on read)" \
        "the large part was truncated or padded"
else
    bad "a prompt part at MAX_ARG_STRLEN was dropped (E2BIG from jq --arg)"
fi

echo "================================"; echo "PASSED: $pass"; echo "FAILED: $fail"
[ "$fail" -eq 0 ]
